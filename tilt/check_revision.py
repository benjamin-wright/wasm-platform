"""Identify the Git revision and effective source tree checked by local CI."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
RECORD = ROOT / '.tilt-ci-revision.json'


def git(*args):
    return subprocess.check_output(['git', *args], cwd=ROOT, timeout=30)


def revision():
    digest = hashlib.sha256()
    names = set(git('ls-files', '--cached', '--others', '--exclude-standard', '-z').split(b'\0'))
    for name in sorted(names - {b''}):
        path = ROOT / os.fsdecode(name)
        digest.update(name + b'\0')
        if path.is_symlink():
            content = os.fsencode(os.readlink(path))
            kind = b'link'
        elif path.is_file():
            content = path.read_bytes()
            kind = b'executable' if path.stat().st_mode & 0o111 else b'file'
        elif not path.exists():
            content = b''
            kind = b'deleted'
        else:
            raise RuntimeError(f'unsupported source entry: {name!r}')
        digest.update(kind + b'\0' + hashlib.sha256(content).digest())
    return {
        'commit': git('rev-parse', 'HEAD').decode().strip(),
        'source_sha256': digest.hexdigest(),
    }


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in {'record', 'verify'}:
        raise SystemExit('usage: check_revision.py record|verify')
    current = revision()
    if sys.argv[1] == 'record':
        RECORD.write_text(json.dumps(current) + '\n')
    else:
        if not RECORD.exists():
            raise SystemExit('No source baseline; trigger ci: record-revision first.')
        if json.loads(RECORD.read_text()) != current:
            raise SystemExit('Source changed during validation; rerun local CI against a stable tree.')
    print(json.dumps(current))


if __name__ == '__main__':
    main()
