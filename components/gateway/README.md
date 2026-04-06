# Gateway

The HTTP gateway for wasm-platform. Accepts inbound HTTP traffic, translates each request into a platform-private NATS payload, publishes it to the target application's NATS subject, waits for the reply, and returns the HTTP response to the caller.

## Responsibilities

- Maintain an in-memory route table populated via gRPC from the wp-operator (`GatewayRoutes` service).
- On each inbound HTTP request:
  1. Look up the request path in the route table → `404` if not found.
  2. Enforce the method allow-list → `405 Method Not Allowed` (with `Allow` header) if the method is not permitted.
  3. Serialise the request as a **platform-private JSON payload** (see below) and publish via `async_nats::Client::request()` to the application's NATS subject.
  4. Deserialise the JSON reply from the execution host and construct an HTTP response.
  5. Return `504 Gateway Timeout` if the NATS reply does not arrive within `GATEWAY_TIMEOUT_SECS`.

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

## Configuration

| Variable               | Default   | Description                                                    |
|------------------------|-----------|----------------------------------------------------------------|
| `OPERATOR_ADDR`        | required  | gRPC address of the wp-operator (e.g. `http://wp-operator-grpc:50051`) |
| `HOSTNAME`             | `unknown` | Used as the `gateway_id` in gRPC messages (injected by the downward API) |
| `GATEWAY_TIMEOUT_SECS` | `30`      | Maximum time to wait for a NATS reply before returning `504`  |
| `HTTP_PORT`            | `3000`    | Port the HTTP server listens on                                |
| `NATS_USERNAME`        | —         | NATS credentials (all four must be set together or all absent) |
| `NATS_PASSWORD`        | —         |                                                                |
| `NATS_HOST`            | —         |                                                                |
| `NATS_PORT`            | —         | When all four are absent the gateway falls back to unauthenticated `localhost:4222` |

## Error mapping

| Condition                                  | HTTP status                    |
|--------------------------------------------|-------------------------------|
| Path not in route table                    | `404 Not Found`               |
| Method not in allow-list                   | `405 Method Not Allowed`      |
| NATS publish error                         | `502 Bad Gateway`             |
| No NATS reply within `GATEWAY_TIMEOUT_SECS`| `504 Gateway Timeout`         |
| Malformed JSON in NATS reply               | `502 Bad Gateway`             |

## No TLS / No Auth (MVP)

TLS termination is handled externally (Kubernetes Ingress or a sidecar). Auth middleware is not part of the MVP; the JSON payload includes a `headers` array so a future middleware layer can inject `x-user-id` (or similar) before the NATS publish without changes to the gateway or execution host.
