#!/usr/bin/env python3
"""Build npm packages from verified native release assets."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

import licenses

import package_release as release

ROOT = Path(__file__).resolve().parent.parent
NPM_ROOT = "@brokkai/feature-bot"


def npm_build_environment(directory):
    # Packaging/offline tests must not inherit developer account settings.
    env = {k: v for k, v in os.environ.items() if not k.lower().startswith("npm_config_")}
    return dict(env, npm_config_userconfig=str(directory / "user.npmrc"),
                npm_config_globalconfig=str(directory / "global.npmrc"),
                npm_config_cache=str(directory / "npm-cache"))


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def pack_record(output, name, version):
    records = json.loads(output)
    # npm 12 keys pack output by package name; npm 11 returns an array.
    if isinstance(records, dict) and set(records) == {name}:
        records = [records[name]]
    if not isinstance(records, list) or len(records) != 1:
        raise ValueError("npm pack must report exactly one package")
    info = records[0]
    if (not isinstance(info, dict) or info.get("name") != name or info.get("version") != version
            or not info.get("integrity") or not info.get("filename")
            or Path(info["filename"]).name != info["filename"]):
        raise ValueError("npm pack metadata does not match the requested package")
    return info


def package(tag, assets, output, sha):
    release.validate_tag(tag)
    npm_version = tag[1:]
    release.verify_local(tag, assets, sha)
    if output.exists() and any(output.iterdir()):
        raise ValueError("installer output directory must be empty")
    output.mkdir(parents=True, exist_ok=True)
    npm_output = output / "npm"
    npm_output.mkdir()
    packages = []
    base = {
        "version": npm_version, "license": "Apache-2.0",
        "repository": {"type": "git", "url": "git+https://github.com/BrokkAi/feature-bot.git"},
        "publishConfig": {"access": "public"},
    }
    with tempfile.TemporaryDirectory() as temporary:
        staging = Path(temporary)

        def npm_pack(name, fields, files):
            directory = staging / name.split("/")[-1]
            directory.mkdir()
            write_json(directory / "package.json", dict(base, name=name, **fields))
            for filename, data in files.items():
                path = directory / filename
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(data)
                path.chmod(0o755 if filename.startswith("bin/") else 0o644)
            result = subprocess.check_output([
                "npm", "pack", "--ignore-scripts", "--json", "--pack-destination", str(npm_output.resolve()),
            ], cwd=directory, env=npm_build_environment(staging))
            info = pack_record(result, name, npm_version)
            tarball = npm_output / info["filename"]
            licenses.check_npm(tarball)
            packages.append({"name": name, "version": npm_version, "filename": tarball.name,
                             "sha256": release.digest(tarball.read_bytes()), "integrity": info["integrity"]})

        dependencies = {}
        for target in release.TARGETS:
            name = release.archive_name(tag, target)
            with tarfile.open(assets / name, "r:gz") as bundle:
                files = {member: bundle.extractfile(member).read() for member in ("bfb", "README.md", "BUILD.json", *licenses.LEGAL_FILES)}
            system, go_arch = target.split("-")
            arch = {"amd64": "x64", "arm64": "arm64"}[go_arch]
            package_name = f"{NPM_ROOT}-{system}-{arch}"
            dependencies[package_name] = npm_version
            npm_pack(package_name, {"os": [system], "cpu": [arch], "description": f"Brokk Feature Bot native binary for {system}/{arch}"},
                     {"bin/bfb": files["bfb"], **{name: files[name] for name in licenses.LEGAL_FILES}, "README.md": files["README.md"], "BUILD.json": files["BUILD.json"]})
        npm_pack(NPM_ROOT, {
            "description": "Brokk Feature Bot: autonomous feature discovery and duplicate-aware GitHub issue proposals",
            "bin": {"bfb": "bin/bfb.cjs"}, "engines": {"node": ">=18"},
            "os": ["linux", "darwin"], "cpu": ["x64", "arm64"], "optionalDependencies": dependencies,
        }, {"bin/bfb.cjs": (ROOT / "npm/bfb.cjs").read_bytes(), **{name: files[name] for name in licenses.LEGAL_FILES}, "README.md": files["README.md"]})
        write_json(npm_output / "manifest.json", {"tag": tag, "commit": sha, "packages": packages})

    print(f"Built {len(packages)} npm packages for {tag} at {sha}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("assets", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    package(args.tag, args.assets, args.output, release.commit())


if __name__ == "__main__":
    main()
