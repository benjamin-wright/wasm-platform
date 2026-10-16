# wp-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning databases, registering message subscriptions, and pushing configuration to execution hosts and the gateway.

## Application CRD

Each `Application` declares one or more deployable WASM functions and their shared runtime requirements.  Functions are listed under `spec.functions`; each function has its own module reference and trigger (exactly one of `trigger.http` or `trigger.topic`).  Application-level fields (`spec.env`, `spec.sql`, `spec.kv`) are shared across all functions.

### Examples

```yaml
# Single message-triggered function with SQL access
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: my-app
spec:
  env:
    LOG_LEVEL: info
  sql: {}   # implicit 'app' user, ALL on all tables
  functions:
    - name: handler
      module: oci://registry.example.com/my-app@sha256:<digest>
      trigger:
        topic: my-app.messages
---
# Multi-user SQL: different functions bound to different users
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: order-service
spec:
  sql:
    users:
      - name: reader
        permissions:
          - tables: [orders]
            grant: [SELECT]
      - name: writer
        permissions:
          - tables: [orders]
            grant: [SELECT, INSERT, UPDATE, DELETE]
  functions:
    - name: api
      module: oci://registry.example.com/order-api@sha256:<digest>
      sqlUser: writer
      trigger:
        http:
          path: /api/orders
          methods: [GET, POST]
    - name: reporter
      module: oci://registry.example.com/order-reporter@sha256:<digest>
      sqlUser: reader
      trigger:
        topic: orders.report
    - name: notifier
      module: oci://registry.example.com/order-notifier@sha256:<digest>
      # no sqlUser — SQL calls fail at runtime; module has no DB access
      trigger:
        topic: orders.notify
```

### Fields

**Application-level (shared across all functions):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.functions` | []FunctionSpec | yes (min 1) | List of functions in this application. |
| `spec.env` | map[string]string | no | Environment variables injected into all functions. |
| `spec.sql` | SQLSpec | no | SQL database access configuration. Absent means no SQL access. Present as `{}` provisions a single implicit `app` user with ALL privileges on all tables, and every function is implicitly bound to it. Present with `users` provisions exactly those users; functions must opt in via `sqlUser`. |
| `spec.sql.migrations` | MigrationsSpec | no | Database migrations configuration. When set, the operator creates a `PostgresMigrationSet` CR (managed by the db-operator) and withholds function activation until it reaches `Ready` phase. See [Application Authoring — Database Migrations](#database-migrations). |
| `spec.sql.migrations.artifact` | string | yes (when `migrations` set) | ORAS artifact reference to a tar+gzip of SQL migration files. Media type `application/vnd.db-operator.migrations.v1.tar+gzip`. Use an immutable tag or digest. |
| `spec.sql.migrations.targetRevision` | int64 | yes (when `migrations` set) | Numeric migration ID to converge the database to. Must match a revision present in the artifact. The db-operator runs all pending apply migrations up to and including this ID; if the database is currently at a higher revision, it runs rollbacks down to it. |
| `spec.kv` | bool | no | Key-value store access. When `true`, the operator provisions a cluster-wide `RedisDatabase` and a `RedisCredential` for the execution host if they do not already exist. When `false` or absent, no KV access is provisioned. |
| `spec.metrics` | []MetricDefinition | no | User-defined Prometheus metrics (max 50). Each metric has a `name`, `type` (`counter`/`gauge`), and optional `labels` (max 10). Names must follow `[a-zA-Z_:][a-zA-Z0-9_:]{0,63}$` and must not start with `__`. Labels must not include `app_name` or `app_namespace` (host-injected). |

**`spec.sql.users[]` (when explicit users are listed):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Logical user identifier. Referenced by function `sqlUser` fields. |
| `permissions[].tables` | []string | no | Tables to grant on. Absent means all tables. |
| `permissions[].grant` | []string | yes | PostgreSQL privileges: `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, or `ALL`. |

