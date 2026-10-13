# End-to-end tests

The `e2e-tests` Tilt resource verifies deployed HTTP, messaging, KV, SQL, metrics, admission, cancellation, and credential-log behavior. It waits for the permanent `demo-app`, `counter-app`, and `sql-hello` fixtures before running. See the [testing standards](../../docs/standards.md#end-to-end-testing) for the required gate.

HTTP operations have finite deadlines and a 4 MiB response limit. Polling passes its deadline into HTTP and kubectl operations; kubectl also has request and subprocess deadlines. Metric checks require a valid sample: an absent series fails the check, while an observed zero is valid.

Cancellation checks aggregate metrics and exit logs across execution-host pods, require a new cancellation-to-exit observation for each timed-out invocation, and reject capacity or accounting violations. Pod replacement or process restart during the check fails the comparison.

The untagged helper regressions exercise stalled HTTP responses, subprocess cancellation, response limits, and missing or malformed metric samples without requiring deployed fixtures. Build and test resources run through the local Tilt environment described in [contributions](../../docs/contributions.md#tilt).
