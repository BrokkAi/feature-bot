import gzip
from datetime import datetime, timezone, timedelta
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import release_preflight as gate


def archive(content=b'binary', mode=0o755, mtime=0):
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode='w') as tar:
        entry = tarfile.TarInfo('package/bin/bfb')
        entry.mode, entry.size = mode, len(content)
        tar.addfile(entry, io.BytesIO(content))
    return gzip.compress(raw.getvalue(), mtime=mtime)


class ReleasePreflight(unittest.TestCase):
    def test_npm_exchange_accepts_numeric_unix_expiry(self):
        expiry = datetime.now(timezone.utc) + timedelta(hours=1)
        self.assertEqual(gate.exchange_expiry(int(expiry.timestamp())).timestamp(), int(expiry.timestamp()))
        with self.assertRaisesRegex(ValueError, 'unsupported expiry'):
            gate.exchange_expiry(True)

    def test_oidc_subject_accepts_immutable_repository_ids_and_rejects_other_environments(self):
        repository = {'id': 1364474149, 'owner': {'id': 204942796}}
        claims = {'repository': 'BrokkAi/feature-bot', 'repository_id': '1364474149',
                  'repository_owner_id': '204942796',
                  'sub': 'repo:BrokkAi@204942796/feature-bot@1364474149:environment:packages-publish'}
        gate.check_subject(claims, repository)
        claims['sub'] = claims['sub'].replace('packages-publish', 'other')
        with self.assertRaisesRegex(ValueError, 'environment'):
            gate.check_subject(claims, repository)

    def test_compression_metadata_does_not_change_payload(self):
        first, second = archive(mtime=0), archive(mtime=1)
        self.assertNotEqual(first, second)
        self.assertEqual(gate.payload(first), gate.payload(second))
        self.assertNotEqual(gate.payload(first), gate.payload(archive(b'wrong binary')))
        self.assertNotEqual(gate.payload(first), gate.payload(archive(mode=0o644)))

    def test_registry_verification_accepts_identical_payload_with_other_compression(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'packages/npm').mkdir(parents=True)
            (root / 'packages/npm/p.tgz').write_bytes(archive(mtime=0))
            p = {'name': 'p', 'version': '1.2.3', 'filename': 'p.tgz'}
            remote = archive(mtime=1)
            record = dict(p, dist={'tarball': 'https://registry.npmjs.org/p.tgz', 'integrity': gate.integrity(remote)})
            with patch.object(gate.registry, 'fetch_json', return_value=record), patch.object(gate, 'download', return_value=remote):
                self.assertTrue(gate.npm_status(p, root, required=True))
            record['dist']['integrity'] = gate.integrity(archive(b'wrong'))
            with patch.object(gate.registry, 'fetch_json', return_value=record), patch.object(gate, 'download', return_value=archive(b'wrong')):
                with self.assertRaisesRegex(ValueError, 'conflicting npm contents'):
                    gate.npm_status(p, root, required=True)

    def test_missing_package_is_not_complete_publication(self):
        with patch.object(gate.registry, 'fetch_json', return_value=None):
            with self.assertRaisesRegex(ValueError, 'missing npm package'):
                gate.npm_status({'name': 'p', 'version': '1.2.3'}, Path('unused'), required=True)

    def test_conflicting_tag_is_rejected(self):
        with patch.object(gate, 'api', return_value={'object': {'type': 'commit', 'sha': 'wrong'}}):
            with self.assertRaisesRegex(ValueError, 'another commit'):
                gate.tag_status('expected', 'v1.2.3')

    def test_version_conflict_stops_before_authorization_or_upload(self):
        with patch.dict(gate.os.environ, {'RELEASE_PUBLISH': 'true', 'GITHUB_REF': 'refs/tags/v1.2.3'}), \
                patch.object(gate, 'tag_status'), patch.object(gate, 'versions', side_effect=ValueError('conflict')), \
                patch.object(gate, 'authorization') as authorize, patch.object(gate, 'gh') as write:
            with self.assertRaisesRegex(ValueError, 'conflict'):
                gate.publish('sha', 'v1.2.3', Path('unused'), [])
            authorize.assert_not_called()
            write.assert_not_called()

    def test_complete_release_is_read_only(self):
        with patch.dict(gate.os.environ, {'RELEASE_PUBLISH': 'true', 'GITHUB_REF': 'refs/tags/v1.2.3'}), \
                patch.object(gate, 'tag_status'), patch.object(gate, 'versions', return_value=({'draft': False}, {'p': True})), \
                patch.object(gate, 'authorization') as authorize, patch.object(gate, 'api') as write:
            gate.publish('sha', 'v1.2.3', Path('unused'), [])
            authorize.assert_not_called()
            write.assert_not_called()

    def test_branch_cannot_publish(self):
        with patch.dict(gate.os.environ, {'RELEASE_PUBLISH': 'true', 'GITHUB_REF': 'refs/heads/master'}), \
                patch.object(gate, 'versions') as remote:
            with self.assertRaisesRegex(ValueError, 'explicit tag'):
                gate.publish('sha', 'v1.2.3', Path('unused'), [])
            remote.assert_not_called()

    def test_authorization_rejects_local_identity(self):
        with patch.dict(gate.os.environ, {'GITHUB_ACTIONS': 'false'}):
            with self.assertRaisesRegex(ValueError, 'publishing Actions job'):
                gate.authorization('sha', 'v1.2.3', Path('unused'))

    def test_missing_or_failed_exact_commit_run_is_not_evidence(self):
        for runs in ([], [{'headSha': 'sha', 'status': 'completed', 'conclusion': 'failure'}],
                     [{'headSha': 'wrong', 'status': 'completed', 'conclusion': 'success'}]):
            with patch.object(gate, 'gh', return_value=json.dumps(runs)):
                with self.assertRaises(ValueError):
                    gate.evidence('sha', 'v1.2.3', Path('unused'))