**Per-function (`spec.functions[]`):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Function identifier, unique within the Application. |
| `module` | string | yes | OCI reference for one raw `.wasm` layer from an approved registry. Tags are resolved once per Application generation and published as digest-pinned references. |
| `trigger.topic` | string | one of `topic`/`http` | NATS subject for `on-message` invocation. Must be unique cluster-wide. Wildcards (`*`, `>`) forbidden. Internally prefixed with `fn.` before pushing to execution hosts. |
| `trigger.http.path` | string | with `http` | URL path the gateway exposes. Must start with `/`, unique cluster-wide. |
| `trigger.http.methods` | []string | no | Allowed HTTP methods. Omit to accept all. Gateway returns `405` for unlisted methods. |
| `sqlUser` | string | no | Name of the SQL user (from `spec.sql.users`) this function uses. Ignored when `spec.sql.users` is absent/empty (implicit `app` user). When `spec.sql.users` is non-empty, functions without this field have no SQL access. |

**Topic uniqueness:** enforced cluster-wide at admission by the `ValidatingWebhookConfiguration`. A `kubectl apply` that would create a topic conflict is rejected immediately with an error message naming the conflicting topic. The Application with the oldest `creationTimestamp` owns the topic (tiebreak: lexicographically lower `namespace/name`).

**Metric name uniqueness:** enforced cluster-wide at admission by the `ValidatingWebhookConfiguration`. A `kubectl apply` that would create a metric name conflict is rejected immediately with an error message naming the conflicting metric. Ownership follows the same rule as topics.

**Identifier constraint:** when `spec.sql` is set, the application name and namespace must not contain consecutive hyphens (`--`). This constraint is enforced at admission and also re-validated by the reconciler as defense-in-depth.

**Internal NATS subjects:** `trigger.topic` functions get a `fn.` prefix; `trigger.http` functions get an auto-generated `http.<namespace>.<app-name>.<function-name>` subject. Both are invisible to the module author.

**CEL validation rules on `spec.sql`:**
- When `spec.sql.users` is non-empty, each function's `sqlUser` must name a defined user or be absent.

## Application Authoring

### Database Migrations

Applications can include versioned SQL schema migrations. When `spec.sql.migrations` is set, the operator creates a `PostgresMigrationSet` CR for the Application; the db-operator runs the migrations and reports status. The wp-operator gates function activation on the migration set reaching `Ready`, ensuring the schema is correct before traffic is served.

**File naming convention**

Migration files are paired by ID. Both files must exist for every migration:

```
migrations/
  001-create-greetings-apply.sql
  001-create-greetings-rollback.sql
  002-add-active-column-apply.sql
  002-add-active-column-rollback.sql
```

> **Footgun — never edit an applied file.** The runner tracks a content hash of every file it has applied. Editing or deleting an applied file is a hard error; the runner will refuse to run until the file is restored. Only ever add new migration files.

For the full file-format contract (ID format, hash tracking, advisory lock behaviour) see the [db-operator migrations spec](../../../db-operator/cmd/db-migrations/spec.md).

**Packaging migrations as an ORAS artifact**

Migrations are packaged as a tar+gzip of the `migrations/` directory and pushed as an OCI artifact with media type `application/vnd.db-operator.migrations.v1.tar+gzip`:

```sh
tar czf sql-hello-migrations.tar.gz -C examples/sql-hello/migrations .
oras push registry.example.com/sql-hello-migrations:v1 \
  sql-hello-migrations.tar.gz:application/vnd.db-operator.migrations.v1.tar+gzip \
  --artifact-type application/vnd.db-operator.migrations.v1.tar+gzip
```

A worked example lives in [`examples/sql-hello/Tiltfile`](../../examples/sql-hello/Tiltfile) (the `sql-hello-migrations` `custom_build` block) and [`examples/sql-hello/migrations/`](../../examples/sql-hello/migrations/).

**Reference the artifact from the Application:**

```yaml
spec:
  sql:
    migrations:
      artifact: registry.example.com/sql-hello-migrations:v1
      targetRevision: 2
```

`targetRevision` is the migration ID to converge to. Bump it when adding new migration files; the db-operator will apply the pending migrations and update the `PostgresMigrationSet` status. The wp-operator watches that status and re-pushes config when the set reaches `Ready` again.

**Immutable tags required**

> **Warning:** `spec.sql.migrations.artifact` accepts any OCI reference, but mutable tags (`:latest`, branch tags) cause silent schema skew across replicas and mask changes from the operator. Always use an immutable tag or digest (`@sha256:…`). This is not enforced by CEL — it is a deployment discipline requirement.

