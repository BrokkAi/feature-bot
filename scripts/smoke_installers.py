#!/usr/bin/env python3
"""Build and exercise a real offline npm install without publishing."""

import argparse
import json
from pathlib import Path
import platform
import subprocess
import tempfile

import package_installers
import package_release as release


def smoke(packages):
    system = {"Linux": "linux", "Darwin": "darwin"}[platform.system()]
    arch = {"x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
    with tempfile.TemporaryDirectory() as temporary:
        temporary = Path(temporary)
        env = package_installers.npm_build_environment(temporary)
        npm_arch = "x64" if arch == "amd64" else "arm64"
        manifest = json.loads((packages / "npm/manifest.json").read_text())
        selected = [p for p in manifest["packages"] if p["name"] in
                    (package_installers.NPM_ROOT, f"{package_installers.NPM_ROOT}-{system}-{npm_arch}")]
        subprocess.run(["npm", "install", "--offline", "--ignore-scripts", "--no-audit", "--no-fund", "--prefix", str(temporary / "npm"),
                        *[str((packages / "npm" / p["filename"]).resolve()) for p in selected]], check=True, env=env)
        subprocess.run([str(temporary / "npm/node_modules/.bin/bfb"), "--help"], check=True, env=env)
        print("Local npm install launched bfb successfully")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", default="v0.0.0")
    parser.add_argument("--assets", type=Path)
    parser.add_argument("--packages", type=Path)
    args = parser.parse_args()
    if bool(args.assets) != bool(args.packages):
        parser.error("--assets and --packages must be supplied together")
    if args.packages:
        smoke(args.packages)
        return
    with tempfile.TemporaryDirectory() as temporary:
        assets, packages = Path(temporary) / "assets", Path(temporary) / "packages"
        release.package(args.tag, assets)
        package_installers.package(args.tag, assets, packages, release.commit())
        smoke(packages)


if __name__ == "__main__":
    main()
