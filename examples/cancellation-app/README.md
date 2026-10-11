# cancellation-app

Regression fixture for interrupted HTTP and message invocations. Its SQL table is created by a migration, and both functions share the application's KV store and SQL writer.

`GET /cancellation` checks readiness. `GET /cancellation?<token>` returns `sql=N kv=true|false` from the database and KV store.

`POST /cancellation` accepts a plain-text command: `<mode> <token> <delay-seconds>`. Prefix a command with `message ` to dispatch it to the message function through `messaging.send`.

| Mode | Behavior |
|---|---|
| `cpu` | Loops indefinitely. |
| `sleep` | Sleeps through WASI, then writes the KV key. |
| `sql` | Submits a delayed SQL insert, then attempts a KV write even if SQL failed. |
| `committed` | Commits SQL and KV writes, then loops indefinitely. |
| `write` | Commits SQL and KV writes and returns successfully. |
| `transaction` | Succeeds only when guest transaction-control and timeout-setting statements are rejected. |

The `cancellation_started_total` metric counts started actions by `world` and `mode`. Tokens distinguish state across test runs.

Build both modules with:

```sh
cargo build --locked --manifest-path examples/cancellation-app/Cargo.toml --target wasm32-wasip2 --release --target-dir target
```

The fixture's Tiltfile builds and publishes the modules and migration bundle, then deploys its Application. The cancellation e2e test uses the gateway and reads stored effects after the invocation deadline.
