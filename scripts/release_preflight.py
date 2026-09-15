#!/usr/bin/env python3
"""Release gates and recovery. Only the explicit `publish` command uploads."""
import argparse
import base64
from datetime import datetime, timezone, timedelta
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request

import package_installers as installers
import package_registry as registry
import package_release as native
import smoke_installers

REPO = 'BrokkAi/feature-bot'
NAMES = [f'@brokkai/feature-bot-{s}-{a}' for s in ('linux', 'darwin') for a in ('x64', 'arm64')] + ['@brokkai/feature-bot']


def require(condition, message):
    if not condition:
        raise ValueError(message)


def gh(*args):
    return subprocess.check_output(['gh', *args], text=True)


def api(path, *args, missing=False):
    result = subprocess.run(['gh', 'api', '--hostname', 'github.com', path, *args], capture_output=True, text=True)
    if result.returncode:
        if missing and '(HTTP 404)' in result.stderr:
            return None
        raise ValueError(f'GitHub API {path}: {result.stderr.strip()}')
    return json.loads(result.stdout) if result.stdout.strip() else None


def context():
    sha, tag = os.environ['RELEASE_COMMIT'], os.environ['RELEASE_TAG']
    native.validate_tag(tag)
    require(native.commit() == sha, 'checkout differs from RELEASE_COMMIT')
    require(not native.run('git', 'status', '--porcelain').strip(), 'checkout is dirty')
    target = os.environ.get('RELEASE_TARGET', sha)
    subprocess.run(['git', 'merge-base', '--is-ancestor', target, sha], check=True)
    return sha, tag, Path('dist/preflight') / sha / tag


def payload(data):
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
        members = archive.getmembers()
        require(all(m.isfile() for m in members), 'non-file archive entry')
        require(len({m.name for m in members}) == len(members), 'duplicate archive entry')
        return {m.name: (m.mode & 0o777, archive.extractfile(m).read()) for m in members}


def build(sha, tag, root):
    # Completed output is reusable only after verifying its exact manifests and bytes.
    if not (root / 'built.json').exists():
        require(not root.exists(), 'incomplete build directory; inspect it and use a fresh output directory')
        if os.environ.get('GITHUB_ACTIONS') != 'true':
            for command in (['go', 'test', '-race', './...'], ['go', 'vet', './...'],
                            ['python3', '-m', 'unittest', 'discover', '-s', 'scripts', '-p', '*_test.py'],
                            ['node', '--test', '--test-isolation=none', 'npm/bfb.test.cjs'],
                            ['sh', '-n', 'install.sh']):
                subprocess.run(command, check=True)
        native.package(tag, root / 'native')
        installers.package(tag, root / 'native', root / 'packages', sha)
        smoke_installers.smoke(root / 'packages')
        (root / 'built.json').write_text(json.dumps({'commit': sha, 'tag': tag}))
    require(json.loads((root / 'built.json').read_text()) == {'commit': sha, 'tag': tag}, 'wrong build identity')
    native.verify_local(tag, root / 'native', sha)
    manifest = json.loads((root / 'packages/npm/manifest.json').read_text())
    require(manifest['commit'] == sha and manifest['tag'] == tag, 'wrong npm build identity')
    packages = manifest['packages']
    require(len(packages) == 5 and {p['name'] for p in packages} == set(NAMES), 'missing npm destination')
    for p in packages:
        require(p['version'] == tag[1:] and Path(p['filename']).name == p['filename'], 'invalid package metadata')
        data = (root / 'packages/npm' / p['filename']).read_bytes()
        require(native.digest(data) == p['sha256'] and integrity(data) == p['integrity'], 'corrupt npm package')
    return sorted(packages, key=lambda p: NAMES.index(p['name']))


def integrity(data):
    return 'sha512-' + base64.b64encode(hashlib.sha512(data).digest()).decode()


def download(url):
    require(url.startswith('https://'), 'artifact URL must use HTTPS')
    with urllib.request.urlopen(url, timeout=60) as response:
        return response.read()


def npm_status(p, root, required=False):
    name = urllib.parse.quote(p['name'], safe='')
    record = registry.fetch_json(f'https://registry.npmjs.org/{name}/{p["version"]}')
    if record is None:
        require(not required, f'missing npm package {p["name"]}@{p["version"]}')
        return False
    require(record.get('name') == p['name'] and record.get('version') == p['version'], 'wrong npm registry metadata')
    data = download(record['dist']['tarball'])
    require(integrity(data) == record['dist']['integrity'], 'npm download integrity mismatch')
    expected = (root / 'packages/npm' / p['filename']).read_bytes()
    require(payload(data) == payload(expected), f'conflicting npm contents: {p["name"]}')
    return True


