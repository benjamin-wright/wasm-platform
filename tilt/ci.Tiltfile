config.define_bool('ci', usage='Run all local validation resources on startup')
CI_MODE = config.parse().get('ci', False)

BASELINE_CHECKS = [
    'ci: static-checks',
    'execution-host: regression-tests',
    'execution-host: lint',
    'execution-host: redis-cancellation-tests',
    'gateway: regression-tests',
    'gateway: lint',
    'module-cache: regression-tests',
    'module-cache: lint',
    'wp: unit-tests',
    'wp: lint',
    'wp: integration-tests',
    'e2e: harness-tests',
    'e2e-tests',
    'monitoring: config-check',
]

def ci():
    local_resource(
        'ci: record-revision',
        cmd = 'python3 tilt/check_revision.py record',
        labels = ['tests'],
        auto_init = CI_MODE,
        trigger_mode = TRIGGER_MODE_MANUAL,
    )
    local_resource(
        'ci: static-checks',
        cmd = 'python3 tilt/static_checks.py',
        deps = ['Cargo.toml', 'Cargo.lock', 'components', 'lib', 'examples', 'tests/e2e', 'helm', 'tilt/static_checks.py'],
        resource_deps = ['ci: record-revision'] if CI_MODE else [],
        labels = ['tests'],
        auto_init = CI_MODE,
        trigger_mode = TRIGGER_MODE_AUTO if CI_MODE else TRIGGER_MODE_MANUAL,
    )
    local_resource(
        'ci: verify-revision',
        cmd = 'python3 tilt/check_revision.py verify',
        resource_deps = ['ci: record-revision'] + BASELINE_CHECKS,
        labels = ['tests'],
        auto_init = CI_MODE,
        trigger_mode = TRIGGER_MODE_MANUAL,
    )
