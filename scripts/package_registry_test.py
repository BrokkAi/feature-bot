import base64
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import patch

import package_installers
import package_registry
import package_release as release


class PackageRegistry(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "npm").mkdir()
        self.packages = []
        names = [package_installers.NPM_ROOT] + [
            f"{package_installers.NPM_ROOT}-{system}-{arch}"
            for system in ("linux", "darwin") for arch in ("x64", "arm64")
        ]
        for index, name in enumerate(names):
            filename = f"package-{index}.tgz"
            data = name.encode()
            (self.root / "npm" / filename).write_bytes(data)
            self.packages.append({"name": name, "version": "0.1.0", "filename": filename,
                                  "sha256": release.digest(data), "integrity": "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()})
        (self.root / "npm/manifest.json").write_text(json.dumps({"tag": "v0.1.0", "commit": "a" * 40, "packages": self.packages}))

    def test_read_only_check_never_publishes(self):
        with patch.object(package_registry, "npm_exists", return_value=False), \
                patch.object(package_registry.subprocess, "run") as command:
            package_registry.run("check", self.root)
            command.assert_not_called()
            with self.assertRaisesRegex(ValueError, "incomplete"):
                package_registry.run("verify", self.root)

    def test_conflicts_in_later_destination_prevent_all_writes(self):
        with patch.object(package_registry, "npm_exists", side_effect=[False, False, False, False, ValueError("conflicting npm bytes")]), \
                patch.object(package_registry.subprocess, "run") as command:
            with self.assertRaisesRegex(ValueError, "conflicting"):
                package_registry.run("publish", self.root)
            command.assert_not_called()

    def test_native_packages_publish_before_root(self):
        with patch.object(package_registry, "npm_exists", return_value=False), \
                patch.object(package_registry.subprocess, "run") as command:
            package_registry.run("publish", self.root)
            calls = [call.args[0] for call in command.call_args_list]
            self.assertEqual(len(calls), 5)
            self.assertTrue(all(call[:2] == ["npm", "publish"] for call in calls[:5]))
            self.assertTrue(calls[4][2].endswith("package-0.tgz"))

    def test_identical_existing_packages_are_verified_without_upload(self):
        with patch.object(package_registry, "npm_exists", return_value=True), \
                patch.object(package_registry.subprocess, "run") as command:
            package_registry.run("publish", self.root)
            package_registry.run("verify", self.root)
            command.assert_not_called()

    def test_prerelease_packages_use_next_dist_tag(self):
        manifest_path = self.root / "npm/manifest.json"
        manifest = json.loads(manifest_path.read_text())
        manifest["tag"] = "v0.1.0-rc.1"
        for package in manifest["packages"]:
            package["version"] = "0.1.0-rc.1"
        manifest_path.write_text(json.dumps(manifest))
        with patch.object(package_registry, "npm_exists", return_value=False), \
                patch.object(package_registry.subprocess, "run") as command:
            package_registry.run("publish", self.root)
            self.assertEqual(command.call_count, 5)
            self.assertTrue(all(call.args[0][-2:] == ["--tag", "next"] for call in command.call_args_list))

    def test_changed_local_tarball_prevents_remote_reads(self):
        (self.root / "npm" / self.packages[0]["filename"]).write_bytes(b"corrupted")
        with patch.object(package_registry, "fetch_json") as fetch:
            with self.assertRaisesRegex(ValueError, "corrupt staged"):
                package_registry.run("publish", self.root)
            fetch.assert_not_called()

    def test_npm_integrity_conflict(self):
        record = dict(self.packages[0], dist={"integrity": "sha512-different"})
        with patch.object(package_registry, "fetch_json", return_value=record):
            with self.assertRaisesRegex(ValueError, "differs"):
                package_registry.npm_exists(self.packages[0])

    def test_only_404_means_version_is_missing(self):
        for code in (404, 403, 500):
            with patch.object(package_registry.urllib.request, "urlopen", side_effect=urllib.error.HTTPError("https://registry.test", code, "error", {}, None)):
                if code == 404:
                    self.assertIsNone(package_registry.fetch_json("https://registry.test"))
                else:
                    with self.assertRaises(urllib.error.HTTPError):
                        package_registry.fetch_json("https://registry.test")

    def test_accepted_uploads_do_not_wait_for_public_indexes(self):
        with patch.object(package_registry, "npm_exists", return_value=False) as exists, \
                patch.object(package_registry.subprocess, "run") as command:
            package_registry.run("publish", self.root)
            self.assertEqual(command.call_count, 5)
            self.assertEqual(exists.call_count, 5)  # Only the pre-upload conflict check.

    def test_upload_failure_stops_before_publishing_the_launcher(self):
        with patch.object(package_registry, "npm_exists", return_value=False), \
                patch.object(package_registry.subprocess, "run", side_effect=package_registry.subprocess.CalledProcessError(1, "npm")) as command:
            with self.assertRaises(package_registry.subprocess.CalledProcessError):
                package_registry.run("publish", self.root)
            self.assertEqual(command.call_count, 1)