def tag_status(sha, tag, required=False):
    ref = api(f'repos/{REPO}/git/ref/tags/{tag}', missing=True)
    if ref is None:
        require(not required, 'release tag is missing')
        return False
    obj = ref['object']
    while obj['type'] == 'tag':
        obj = api(f'repos/{REPO}/git/tags/{obj["sha"]}')['object']
    require(obj['type'] == 'commit' and obj['sha'] == sha, 'release tag points to another commit')
    return True


def github_status(sha, tag, root, required=False):
    # List includes drafts, unlike the public tag endpoint on some API versions.
    releases = json.loads(gh('api', '--hostname', 'github.com', f'repos/{REPO}/releases', '--paginate', '--slurp'))
    matches = [r for page in releases for r in page if r['tag_name'] == tag]
    require(len(matches) <= 1, 'multiple releases have this tag; resolve the conflict explicitly')
    if not matches:
        require(not required, 'GitHub release is missing')
        return None
    record = matches[0]
    if required:
        require(not record['draft'] and record['published_at'], 'GitHub release is still a draft')
    expected = {p.name: p for p in (root / 'native').iterdir()}
    assets = record['assets']
    require(len({a['name'] for a in assets}) == len(assets), 'duplicate GitHub assets')
    require({a['name'] for a in assets} <= set(expected), 'unexpected GitHub asset')
    with tempfile.TemporaryDirectory() as temporary:
        directory = Path(temporary)
        for asset in assets:
            data = subprocess.check_output(['gh', 'api', '--hostname', 'github.com',
                f'repos/{REPO}/releases/assets/{asset["id"]}', '-H', 'Accept: application/octet-stream'])
            (directory / asset['name']).write_bytes(data)
        if record['draft']:
            # Partial staging must match exact bytes; never overwrite a conflicting upload.
            for path in directory.iterdir():
                require(path.read_bytes() == expected[path.name].read_bytes(), f'conflicting draft asset {path.name}')
        else:
            native.verify_local(tag, directory, sha)
            for path in directory.glob('*.tar.gz'):
                require(payload(path.read_bytes()) == payload(expected[path.name].read_bytes()), f'conflicting native contents {path.name}')
    return record


def versions(sha, tag, root, packages, required=False):
    tag_status(sha, tag, required)
    record = github_status(sha, tag, root, required)
    existing = {p['name']: npm_status(p, root, required) for p in packages}
    return record, existing


def request_json(url, token, method='GET'):
    request = urllib.request.Request(url, method=method, headers={'Authorization': f'Bearer {token}', 'Accept': 'application/json'})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)


def exchange_expiry(value):
    if isinstance(value, int) and not isinstance(value, bool):
        return datetime.fromtimestamp(value, timezone.utc)
    if isinstance(value, str):
        return datetime.fromisoformat(value.replace('Z', '+00:00'))
    raise ValueError('npm exchange returned an unsupported expiry')


def check_subject(claims, repository):
    owner_id, repo_id = str(repository['owner']['id']), str(repository['id'])
    subjects = {
        f'repo:{REPO}:environment:packages-publish',
        f'repo:BrokkAi@{owner_id}/feature-bot@{repo_id}:environment:packages-publish',
    }
    require(claims.get('repository') == REPO and
        str(claims.get('repository_owner_id')) == owner_id and
        str(claims.get('repository_id')) == repo_id and
        claims.get('sub') in subjects, 'wrong OIDC repository or environment')


