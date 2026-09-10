import json
import os
from pathlib import Path
import unittest
from unittest.mock import patch

import package_installers


class PackageMetadata(unittest.TestCase):
    def setUp(self):
        self.info = {"name": "@brokkai/feature-bot", "version": "0.1.0-rc.1",
                     "filename": "package.tgz", "integrity": "sha512-example"}

    def test_accepts_npm_11_and_12_pack_metadata(self):
        for output in ([self.info], {self.info["name"]: self.info}):
            self.assertEqual(package_installers.pack_record(
                json.dumps(output), self.info["name"], self.info["version"]), self.info)

    def test_rejects_ambiguous_and_mismatched_pack_metadata(self):
        for output in ([], [self.info, self.info], {"other": self.info}, [None],
                       [dict(self.info, version="0.2.0")],
                       [dict(self.info, filename="../escape.tgz")]):
            with self.subTest(output=output), self.assertRaises(ValueError):
                package_installers.pack_record(json.dumps(output), self.info["name"], self.info["version"])

    def test_builds_isolate_npm_settings_without_changing_process_environment(self):
        original = {"PATH": "/tools", "NPM_CONFIG_GLOBAL": "true", "npm_config_registry": "https://fixture.invalid",
                    "Npm_Config_UserConfig": "/account/.npmrc"}
        with patch.dict(os.environ, original, clear=True):
            environment = package_installers.npm_build_environment(Path("/staging"))
            self.assertEqual(dict(os.environ), original)
        self.assertEqual(environment, {
            "PATH": "/tools", "npm_config_userconfig": "/staging/user.npmrc",
            "npm_config_globalconfig": "/staging/global.npmrc", "npm_config_cache": "/staging/npm-cache",
        })
