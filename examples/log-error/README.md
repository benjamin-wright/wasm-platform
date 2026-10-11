# log-error

A WebAssembly regression fixture for handling errors that contain request credentials.

`POST /log-error` returns the request's `Authorization` header and body through the guest's error result. It does not emit guest log entries. The platform returns HTTP 500 with `internal error: invocation failed` and records the application identity without the error contents.

The end-to-end log regression sends dummy token and password values through this route and verifies that they appear in neither the HTTP response nor component logs.

Tilt builds the guest for `wasm32-wasip2`, publishes `log_error.wasm` to the local OCI registry, and deploys its Application in the `examples` namespace.
