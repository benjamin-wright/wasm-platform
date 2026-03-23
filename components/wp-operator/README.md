# wp-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning database bindings, registering message subscriptions, and managing the lifecycle of deployed WASM modules.

## Application CRD

`Application` is the primary resource. Each instance declares a single deployable WASM module and its runtime requirements.

```yaml
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: default
spec:
  module: oci://registry.example.com/my-app@sha256:<digest>
  topic: my-app.messages
  env:
    LOG_LEVEL: info
  sql: orders
  keyValue: sessions
```

### Fields

#### `spec.module`

| Type | Required | Description |
|------|----------|-------------|
| string | yes | OCI URI for the `.wasm` module. Use a digest-pinned reference (`@sha256:…`) for deterministic deployments. Format: `oci://<registry>/<repository>@sha256:<digest>`. |

#### `spec.topic`

| Type | Required | Description |
|------|----------|-------------|
| string | yes | Message subject the execution host subscribes to. Messages arriving on this subject invoke the module's `on-message` export. |

#### `spec.env` (optional)

A string map of environment variables injected into the module's runtime configuration. Keys must be unique.

```yaml
env:
  LOG_LEVEL: info
  FEATURE_FLAG: enabled
```

#### `spec.sql` (optional)

| Type | Required | Description |
|------|----------|-------------|
| string | no | Logical database name. Exposed to the module via the `sql` host import as the `db` argument. Must correspond to a provisioned database managed by the [db-operator](https://github.com/benjamin-wright/db-operator). Omit to disable SQL access entirely. |

#### `spec.keyValue` (optional)

| Type | Required | Description |
|------|----------|-------------|
| string | no | Key prefix for the module's key-value namespace. Keys written by the module are namespaced by `<namespace>/<prefix>/` to prevent conflicts between applications. Must correspond to a provisioned KV store managed by the [db-operator](https://github.com/benjamin-wright/db-operator). Omit to disable KV access entirely. |

## Operator Behaviour

On `Application` create or update, the operator:

1. Resolves the OCI digest for `spec.module` if a mutable tag is given, and writes the resolved digest to the status.
2. If `spec.sql` is set, ensures the named database exists (via [db-operator](https://github.com/benjamin-wright/db-operator) resources) and that credentials are provisioned.
3. If `spec.keyValue` is set, ensures the KV store exists (via [db-operator](https://github.com/benjamin-wright/db-operator) resources) and that credentials are provisioned.
4. Creates or updates the message consumer configuration for `spec.topic`.
5. Pushes an incremental config update (env vars + binding references + resolved module reference) to all connected execution hosts via the gRPC `PushIncrementalUpdate` RPC so they can load the new module.

On `Application` delete, the operator removes the message consumer and releases (but does not destroy) the database and KV bindings so data is not lost on accidental deletion.

## Config API

The operator exposes a gRPC `ConfigSync` service that execution hosts use to stay in sync.

### Service Definition

```proto
syntax = "proto3";
package configsync;

service ConfigSync {
  // Client requests a full configuration (on startup or desync)
  rpc RequestFullConfig(FullConfigRequest) returns (FullConfigResponse);

  // Server streams incremental updates to the client; client streams acks back
  rpc PushIncrementalUpdate(stream IncrementalUpdateAck) returns (stream IncrementalUpdateRequest);
}
```

### RPCs

- **`RequestFullConfig` (host → operator)** — on startup or when a desync is suspected, the execution host calls this RPC to receive the latest full configuration snapshot for all applications. The response includes the complete `FullConfig` (version, all `ApplicationConfig` entries, and timestamp).

- **`PushIncrementalUpdate` (bidirectional streaming)** — after an execution host connects, it calls this RPC and keeps the stream open. The operator streams `IncrementalUpdateRequest` messages (config deltas) to the host whenever applications are created, updated, or deleted. Each message carries a version identifier, a list of `AppUpdate` entries (add/modify or delete), and a timestamp. The host streams back an `IncrementalUpdateAck` after processing each delta; if the ack reports a failure, the host should close the stream and re-request the full configuration via `RequestFullConfig`.

### Key Message Types

#### Full Configuration Flow

```proto
message FullConfigRequest {
  string host_id = 1;                    // Identifier for the execution host
  optional int64 last_ack_timestamp = 2; // Timestamp of the last successfully applied config; omit or zero if unknown
}

message FullConfigResponse {
  FullConfig config = 1; // Full configuration payload
  bool success = 2;      // Whether the full config was successfully retrieved
  string message = 3;    // Optional message for errors or metadata
}

message FullConfig {
  string version = 1;                          // Config version identifier
  repeated ApplicationConfig applications = 2; // List of all applications
  int64 timestamp = 3;                         // Timestamp of full config generation
}
```

#### Incremental Update Flow

```proto
message IncrementalUpdateRequest {
  IncrementalConfig incremental_config = 1; // Incremental config payload
  string target_host_id = 2;                // Host ID receiving the update
}

message IncrementalUpdateAck {
  string host_id = 1;         // Identifier for the execution host
  string version_applied = 2; // The last successfully applied version
  bool success = 3;           // True if the update was successfully applied
  string message = 4;         // Optional details (e.g., error info)
}

message IncrementalConfig {
  string version = 1;             // New version identifier
  repeated AppUpdate updates = 2; // List of applications to add/modify/delete
  int64 timestamp = 3;            // Timestamp of the incremental update
}

message AppUpdate {
  ApplicationConfig app_config = 1; // Application config for "add" or "modify" actions
  bool delete = 2;                  // Whether this entry removes the application
}
```

## Status

| Field | Description |
|-------|-------------|
| `status.resolvedImage` | Fully qualified OCI reference with resolved digest. |
| `status.conditions` | Standard Kubernetes condition list (`Ready`, `DatabaseBound`, `StoreBound`). |

## TODO

1. Specify the exact [db-operator](https://github.com/benjamin-wright/db-operator) resource kinds used to request SQL and KV instances.
2. Add HTTP route bindings (`spec.routes`) in a future pass.
3. Add scheduling bindings (`spec.schedules`) in a future pass.
