#!/usr/bin/env python3
"""Temporary validation-branch tooling; never part of the product tree."""
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import urllib.request

ROOT = Path.cwd()
MANIFEST = json.loads(Path('.native-build/manifest.json').read_text())
BASE = MANIFEST['base']
SOURCE = Path('.native-build/tree')

def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()

def assemble():
    subprocess.run(['git', 'merge-base', '--is-ancestor', BASE, 'HEAD'], check=True)
    files = [p for p in SOURCE.rglob('*') if p.is_file()]
    assert len(files) == 18, f'expected 18 authored files, got {len(files)}'
    paths = {str(p.relative_to(SOURCE)) for p in files}
    paths.update(MANIFEST['reset'])
    paths.update(MANIFEST['patches'])
    paths.update(MANIFEST['delete'])
    paths.update(['frontend/package.json', 'frontend/package-lock.json', 'THIRD_PARTY_NOTICES.txt'])
    for path, ref in MANIFEST['reset'].items():
        Path(path).write_bytes(subprocess.check_output(['git', 'show', f'{ref}:{path}']))
    for source in files:
        target = Path(source.relative_to(SOURCE))
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
    for path, edits in MANIFEST['patches'].items():
        data = Path(path).read_text()
        for edit in edits:
            count = data.count(edit['find'])
            assert count == 1, f'{path}: patch expected once, found {count}: {edit["find"][:90]}'
            data = data.replace(edit['find'], edit['replace'])
        Path(path).write_text(data)
    for path in MANIFEST['delete']:
        Path(path).unlink()
    fix = Path('.native-build/fix.py')
    if fix.exists():
        subprocess.run([sys.executable, str(fix)], check=True)
    go_files = [p for p in sorted(paths) if p.endswith('.go') and Path(p).is_file()]
    subprocess.run(['gofmt', '-w', *go_files], check=True)
    subprocess.run(['git', 'add', '--', *sorted(paths)], check=True)
    Path('.native-build/product-paths.json').write_text(json.dumps(sorted(paths)))
    print(git('diff', '--stat', BASE, '--', *sorted(paths)))

def api(method, path, data=None):
    body = None if data is None else json.dumps(data).encode()
    request = urllib.request.Request('https://api.github.com/repos/' + os.environ['GITHUB_REPOSITORY'] + path, data=body, method=method,
        headers={'Authorization': 'Bearer ' + os.environ['GH_TOKEN'], 'Accept': 'application/vnd.github+json', 'Content-Type': 'application/json', 'X-GitHub-Api-Version': '2022-11-28'})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)

def publish_tree():
    paths = json.loads(Path('.native-build/product-paths.json').read_text())
    subprocess.run(['git', 'diff', '--exit-code', '--', *paths], check=True)
    base_tree = api('GET', '/git/commits/' + BASE)['tree']['sha']
    changed = set(git('diff', '--name-only', BASE, '--', *paths).splitlines())
    entries = []
    for path in sorted(changed):
        file = Path(path)
        if not file.exists():
            entries.append({'path': path, 'mode': '100644', 'type': 'blob', 'sha': None})
            continue
        content = file.read_bytes()
        blob = api('POST', '/git/blobs', {'content': base64.b64encode(content).decode(), 'encoding': 'base64'})['sha']
        expected = hashlib.sha1(f'blob {len(content)}\0'.encode() + content).hexdigest()
        assert blob == expected, f'blob verification failed: {path}'
        entries.append({'path': path, 'mode': '100644', 'type': 'blob', 'sha': blob})
    # GitHub's Actions token intentionally cannot modify workflow trees. Export
    # the tested blob references; the authorized connector will assemble the
    # clean tree and update the PR with an expected-head lease. No branch writes.
    result = {'base': BASE, 'base_tree': base_tree, 'changed_files': len(entries), 'validation_commit': os.environ['GITHUB_SHA'], 'files': entries}
    Path('native-validated-tree.json').write_text(json.dumps(result, indent=2))
    print('NATIVE_TESTED_BLOBS=' + json.dumps(result), flush=True)

if __name__ == '__main__':
    if len(sys.argv) == 2 and sys.argv[1] == 'publish': publish_tree()
    else: assemble()