**Failure path**

If a migration fails, the `PostgresMigrationSet` enters `Failed` phase. The wp-operator surfaces this as `Ready: False, reason: MigrationFailed` on the Application; inspect the migration set's status conditions for details. Recovery requires fixing the SQL files, pushing a new immutable artifact tag, and updating `spec.sql.migrations.artifact` (or `targetRevision`) — both trigger a fresh migrations run via the same `PostgresMigrationSet` CR.

Rollback is not yet wired through the platform; if a schema rollback is needed, add forward-correcting SQL rather than editing existing migration files.

## PostgreSQL Identifier Derivation

The operator derives deterministic PostgreSQL identifiers from the Application's namespace and name. Both the operator and execution host use the same algorithm so the correct pool is looked up at invocation time.

**Algorithm** (hyphens replaced with underscores throughout):

| Identifier | Formula |
|---|---|
| Database name | `wasm_<namespace>__<app_name>` |
| PG username | `wasm_<namespace>__<app_name>__<user_name>` |

**Truncation:** if the result exceeds 63 characters, take the first 47 characters, append `_`, then the first 15 hex characters of the lowercase SHA-256 of the full pre-truncation string.

**Inputs with consecutive hyphens** (`--`) are rejected at reconcile time with `Ready: False, reason: InvalidIdentifier` — double hyphens would produce `____` after sanitisation, colliding with the `__` component separator.

The derived database name and per-user PG usernames are surfaced in `status.sqlDatabaseName` and `status.sqlUsernames` for observability.

## Operator Behaviour

**On create/update:**

1. The admission webhook has already enforced topic uniqueness, metric name uniqueness, and identifier constraints before the reconciler runs. The reconciler re-validates the identifier constraint as defense-in-depth.
2. Reconciles the persisted resource inventory, including credentials for removed users and migration sets removed from the spec. Cleanup blocks the desired configuration with `Ready: False, reason: ResourceCleanupPending`, retrying every 5 s.
3. Resolves immutable module identities. If `spec.sql` is set, ensures the namespace's shared `PostgresDatabase`, one credential per SQL user, and the optional migration set. Credentials must be `Ready` with verified Secrets; migrations must be `Ready` before publishing the desired configuration.
4. If `spec.kv` is set, ensures the shared `RedisDatabase` and execution-host `RedisCredential`. Once ready, verifies the Redis Secret and publishes the connection URL to connected execution hosts.
5. Publishes the Application configuration and HTTP routes. Provisioning and migration waits requeue every 5 s; ownership, binding, or existing credential policy mismatches report `ReconcileError`.

**On delete:**

1. Withdraws the Application configuration and HTTP routes and publishes deletion updates, even when resource cleanup encounters an ownership or binding conflict.
2. Reconciles cleanup from the persisted inventory, even when SQL users, SQL access, or migrations have already been removed from the spec.
3. By default (`spec.resourceCleanupPolicy: Wait`), keeps the Application finalizer until its ephemeral CRs and credential Secrets have disappeared. With `OrphanInventory`, removes the Application after persisting the release request and transferring child ownership; the independent inventory continues cleanup. Deletion uses UID and resource-version preconditions with foreground propagation and confirms cached absence against the API. Downstream finalizers and garbage collection may keep cleanup pending.

