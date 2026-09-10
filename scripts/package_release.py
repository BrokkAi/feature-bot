#!/usr/bin/env python3
"""Build and verify native release archives using Python and Go."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

import licenses


ROOT = Path(__file__).resolve().parent.parent
TARGETS = ("linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64")
TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?")


def run(*args, data=None, env=None):
    return subprocess.run(args, input=data, env=env, check=True, stdout=subprocess.PIPE, cwd=ROOT).stdout


def commit():
    return run("git", "rev-parse", "HEAD").decode().strip()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def validate_tag(tag):
    if not TAG.fullmatch(tag):
        raise ValueError("tag must be a version such as v0.1.0 or v0.1.0-rc.1")


def archive_name(tag, target):
    return f"brokk-feature-bot-{tag}-{target}.tar.gz"


def archive(path, files, timestamp):
    # Fixed tar metadata; gzip bytes can still differ across compressor versions.
    with path.open("wb") as output, gzip.GzipFile(filename="", fileobj=output, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as tar:
            for name, data in sorted(files.items()):
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                entry.mode = 0o755 if name == "bfb" else 0o644
                entry.mtime = timestamp
                tar.addfile(entry, io.BytesIO(data))


def package(tag, directory):
    validate_tag(tag)
    if run("git", "status", "--porcelain").strip():
        raise ValueError("commit all preparation changes before packaging a release")
    licenses.check()
    directory.mkdir(parents=True, exist_ok=True)
    if any(directory.iterdir()):
        raise ValueError("package output directory must be empty")
    sha = commit()
    timestamp = int(run("git", "show", "-s", "--format=%ct", sha))
    manifest = {"tag": tag, "commit": sha, "assets": []}
    with tempfile.TemporaryDirectory() as temp:
        binary = Path(temp) / "bfb"
        for target in TARGETS:
            goos, goarch = target.split("-")
            env = dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=goarch,
                       GOWORK="off", GOFLAGS="-mod=readonly")
            run("go", "build", "-trimpath", "-buildvcs=false", f"-ldflags=-s -w -X main.version={tag}", "-o", str(binary), "./cmd/bfb", env=env)
            metadata = {"tag": tag, "commit": sha, "target": target}
            name = archive_name(tag, target)
            archive(directory / name, {
                "bfb": binary.read_bytes(),
                **licenses.legal_files(),
                "README.md": (ROOT / "README.md").read_bytes(),
                "BUILD.json": json.dumps(metadata, sort_keys=True).encode() + b"\n",
            }, timestamp)
            data = (directory / name).read_bytes()
            manifest["assets"].append({"name": name, "size": len(data), "sha256": digest(data)})
    (directory / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (directory / "checksums.txt").write_text("".join(f"{a['sha256']}  {a['name']}\n" for a in manifest["assets"]))
    if run("git", "status", "--porcelain").strip() or commit() != sha:
        raise ValueError("checkout changed during packaging; discard these assets and rebuild")
    verify_local(tag, directory, sha)
    print(f"Built and validated {len(TARGETS)} archives at {sha}")


def verify_local(tag, directory, sha):
    validate_tag(tag)
    manifest = json.loads((directory / "release.json").read_text())
    if manifest["tag"] != tag or manifest["commit"] != sha:
        raise ValueError("assets do not belong to the requested tag and exact checkout commit")
    expected = {archive_name(tag, target) for target in TARGETS}
    names = [a["name"] for a in manifest["assets"]]
    if len(names) != len(expected) or set(names) != expected:
        raise ValueError("manifest must include every supported platform exactly once")
    if {p.name for p in directory.iterdir()} != expected | {"release.json", "checksums.txt"}:
        raise ValueError("release directory contains missing or unexpected assets")
    for asset in manifest["assets"]:
        path = directory / asset["name"]
        data = path.read_bytes()
        if not data or len(data) != asset["size"] or digest(data) != asset["sha256"]:
            raise ValueError(f"corrupt release asset: {path.name}")
        with tarfile.open(path, "r:gz") as tar:
            entries = tar.getmembers()
            expected_entries = {"bfb", "README.md", "BUILD.json", *licenses.LEGAL_FILES}
            if len(entries) != len(expected_entries) or {m.name for m in entries} != expected_entries:
                raise ValueError("archive has missing or unexpected contents")
            if any(not m.isfile() or m.size <= 0 for m in entries):
                raise ValueError("archive contains an invalid entry")
            if tar.getmember("bfb").mode & 0o111 == 0:
                raise ValueError("release binary is not executable")
            for filename, expected_text in licenses.legal_files().items():
                if tar.extractfile(filename).read() != expected_text:
                    raise ValueError(f"archive legal file does not match the checkout: {filename}")
            metadata = json.load(tar.extractfile("BUILD.json"))
            target = next(t for t in TARGETS if archive_name(tag, t) == path.name)
            if metadata != {"tag": tag, "commit": sha, "target": target}:
                raise ValueError("archive build metadata does not match the release")
    checksums = "".join(f"{a['sha256']}  {a['name']}\n" for a in manifest["assets"])
    if (directory / "checksums.txt").read_text() != checksums:
        raise ValueError("checksum list does not match all release assets")
    return manifest


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    package(args.version, args.output.resolve())