def authorization(sha, tag, root):
    require(os.environ.get('GITHUB_ACTIONS') == 'true', 'authorization must run in the publishing Actions job')
    require(os.environ['GITHUB_REPOSITORY'] == REPO and os.environ['GITHUB_SHA'] == sha, 'wrong Actions repository or SHA')
    require(os.environ['GITHUB_JOB'] == 'packages', 'wrong publishing job')
    url = os.environ['ACTIONS_ID_TOKEN_REQUEST_URL'] + '&audience=npm%3Aregistry.npmjs.org'
    token = request_json(url, os.environ['ACTIONS_ID_TOKEN_REQUEST_TOKEN'])['value']
    encoded = token.split('.')[1]
    claims = json.loads(base64.urlsafe_b64decode(encoded + '=' * (-len(encoded) % 4)))
    check_subject(claims, api(f'repos/{REPO}'))
    require(claims['workflow_ref'].startswith(f'{REPO}/.github/workflows/publish-packages.yml@'), 'wrong publishing workflow')
    require(claims['sha'] == sha and claims['aud'] == 'npm:registry.npmjs.org', 'wrong OIDC commit or audience')
    expiries = []
    for name in NAMES:
        endpoint = 'https://registry.npmjs.org/-/npm/v1/oidc/token/exchange/package/' + urllib.parse.quote(name, safe='@')
        result = request_json(endpoint, token, 'POST')
        require(result.get('token_type') == 'oidc' and bool(result.get('token')), f'npm did not grant a package publishing token for {name}')
        expiry = exchange_expiry(result['expires'])
        require(expiry > datetime.now(timezone.utc) + timedelta(minutes=5), f'npm publishing token expires too soon: {name}')
        expiries.append(expiry.isoformat())
        print(f'npm accepted package-scoped OIDC exchange for {name}; expires {expiry.isoformat()}')
    # No tag is created by a draft release. Delete only the unique probe we created.
    probe = f'preflight-{sha[:12]}-{os.environ["GITHUB_RUN_ID"]}-{os.environ["GITHUB_RUN_ATTEMPT"]}'
    record = api(f'repos/{REPO}/releases', '-X', 'POST', '-f', f'tag_name={probe}', '-f', f'target_commitish={sha}', '-F', 'draft=true', '-f', 'name=Disposable authorization probe')
    try:
        require(record['draft'] and record['tag_name'] == probe, 'authorization probe is not a draft')
        api(f'repos/{REPO}/releases/{record["id"]}', '-X', 'PATCH', '-f', 'body=Non-publishing authorization check')
    finally:
        api(f'repos/{REPO}/releases/{record["id"]}', '-X', 'DELETE')
    proof = {'commit': sha, 'tag': tag, 'run_id': os.environ['GITHUB_RUN_ID'],
        'attempt': os.environ['GITHUB_RUN_ATTEMPT'], 'workflow_ref': claims['workflow_ref'],
        'environment': 'packages-publish', 'job': 'packages', 'packages': NAMES,
        'expires': min(expiries), 'github_draft_create_update_delete': True}
    (root / 'authorization.json').write_text(json.dumps(proof, indent=2))


def evidence(sha, tag, root):
    runs = json.loads(gh('run', 'list', '--repo', f'github.com/{REPO}', '--commit', sha,
        '--workflow', 'publish-packages.yml', '--event', 'workflow_dispatch', '--limit', '100',
        '--json', 'databaseId,headSha,status,conclusion'))
    require(bool(runs), 'missing exact-commit preflight run')
    run = runs[0]
    require(run['headSha'] == sha and run['status'] == 'completed' and run['conclusion'] == 'success', 'latest preflight did not succeed')
    detail = json.loads(gh('run', 'view', str(run['databaseId']), '--repo', f'github.com/{REPO}', '--json', 'headSha,status,conclusion,jobs,attempt'))
    require(detail['headSha'] == sha and detail['conclusion'] == 'success', 'wrong preflight SHA or conclusion')
    jobs = detail['jobs']
    require(any(j['name'] == 'packages' for j in jobs) and all(j['conclusion'] == 'success' for j in jobs), 'missing or unsuccessful publishing-context job')
    with tempfile.TemporaryDirectory() as temporary:
        gh('run', 'download', str(run['databaseId']), '--repo', f'github.com/{REPO}', '--name', f'preflight-{tag}', '--dir', temporary)
        proof = json.loads((Path(temporary) / 'authorization.json').read_text())
        require(proof['commit'] == sha and proof['tag'] == tag and proof['run_id'] == str(run['databaseId']) and proof['attempt'] == str(detail['attempt']), 'wrong authorization evidence')
        require(proof['packages'] == NAMES and proof['job'] == 'packages' and proof['environment'] == 'packages-publish' and proof['github_draft_create_update_delete'], 'incomplete authorization evidence')
        require(datetime.fromisoformat(proof['expires']) > datetime.now(timezone.utc), 'authorization evidence expired; rerun non-publishing preflight')
    print(f'Exact-commit publishing-context authorization passed in run {run["databaseId"]}')