Shared PostgreSQL and Redis resources remain provisioned after the last Application stops using them. Application deletion does not purge its logical SQL database or KV prefix. See the [data ownership contract](../../docs/architecture.md#data-ownership-and-retention).

Kubernetes cleanup is not proof of backend SQL revocation: the pinned db-operator uses best-effort role deletion, so CR disappearance does not establish that old credentials or existing SQL sessions can no longer access data.

## Infrastructure CR Naming

The operator creates shared Redis resources in `POD_NAMESPACE` and shared PostgreSQL infrastructure in each requesting Application's namespace. The umbrella Helm chart owns NATS resources.

| Resource | CR name | Namespace |
|---|---|---|
| `PostgresDatabase` | `wasm-<namespace>-postgres` | application namespace |
| `RedisDatabase` | `wasm-platform-redis` | operator namespace |
| `RedisCredential` (execution host) | `wasm-platform-redis-execution-host` | operator namespace |

Shared resources have no Application owner reference. Reuse requires `app.kubernetes.io/managed-by: wp-operator`, no foreign owner, an expected specification and a matching `wasm-platform.io/resource-scope` annotation; a missing scope annotation is added only after those checks. Redis Secret use additionally requires the exact credential owner UID and expected username.

## SQL Credential Lifecycle

For each SQL user (or the synthetic `app` user when `spec.sql: {}`):

- **New `PostgresCredential` name:** `wasm-pg-<32-hex-digest>`, derived from the resource kind, namespace, Application name and UID, and logical user name as an unambiguous tuple.
- **Secret name:** `<credential-name>-creds` (created and owned by db-operator's credential CR).
- **Namespace:** the same namespace as the Application.
- **Privileges:** declared in `spec.sql.users[*].permissions`; defaults to `ALL` on all tables when permissions are absent.

When `spec.sql.migrations` is set, the operator creates one `PostgresMigrationSet` named `wasm-migrations-<32-hex-digest>` using the same identity tuple and logical name `migrations`. The migrations runner uses an internal db-operator-owned role; the wp-operator does not provision a separate `migrations` credential.

Both CR kinds carry a `ResourceInventory` controller owner reference, `wasm-platform.io/application-uid` label, and `wasm-platform.io/logical-name` annotation. The inventory is namespaced with the Application, has an immutable `spec.applicationRef` identifying its namespace/name/UID, and has no Application owner reference. Its `spec.resources` records up to 128 intended bindings, observed UIDs, retention flags, and `Active` or `Release` dispositions before resources are used. The inventory finalizer preserves unresolved cleanup, including credential Secrets whose parent CR has already disappeared.

The inventory controller continues cleanup independently after Application deletion and removes a released inventory only once its originating Application and ephemeral obligations are gone. A same-name Application with SQL waits for older inventories to finish cleanup before provisioning new credentials. Shared infrastructure is retained. Inventory status reports observed progress; Application `status.resourceInventory` is a derived summary. Clearing either status does not discard resource obligations.

The operator verifies ownership and database, username, and Secret bindings before reuse, updates, or deletion. SQL Secret reads require the exact credential owner UID and expected `PGUSER`. Existing Application-owned children and recorded status bindings are verified and transferred to their inventory without changing child names or UIDs.

Existing unowned migration sets with the deterministic name `wasm-<namespace>-<app_name>-migrations` and matching Application namespace/name labels can be adopted after their database binding and observed UID are verified. Unowned credentials require an administrator to attest the current Application UID with `wasm-platform.io/legacy-application-uid`; credentials for users absent from the current spec also need `wasm-platform.io/logical-name`. Attestation does not bypass owner, UID, or binding checks. Adopted resources retain their existing names; unverified resources are not adopted or deleted.

The operator does not push an `ApplicationConfig` to execution hosts until all `PostgresCredential` CRs for the Application have reached `Ready` phase **and**, if `spec.sql.migrations` is set, the `PostgresMigrationSet` has reached `Ready` phase.

## Config API

gRPC `ConfigSync` service for execution hosts. Schema: [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto).

- **`RequestFullConfig`** — full snapshot on startup or desync.
- **`PushIncrementalUpdate`** — bidirectional stream for ongoing deltas. Host acks each delta; on failure, falls back to `RequestFullConfig`.

The operator also exposes a `GatewayRoutes` service on the same gRPC port for the gateway's route table sync.

Both services require `authorization: Bearer <token>` metadata with a short-lived projected Kubernetes ServiceAccount token for audience `wasm-platform`. TokenReview verifies the token and audience. Only `EXECUTION_HOST_SERVICE_ACCOUNT` in `POD_NAMESPACE` can receive credential-bearing ConfigSync data; only `GATEWAY_SERVICE_ACCOUNT` in that namespace can receive GatewayRoutes data. The two accounts must differ. Client-supplied host and gateway IDs do not grant access.

Snapshots and streams return `Unavailable` until the elected leader has processed its startup Application inventory. Pending startup provisioning or reconciliation failures keep publication unready. `/readyz` reflects that authority; standby operators keep their admission webhook available through its separate Service. Leadership loss closes active streams and rejects new configuration requests.

Streams close at token expiry or after five minutes, and TokenReview rechecks active tokens every 30 seconds with a five-second API deadline. Clients must reconnect with their current projected token. Authentication failures return fixed error messages without token or API-response details. In-cluster transport is protected by the platform's Istio ambient strict mTLS policy.

## Module Registry Policy

`APPROVED_REGISTRIES` is required and lists exact lowercase registry authorities (`hostname[:port]`), separated by commas without whitespace. Module pulls use anonymous HTTPS requests with certificate validation. `LOCAL_PLAINTEXT_REGISTRIES` optionally lists an approved subset using `localhost`, a `.localhost` hostname, or a loopback IP; those registries use HTTP. Redirects and delegated authentication are rejected.

Resolution verifies a SHA-256 OCI image manifest and exactly one raw Wasm layer descriptor. Manifest responses are limited to 1 MiB and layer descriptors to 8 bytes–16 MiB. Layer media types are `application/wasm`, `application/vnd.wasm.content.layer.v1+wasm`, or `application/octet-stream`; external layer URLs are rejected. Registry operations have a 10-second deadline.

ORAS raw Wasm layers labelled `application/vnd.oci.image.layer.v1.tar` are also accepted when the manifest declares `artifactType: application/vnd.wasm.content.layer.v1+wasm`.

Resolved identities are persisted before configuration publication and reused for the same Application generation after reconciliation or operator restart. A spec change creates a new generation and resolves its requested references again. Within one operator process, resolution failure preserves its published configuration and marks the desired Application unready.

## Generated Code

```sh
make generate   # from components/wp-operator/
```

Requires `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`, `controller-gen`.

Generates: gRPC stubs → `internal/grpc/configsync/`, CRD deepcopy → `api/v1alpha1/`. All marked `DO NOT EDIT`.

## Validating Webhook

wp-operator exposes a validating admission webhook on port 9443, registered as `wasm-platform-application-validator` in Kubernetes. TLS is provided by a cert-manager self-signed `ClusterIssuer` (`wasm-platform-selfsigned`) and a `Certificate` (`wp-operator-webhook-tls`) whose Secret is mounted into the operator container.

**Scope:** fires on `Application` create and update.

**Checks performed:**

| Check | Description |
|-------|-------------|
| `InvalidIdentifier` | When `spec.sql` is set, rejects names containing consecutive hyphens (`--`). |
| `TopicConflict` | Rejects any function whose `trigger.topic` is already owned by another Application. |
| `MetricConflict` | Rejects any metric name already owned by another Application. |

**Failure policy:** `Fail` — if the webhook is unavailable, `Application` create and update requests are blocked. Existing execution hosts continue serving the last-known configuration while the operator is down; in-flight traffic is unaffected.

## Status

| Condition | Description |
|-----------|-------------|
| `Ready` | `True` when config is pushed to all hosts. `False` while provisioning or on error. |
| `DatabaseProvisioningPending` | Set while the per-namespace `PostgresDatabase` CR is not yet `Ready`. |
| `KVProvisioningPending` | Set while the `RedisDatabase` or `RedisCredential` CR is not yet `Ready` (only when `spec.kv` is set). |
| `ResourceCleanupPending` | Set while removed credential or migration bindings await CR and Secret cleanup. |
| `ReconcileError` | Reconciliation failed, including an ownership, inventory, or resource binding conflict. |
| `InvalidIdentifier` | Set when namespace or app name contains consecutive hyphens, preventing PG identifier derivation. |
| `MigrationsRunning` | Set while the `PostgresMigrationSet` is in `Pending` or `Running` phase. |
| `MigrationFailed` | Set when the `PostgresMigrationSet` reports `Failed`; inspect that CR's status conditions. Update `spec.sql.migrations.artifact` or `targetRevision` to trigger a new run. |

**Status fields:**

| Field | Description |
|-------|-------------|
| `status.sqlDatabaseName` | Derived PostgreSQL database name. Populated when `spec.sql` is set. |
| `status.sqlUsernames` | Derived PostgreSQL usernames, one per provisioned user. Populated when `spec.sql` is set. |
| `status.resolvedModules` | Current desired generation's per-function requested reference, pinned module reference, manifest digest and raw Wasm digest. |
| `status.resourceInventory` | Derived summary of the independent ResourceInventory spec; not the cleanup authority. |
