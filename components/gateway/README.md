# Gateway

The HTTP gateway for wasm-platform. Accepts inbound HTTP traffic, translates each request into a platform-private NATS payload, publishes it to the target application's NATS subject, waits for the reply, and returns the HTTP response to the caller. The listener accepts HTTP/1.1.

## Responsibilities

- Maintain an in-memory route table populated via gRPC from the wp-operator (`GatewayRoutes` service).
- On each inbound HTTP request:
  1. Look up the request path in the route table → `404` if not found.
  2. Enforce the method allow-list → `405 Method Not Allowed` (with `Allow` header) if the method is not permitted.
  3. Reserve request capacity, read a bounded body within its deadline, and serialise a bounded **platform-private JSON payload** (see below). Publish it to the application's NATS subject with a gateway-owned reply reservation.
  4. Deserialise the JSON reply from the execution host and construct an HTTP response.
  5. Return `504 Gateway Timeout` if dispatch exceeds the remaining request budget.

## Route Sync Protocol

The gateway connects to the wp-operator's gRPC server (same address as the `ConfigSync` service, same port) and calls the `GatewayRoutes` service:

1. **`RequestFullRoutes`** — called on startup (and on reconnect after any error). Returns the complete set of routes currently known to the operator.
2. **`PushRouteUpdate` (bidirectional stream)** — the operator pushes `RouteUpdateRequest` messages containing incremental adds, updates, and deletes. The gateway acknowledges each with a `RouteUpdateAck`.

On any stream error or close, the gateway backs off exponentially and reconnects, re-requesting a full snapshot.

## Platform JSON Payload Format

The gateway serialises the incoming HTTP request as JSON before publishing to NATS. This format is **internal to the platform** — guest modules never see it. The execution host decodes it, calls `on-request` with typed WIT records, and serialises the returned `http-response` record back to JSON for the NATS reply.

### Request payload (gateway → NATS → execution host)

```json
{
  "method": "POST",
  "path": "/api/orders",
  "query": "debug=true",
  "headers": [["content-type", "application/json"], ["x-user-id", "42"]],
  "body": [123, 34, 104, 101, ...]
}
```

| Field     | Type                       | Notes                                         |
|-----------|----------------------------|-----------------------------------------------|
| `method`  | string                     | HTTP method in uppercase                      |
| `path`    | string                     | Request path (no query string)                |
| `query`   | string                     | Raw query string, empty string if absent      |
| `headers` | array of `[string, string]`| All request headers as key/value pairs        |
| `body`    | array of bytes or `null`   | Request body bytes, `null` if body is empty   |

### Response payload (execution host → NATS reply → gateway)

```json
{
  "status": 200,
  "headers": [["content-type", "application/json"]],
  "body": [123, 34, 111, 107, ...]
}
```

| Field     | Type                       | Notes                                         |
|-----------|----------------------------|-----------------------------------------------|
| `status`  | number                     | HTTP status code                              |
| `headers` | array of `[string, string]`| Response headers                              |
| `body`    | array of bytes or `null`   | Response body bytes, `null` if body is empty  |

## Request ownership and limits

Route and method checks run before body polling. Valid requests reserve capacity immediately; excess work receives `503` with no waiting queue. The reservation lasts through body intake, NATS dispatch, and buffered response consumption or disposal. Dropping a request removes its pending reply record; late replies are discarded. Cancellation after publication does not undo guest work or committed effects.

The body deadline is absolute: sending more chunks does not extend it. One monotonic request budget covers body intake, serialisation and NATS dispatch. Both declared and streamed body lengths are checked. Serialised JSON has a separate byte cap because byte arrays expand in JSON. The broker's advertised maximum payload can impose a smaller cap.

The listener closes excess TCP connections before parsing requests. Each connection has a 32 KiB parsing buffer, a 128-header count limit, and a header-read deadline equal to the body-read timeout. Request metadata counts raw URI, method, header names and values. NATS command and reply-subscription buffers each hold at most the configured in-flight request count. Limits apply per gateway process. The defaults bound request and transport allocations within the chart's 256 MiB pod limit; higher settings require corresponding memory capacity.

## Configuration

All numeric settings must be positive integers within the maxima below; invalid values fail startup.

| Variable | Default | Maximum | Description |
|---|---|---|---|
| `OPERATOR_ADDR` | required | — | Operator gRPC address |
| `HOSTNAME` | `unknown` | — | Gateway ID, injected by the downward API |
| `NATS_CREDENTIALS_PATH` | required | — | Mounted directory containing `NATS_HOST`, `NATS_PORT`, `NATS_USERNAME`, `NATS_PASSWORD` |
| `HTTP_PORT` | `3000` | `65535` | Application HTTP listener |
| `GATEWAY_TIMEOUT_SECS` | `30` | `300` | Body intake through NATS reply budget |
| `GATEWAY_BODY_READ_TIMEOUT_SECS` | `5` | `300` | Absolute body and header read deadline |
| `GATEWAY_MAX_IN_FLIGHT_REQUESTS` | `32` | `256` | Admitted requests, including buffered replies |
| `GATEWAY_MAX_CONNECTIONS` | `128` | `4096` | Accepted application TCP connections |
| `GATEWAY_MAX_BODY_BYTES` | `131072` | `1048576` | Raw body bytes |
| `GATEWAY_MAX_METADATA_BYTES` | `16384` | `65536` | Raw request metadata bytes |
| `GATEWAY_MAX_PAYLOAD_BYTES` | `786432` | `1048576` | Serialised request bytes |

## Error mapping

| Condition | HTTP status |
|---|---|
| Path not in route table | `404 Not Found` |
| Method not in allow-list | `405 Method Not Allowed`, with `Allow` |
| Request capacity exhausted or NATS unavailable | `503 Service Unavailable` |
| Raw body or serialised payload exceeds a limit | `413 Payload Too Large` |
| Request metadata exceeds its limit | `431 Request Header Fields Too Large` |
| Body read deadline expires | `408 Request Timeout` |
| Malformed body framing | `400 Bad Request` |
| NATS publish/reply error or malformed reply JSON | `502 Bad Gateway` |
| Request budget expires during dispatch | `504 Gateway Timeout` |

## Metrics

Port `9090` exposes `/metrics`, `/healthz` and `/readyz` on a separate listener with eight connection slots. Kubernetes probes use this listener, so application connection saturation does not block probes. Health endpoints also remain available on the application listener and bypass request admission.

Prometheus reads request-supervisor snapshots; reporting does not control admission or cleanup. Metrics have fixed labels and contain no route, query, header or credential values.

| Metric | Meaning |
|---|---|
| `wasm_gateway_work{pool}` | Current admitted requests or accepted application connections |
| `wasm_gateway_limit{pool}` | Capacity for `in_flight` requests and `connections` |
| `wasm_gateway_high_water{pool="in_flight"}` | Highest observed admitted request count since startup |
| `wasm_gateway_requests_rejected_total` | Valid-route requests rejected for lack of capacity |
| `wasm_gateway_requests_completed_total` | Released requests whose handler produced a response, including error responses |
| `wasm_gateway_requests_cancelled_total` | Released requests cancelled before the handler produced a response |
| `process_*` | Linux process metrics, including resident memory |

Local Prometheus retains these per-pod samples; see [monitoring](../monitoring/README.md).

## Transport

TLS termination is handled externally by Kubernetes Ingress or a sidecar. The gateway does not authenticate HTTP callers; request headers are forwarded to the guest.