def tag_authorization():
    # Tag creation is performed by the release operator/daemon, not by Actions.
    response = gh('api', '--hostname', 'github.com', '-i', f'repos/{REPO}')
    headers, body = response.split('\n\n', 1)
    fields = dict(line.split(': ', 1) for line in headers.splitlines() if ': ' in line)
    fields = {key.lower(): value for key, value in fields.items()}
    scopes = {s.strip() for s in fields.get('x-oauth-scopes', '').split(',')}
    require('repo' in scopes, 'tag publisher must have a validated repo-scoped OAuth identity')
    require(json.loads(body)['permissions']['push'], 'tag publisher lacks repository push permission')
    expiry = fields.get('github-authentication-token-expiration')
    if expiry:
        require(datetime.fromisoformat(expiry.replace('Z', '+00:00')) > datetime.now(timezone.utc), 'tag publisher token expired')
    require(api(f'repos/{REPO}/rulesets') == [], 'repository rulesets require explicit tag publisher review')
    identity = api('user')['login']
    print(f'Git tag publisher {identity}: active repo OAuth scope, push permission, no tag restrictions')


def publish(sha, tag, root, packages):
    require(os.environ.get('RELEASE_PUBLISH') == 'true' and os.environ.get('GITHUB_REF') == f'refs/tags/{tag}', 'publication requires an explicit tag workflow trigger')
    tag_status(sha, tag, required=True)
    record, existing = versions(sha, tag, root, packages)
    if record and not record['draft'] and all(existing.values()):
        print('Complete immutable release verified; nothing to upload')
        return
    authorization(sha, tag, root)  # Recheck package-scoped identity before any final asset upload.
    if record is None:
        record = api(f'repos/{REPO}/releases', '-X', 'POST', '-f', f'tag_name={tag}', '-f', f'target_commitish={sha}', '-F', 'draft=true', '-f', f'name=Brokk Feature Bot {tag}')
    if record['draft']:
        names = {a['name'] for a in record['assets']}
        for path in sorted((root / 'native').iterdir()):
            if path.name not in names:
                gh('release', 'upload', tag, str(path), '--repo', f'github.com/{REPO}')
        staged = github_status(sha, tag, root)  # Checks uploaded bytes against this job's staging.
        require({a['name'] for a in staged['assets']} == {p.name for p in (root / 'native').iterdir()},
                'native staging is incomplete; do not finalize the release')
    for p in packages:
        if not existing[p['name']]:
            subprocess.run(['npm', 'publish', str((root / 'packages/npm' / p['filename']).resolve()), '--access', 'public', '--registry', 'https://registry.npmjs.org', '--tag', 'next' if '-' in tag else 'latest'], check=True)
            # Upload integrity uses the staged digest, independent verification compares payloads.
            for attempt in range(20):
                name = urllib.parse.quote(p['name'], safe='')
                remote = registry.fetch_json(f'https://registry.npmjs.org/{name}/{p["version"]}')
                if remote:
                    require(remote['dist']['integrity'] == p['integrity'], 'uploaded npm bytes differ from staging')
                    break
                time.sleep(15)
            else:
                raise ValueError(f'npm version is not visible yet: {p["name"]}; resume this tag later')
    for p in packages:
        npm_status(p, root, required=True)
    if record['draft']:
        gh('release', 'edit', tag, '--repo', f'github.com/{REPO}', '--draft=false', '--prerelease=' + str('-' in tag).lower(), '--latest=' + str('-' not in tag).lower())
    versions(sha, tag, root, packages, required=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['build', 'authorization', 'version', 'published', 'authorize-job', 'tag-authorization', 'publish'])
    args = parser.parse_args()
    sha, tag, root = context()
    if args.command == 'tag-authorization':
        tag_authorization()
        return
    if args.command == 'authorization':
        evidence(sha, tag, root)
        return
    packages = build(sha, tag, root)
    if args.command == 'authorize-job':
        authorization(sha, tag, root)
    elif args.command in ('version', 'published'):
        versions(sha, tag, root, packages, args.command == 'published')
    elif args.command == 'publish':
        publish(sha, tag, root, packages)
    print(f'{args.command} passed for {tag} at {sha}')


if __name__ == '__main__':
    main()
