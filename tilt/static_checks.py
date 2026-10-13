"""Read-only formatting and Helm checks for the local Tilt validation gate."""

from pathlib import Path
import subprocess


ROOT = Path(__file__).resolve().parents[1]


def run(*args, cwd=ROOT, quiet=False):
    print(' '.join(args), flush=True)
    subprocess.run(args, cwd=cwd, check=True, timeout=120,
                   stdout=subprocess.DEVNULL if quiet else None)


def main():
    run('git', 'diff', '--check')
    run('cargo', 'fmt', '--all', '--', '--check')
    for manifest in sorted((ROOT / 'examples').glob('*/Cargo.toml')):
        run('cargo', 'fmt', '--manifest-path', str(manifest), '--all', '--', '--check')
    go_files = [str(path) for directory in ['components/wp-operator', 'tests/e2e']
                for path in sorted((ROOT / directory).rglob('*.go'))]
    unformatted = subprocess.check_output(['gofmt', '-l', *go_files], cwd=ROOT, timeout=30).decode()
    if unformatted:
        raise SystemExit('Go files require gofmt:\n' + unformatted)
    for chart in ['helm/wasm-platform', 'components/monitoring/helm']:
        run('helm', 'lint', '--strict', chart)
        run('helm', 'template', 'validation', chart, '--namespace', 'wasm-platform', quiet=True)


if __name__ == '__main__':
    main()
