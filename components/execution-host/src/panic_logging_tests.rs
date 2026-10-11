use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use crate::logging_tests::LogCapture;

const PANIC_PASSWORD: &str = "panic-password-sentinel";
const PANIC_TOKEN: &str = "panic-token-sentinel";
const PANIC_BODY: &str = "arbitrary-private-panic-body-sentinel";

#[test]
fn panic_hook_omits_payload_from_subprocess_output() {
    let mut child =
        Command::new(std::env::current_exe().expect("test executable path is available"))
            .args([
                "--exact",
                "panic_logging_tests::panic_hook_subprocess_fixture",
                "--ignored",
                "--nocapture",
                "--test-threads=1",
            ])
            .env("RUST_BACKTRACE", "full")
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("panic hook subprocess starts");

    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if child
            .try_wait()
            .expect("panic hook subprocess status is available")
            .is_some()
        {
            break;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            panic!("panic hook subprocess exceeded its deadline");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    let output = child
        .wait_with_output()
        .expect("panic hook subprocess output is available");
    assert!(
        output.status.success(),
        "panic hook subprocess failed; output withheld"
    );

    let stdout = String::from_utf8_lossy(&output.stdout);
    let stderr = String::from_utf8_lossy(&output.stderr);
    for secret in [PANIC_PASSWORD, PANIC_TOKEN, PANIC_BODY] {
        assert!(
            !stdout.contains(secret) && !stderr.contains(secret),
            "panic payload appeared in process output; content withheld",
        );
    }
    let diagnostic = stdout
        .lines()
        .find(|line| line.contains("process panicked"))
        .expect("panic hook emits a safe diagnostic");
    assert!(
        diagnostic.contains(file!()),
        "panic diagnostic includes the source file"
    );
    assert!(
        diagnostic.contains("line="),
        "panic diagnostic includes the source line"
    );
}

// Only the parent regression launches this fixture. Isolating the process keeps
// replacement of Rust's global panic hook from affecting concurrent tests.
#[test]
#[ignore]
fn panic_hook_subprocess_fixture() {
    let capture = LogCapture::default();
    let _subscriber = capture.subscribe();
    platform_common::logging::install_panic_hook();

    let payload = format!(
        "postgres://user:{PANIC_PASSWORD}@localhost/database Authorization: Bearer {PANIC_TOKEN} {PANIC_BODY}"
    );
    let panicked = std::panic::catch_unwind(|| panic!("{payload}"));
    assert!(
        panicked.is_err(),
        "panic hook fixture exercises a real panic"
    );
    print!("{}", capture.text());
}
