"""Check of the Windows updater: python tests/ix_update_check.py (Windows, needs Go).

Runs the real ix-update.ps1 against a local stand-in for GitHub's release API with
small invented programs. It must move forward only, verify the checksum, and keep
the installed program on any problem.
"""
import hashlib
import http.server
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
from pathlib import Path

if os.name != 'nt':
    print('skipped: the updater is for Windows')
    sys.exit(0)

root = Path(__file__).resolve().parents[1]
work = Path(tempfile.mkdtemp(prefix='ix-update-'))
source = work / 'fake'
source.mkdir()
(source / 'go.mod').write_text('module fake\n\ngo 1.21\n')
(source / 'main.go').write_text(
    'package main\n\nimport "fmt"\n\nvar v = "0"\n\nfunc main() { fmt.Println("tgdl-server", v, "windows/amd64 go") }\n')


def program(version):
    out = work / f'prog-{version}.exe'
    if not out.exists():
        subprocess.run(['go', 'build', '-ldflags', f'-X main.v={version}', '-o', str(out), '.'], cwd=source, check=True)
    return out


state = {}


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, body, code=200):
        self.send_response(code)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.endswith('/releases/latest'):
            if state.get('down'):
                return self.send(b'{}', 503)
            return self.send(json.dumps(state['release']).encode())
        return self.send(state['files'].get(self.path.rsplit('/', 1)[-1], b''))


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
base = f'http://127.0.0.1:{server.server_port}'


def publish(version, corrupt=False, without_asset=False):
    name = f'tgdl-server-v{version}-windows-amd64.tar.gz'
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode='w:gz') as tar:
        tar.add(program(version), arcname='tgdl-server.exe')
    body = data.getvalue()
    digest = hashlib.sha256(b'other' if corrupt else body).hexdigest()
    state['files'] = {name: body, 'SHA256SUMS': f'{digest}  {name}\n'.encode()}
    assets = [{'name': 'SHA256SUMS', 'browser_download_url': f'{base}/download/SHA256SUMS'}]
    if not without_asset:
        assets.append({'name': name, 'browser_download_url': f'{base}/download/{name}'})
    state['release'] = {'tag_name': 'v' + version, 'assets': assets}


def run(installed):
    """Installs a program of that version, runs the updater, says what is installed."""
    home = work / 'install'
    if home.exists():
        shutil.rmtree(home)
    (home / 'core-service').mkdir(parents=True)
    shutil.copy(root / 'ix-update.ps1', home)
    exe = home / 'core-service' / 'tgdl-server.exe'
    if installed:
        shutil.copy(program(installed), exe)
    env = dict(os.environ, TGDL_RELEASE_API=base)
    r = subprocess.run(['powershell', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', str(home / 'ix-update.ps1')],
                       env=env, capture_output=True, text=True)
    assert r.returncode == 0, r.stdout + r.stderr
    if not exe.exists():
        return 'nothing installed: ' + r.stdout + r.stderr, False
    version = subprocess.run([str(exe), 'version'], capture_output=True, text=True).stdout.split()[1]
    previous = (home / 'core-service' / 'tgdl-server.exe.previous').exists()
    return version, previous


failures = []


def check(name, got, want):
    print(('ok  ' if got == want else 'FAIL'), name, got)
    if got != want:
        failures.append(name)


publish('3.4.18-ix.10.2')
check('a newer local build is not replaced by an older release', run('3.4.18-ix.11'), ('3.4.18-ix.11', False))
check('a local suffix on the same version stays', run('3.4.18-ix.10.2-regularfix'), ('3.4.18-ix.10.2-regularfix', False))
check('nothing to do at the same version', run('3.4.18-ix.10.2'), ('3.4.18-ix.10.2', False))
publish('3.4.18-ix.11')
check('an older build moves forward and keeps the old one', run('3.4.18-ix.10.2-regularfix'), ('3.4.18-ix.11', True))
check('ix.10.2 moves to ix.11, not read as text', run('3.4.18-ix.10.2'), ('3.4.18-ix.11', True))
check('no program installed: it is fetched', run(''), ('3.4.18-ix.11', False))
publish('3.4.19')
check('a later base version is newer', run('3.4.18-ix.11'), ('3.4.19', True))
publish('3.4.18-ix.12', corrupt=True)
check('a bad checksum keeps the installed program', run('3.4.18-ix.11'), ('3.4.18-ix.11', False))
publish('3.4.18-ix.12', without_asset=True)
check('a release without this platform keeps the installed program', run('3.4.18-ix.11'), ('3.4.18-ix.11', False))
state['down'] = True
check('GitHub unreachable keeps the installed program', run('3.4.18-ix.11'), ('3.4.18-ix.11', False))

server.shutdown()
shutil.rmtree(work, ignore_errors=True)
if failures:
    sys.exit('failed: ' + ', '.join(failures))
print('the updater moves forward only: ok')
