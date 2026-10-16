package controller

import (
	"context"
	"fmt"
	"net/url"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/artifacts"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/authority"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/routestore"
)

const applicationFinalizer = "wasm-platform.io/application-protection"

// topicIndexField is the cache field index key for function topics within an Application.
const topicIndexField = "spec.functions.topic"

// metricNameIndexField is the cache field index key for metric names within an Application.
const metricNameIndexField = "spec.metrics.name"

// Infrastructure CR names. Postgres CRs (PostgresDatabase, PostgresCredential,
// PostgresMigrationSet) are created in the application's namespace. Redis CRs
// are created in the operator's own namespace (Config.PostgresCredentialNamespace).
const (
	redisDatabaseName       = "wasm-platform-redis"
	redisCredExecHostName   = "wasm-platform-redis-execution-host"
	redisExecHostSecretName = "execution-host-redis-credentials"
)

func postgresDatabaseCRName(appNamespace string) string {
	return fmt.Sprintf("wasm-%s-postgres", appNamespace)
}

// Config holds environment-driven settings injected into the reconciler at
// startup. Values are sourced from env vars (see cmd/main.go).
type Config struct {
	// PostgresCredentialNamespace is the namespace in which Redis infrastructure
	// CRs (RedisDatabase, RedisCredential) are created. Defaults to POD_NAMESPACE.
	// Postgres CRs are always created in the application's own namespace.
	PostgresCredentialNamespace string

	// PostgresVersion is the PostgreSQL version for provisioned PostgresDatabase
	// CRs (e.g. "16").
	PostgresVersion string
	// PostgresStorageSize is the PVC size for PostgresDatabase CRs (e.g. "1Gi").
	PostgresStorageSize string

	// RedisStorageSize is the PVC size for RedisDatabase CRs (e.g. "1Gi").
	RedisStorageSize string
}

// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=list;watch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgrescredentials,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgresdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgresmigrationsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=redisdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=rediscredentials,verbs=get;list;watch;create;update;patch;delete
type ApplicationReconciler struct {
	client.Client
	APIReader  client.Reader
	Scheme     *runtime.Scheme
	Store      *configstore.Store
	RouteStore *routestore.Store
	Config     Config
	Artifacts  artifacts.Resolver
	Authority  *authority.Authority
}

func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var app wasmplatformv1alpha1.Application
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierrors.IsNotFound(err) {
			if r.Authority != nil {
				r.Authority.Reconciled(req.NamespacedName, "", 0)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !app.DeletionTimestamp.IsZero() {
		result, err := r.reconcileDelete(ctx, &app)
		r.recordInitialized(&app, result, err)
		return result, err
	}

	if !controllerutil.ContainsFinalizer(&app, applicationFinalizer) {
		controllerutil.AddFinalizer(&app, applicationFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	result, err := r.reconcileUpsert(ctx, &app)
	if err != nil {
		logger.Error(err, "reconcile failed")
		r.setReadyCondition(&app, metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, &app)
	}
	r.recordInitialized(&app, result, err)
	return result, err
}

func (r *ApplicationReconciler) recordInitialized(app *wasmplatformv1alpha1.Application, result ctrl.Result, err error) {
	if r.Authority != nil && err == nil && !result.Requeue && result.RequeueAfter == 0 {
		r.Authority.Reconciled(client.ObjectKeyFromObject(app), app.UID, app.Generation)
	}
}

func (r *ApplicationReconciler) reconcileDelete(ctx context.Context, app *wasmplatformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	r.Store.Delete(key)
	r.Store.BroadcastUpdate(buildDeleteUpdate(r.Store, app))

	oldRoutes := r.RouteStore.Get(key)
	if len(oldRoutes) > 0 {
		r.RouteStore.Delete(key)
		r.RouteStore.BroadcastUpdate(buildRouteDeleteUpdate(oldRoutes))
	}

	pending, err := r.reconcileResourceInventory(ctx, app, true)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(app, applicationFinalizer)
	if err := r.Update(ctx, app); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("application deleted from config store", "name", app.Name, "namespace", app.Namespace)
	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) reconcileUpsert(ctx context.Context, app *wasmplatformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	pending, err := r.reconcileResourceInventory(ctx, app, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		r.setReadyCondition(app, metav1.ConditionFalse, "ResourceCleanupPending", "Waiting for removed resource bindings to finish cleanup.")
		if err := r.Status().Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	modules, err := r.resolveModules(ctx, app)
	if err != nil {
		return ctrl.Result{}, err
	}

	// ── Step 1: Ensure Postgres infrastructure (if spec.sql is set) ─────────────
	if app.Spec.SQL != nil {
		if err := ValidatePGInputs(app.Namespace, app.Name); err != nil {
			msg := fmt.Sprintf("cannot derive PG identifiers: %s", err)
			r.setReadyCondition(app, metav1.ConditionFalse, "InvalidIdentifier", msg)
			_ = r.Status().Update(ctx, app)
			return ctrl.Result{}, nil
		}

		pgdbRequeue, err := r.reconcilePostgresDatabase(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pgdbRequeue {
			r.setReadyCondition(app, metav1.ConditionFalse, "DatabaseProvisioningPending", "Waiting for PostgresDatabase to reach Ready phase.")
			_ = r.Status().Update(ctx, app)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// ── Step 2: Ensure Redis infrastructure (if spec.kv is set) ─────────────────
	if app.Spec.KV {
		kvRequeue, err := r.reconcileRedisDatabase(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if kvRequeue {
			r.setReadyCondition(app, metav1.ConditionFalse, "KVProvisioningPending", "Waiting for RedisDatabase and credentials to reach Ready phase.")
			_ = r.Status().Update(ctx, app)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// ── Step 3: Build function configs ───────────────────────────────────────────
	functions := make([]*configsync.FunctionConfig, 0, len(app.Spec.Functions))
	for i := range app.Spec.Functions {
		fn := &app.Spec.Functions[i]
		topic := internalFunctionTopic(app, fn)
		fnCfg := &configsync.FunctionConfig{
			Name:      fn.Name,
			ModuleRef: modules[fn.Name].ModuleRef,
			Topic:     &topic,
		}
		if fn.Trigger.HTTP != nil {
			fnCfg.WorldType = configsync.WorldType_WORLD_TYPE_HTTP
			fnCfg.HttpConfig = &configsync.HttpConfig{
				Path:    fn.Trigger.HTTP.Path,
				Methods: fn.Trigger.HTTP.MethodStrings(),
			}
		} else {
			fnCfg.WorldType = configsync.WorldType_WORLD_TYPE_MESSAGE
		}
		if app.Spec.SQL != nil {
			if pgUsername := sqlUsernameForFunction(app, fn); pgUsername != "" {
				fnCfg.SqlUsername = &pgUsername
			}
		}
		functions = append(functions, fnCfg)
	}

	cfg := &configsync.ApplicationConfig{
		Name:      app.Name,
		Namespace: app.Namespace,
		Functions: functions,
		Env:       app.Spec.Env,
		KeyValue:  app.Spec.KV,
	}
	cfg.Metrics = buildMetricDefs(app.Spec.Metrics)

	if app.Spec.SQL != nil {
		sqlUsers, requeue, err := r.reconcileSQLBinding(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		if app.Spec.SQL.Migrations != nil {
			done, failMsg, err := r.reconcileMigrationSet(ctx, app)
			if err != nil {
				return ctrl.Result{}, err
			}
			if failMsg != "" {
				r.setReadyCondition(app, metav1.ConditionFalse, "MigrationFailed", failMsg)
				_ = r.Status().Update(ctx, app)
				return ctrl.Result{}, nil
			}
			if !done {
				r.setReadyCondition(app, metav1.ConditionFalse, "MigrationsRunning", "Waiting for PostgresMigrationSet to reach Ready phase.")
				_ = r.Status().Update(ctx, app)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}

		cfg.SqlUsers = sqlUsers

		app.Status.SQLDatabaseName = PGDatabaseName(app.Namespace, app.Name)
		userNames := sqlUsersForApp(app.Spec.SQL)
		pgUsernames := make([]string, len(userNames))
		for i, u := range userNames {
			pgUsernames[i] = PGUsername(app.Namespace, app.Name, u)
		}
		app.Status.SQLUsernames = pgUsernames
	} else {
		app.Status.SQLDatabaseName = ""
		app.Status.SQLUsernames = nil
	}

	if r.Store.Set(key, cfg) {
		r.Store.BroadcastUpdate(buildUpsertUpdate(r.Store, cfg))
	}

	var httpRoutes []*routestore.RouteConfig
	for i := range app.Spec.Functions {
		fn := &app.Spec.Functions[i]
		if fn.Trigger.HTTP != nil {
			httpRoutes = append(httpRoutes, &routestore.RouteConfig{
				Path:        fn.Trigger.HTTP.Path,
				Methods:     fn.Trigger.HTTP.MethodStrings(),
				NatsSubject: internalFunctionTopic(app, fn),
			})
		}
	}
	if r.RouteStore.Set(key, httpRoutes) {
		r.RouteStore.BroadcastUpdate(buildRouteUpsertUpdate(httpRoutes))
	}

	r.setReadyCondition(app, metav1.ConditionTrue, "ConfigPushed", "Application config pushed to execution hosts.")
	if err := r.Status().Update(ctx, app); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	logger.Info("application config pushed", "name", app.Name, "namespace", app.Namespace)
	return ctrl.Result{}, nil
}

// Returns (true, nil) when pending.
func (r *ApplicationReconciler) reconcilePostgresDatabase(ctx context.Context, app *wasmplatformv1alpha1.Application) (bool, error) {
	entry, err := r.sharedResourceIntent(ctx, app, "PostgresDatabase", app.Namespace, postgresDatabaseCRName(app.Namespace))
	if err != nil {
		return false, err
	}
	pgdb, err := r.resources().EnsurePostgres(ctx, app.Namespace, r.Config.PostgresVersion, r.Config.PostgresStorageSize, entry.UID)
	if err != nil {
		return false, err
	}
	if err := r.recordResourceUID(ctx, app, entry, pgdb); err != nil {
		return false, err
	}
	return pgdb.Status.Phase != dboperator.DatabasePhaseReady, nil
}

func (r *ApplicationReconciler) reconcileRedisDatabase(ctx context.Context, app *wasmplatformv1alpha1.Application) (bool, error) {
	resources := r.resources()
	namespace := r.Config.PostgresCredentialNamespace
	entry, err := r.sharedResourceIntent(ctx, app, "RedisDatabase", namespace, redisDatabaseName)
	if err != nil {
		return false, err
	}
	database, err := resources.EnsureRedis(ctx, namespace, r.Config.RedisStorageSize, entry.UID)
	if err != nil {
		return false, err
	}
	if err := r.recordResourceUID(ctx, app, entry, database); err != nil {
		return false, err
	}
	if database.Status.Phase != dboperator.RedisDatabasePhaseReady {
		return true, nil
	}
	entry, err = r.sharedResourceIntent(ctx, app, "RedisCredential", namespace, redisCredExecHostName)
	if err != nil {
		return false, err
	}
	credential, err := resources.EnsureRedisCredential(ctx, namespace, entry.UID)
	if err != nil {
		return false, err
	}
	if err := r.recordResourceUID(ctx, app, entry, credential); err != nil {
		return false, err
	}
	if credential.Status.Phase != dboperator.RedisCredentialPhaseReady {
		return true, nil
	}
	secret, err := resources.RedisSecret(ctx, credential)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	host, port := string(secret.Data["REDIS_HOST"]), string(secret.Data["REDIS_PORT"])
	if host == "" || port == "" || len(secret.Data["REDIS_PASSWORD"]) == 0 {
		return true, nil
	}
	connection := (&url.URL{Scheme: "redis", User: url.UserPassword(string(secret.Data["REDIS_USERNAME"]), string(secret.Data["REDIS_PASSWORD"])), Host: host + ":" + port}).String()
	if r.Store.SetRedisConfig(&configsync.RedisConnectionConfig{Url: connection}) {
		r.Store.BroadcastUpdate(buildInfraUpdate(r.Store))
	}
	return false, nil
}

// Returns (nil, true, nil) when any credential or Secret is not yet ready.
func (r *ApplicationReconciler) reconcileSQLBinding(ctx context.Context, app *wasmplatformv1alpha1.Application) ([]*configsync.SqlUserConfig, bool, error) {
	resources, err := r.applicationResources(ctx, app)
	if err != nil {
		return nil, false, err
	}
	permissions := sqlPermissionsForApp(app.Spec.SQL)
	var sqlUsers []*configsync.SqlUserConfig
	for _, user := range sqlUsersForApp(app.Spec.SQL) {
		entry, err := r.applicationResource(ctx, app, "PostgresCredential", user)
		if err != nil {
			return nil, false, err
		}
		desired := buildPostgresCredentialForUser(entry.Name, entry.Namespace, entry.SecretName, entry.Username, entry.Database, permissions[user], entry.DatabaseRef)
		credential, err := resources.EnsureCredential(ctx, app, entry, desired.Spec)
		if err != nil {
			return nil, false, err
		}
		if err := r.recordResourceUID(ctx, app, entry, credential); err != nil {
			return nil, false, err
		}
		if credential.Status.Phase != dboperator.CredentialPhaseReady {
			return nil, true, nil
		}
		secret, err := resources.CredentialSecret(ctx, credential)
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		host, port := string(secret.Data["PGHOST"]), string(secret.Data["PGPORT"])
		if host == "" || port == "" || len(secret.Data["PGPASSWORD"]) == 0 {
			return nil, true, nil
		}
		connection := (&url.URL{Scheme: "postgres", User: url.UserPassword(string(secret.Data["PGUSER"]), string(secret.Data["PGPASSWORD"])), Host: host + ":" + port, Path: "/" + entry.Database}).String()
		sqlUsers = append(sqlUsers, &configsync.SqlUserConfig{Username: entry.Username, ConnectionUrl: connection})
	}
	return sqlUsers, false, nil
}

// ── SQL helpers ───────────────────────────────────────────────────────────────

// reconcileMigrationSet ensures a PostgresMigrationSet CR exists for the Application
// and reports whether it has reached Ready phase. On failure it returns a non-empty
// failMsg and no automatic requeue is performed — the user must correct the artifact.
func (r *ApplicationReconciler) reconcileMigrationSet(ctx context.Context, app *wasmplatformv1alpha1.Application) (bool, string, error) {
	entry, err := r.applicationResource(ctx, app, "PostgresMigrationSet", "migrations")
	if err != nil {
		return false, "", err
	}
	desired := dboperator.PostgresMigrationSetSpec{DatabaseRef: entry.DatabaseRef, Database: entry.Database, Artifact: app.Spec.SQL.Migrations.Artifact, TargetRevision: app.Spec.SQL.Migrations.TargetRevision}
	resources, err := r.applicationResources(ctx, app)
	if err != nil {
		return false, "", err
	}
	migration, err := resources.EnsureMigration(ctx, app, entry, desired)
	if err != nil {
		return false, "", err
	}
	if err := r.recordResourceUID(ctx, app, entry, migration); err != nil {
		return false, "", err
	}
	switch migration.Status.Phase {
	case dboperator.MigrationSetPhaseReady:
		return true, "", nil
	case dboperator.MigrationSetPhaseFailed:
		return false, "PostgresMigrationSet failed; inspect its status conditions.", nil
	default:
		return false, "", nil
	}
}

// sqlUsernameForFunction resolves the derived PG username for a function based on
// spec.sql. Implicit mode (spec.sql.users absent/empty): all functions are bound to the
// 'app' user. Explicit mode: only functions with sqlUser set get a username; functions
// without sqlUser return "" (no SQL access).
func sqlUsernameForFunction(app *wasmplatformv1alpha1.Application, fn *wasmplatformv1alpha1.FunctionSpec) string {
	if len(app.Spec.SQL.Users) == 0 {
		return PGUsername(app.Namespace, app.Name, "app")
	}
	if fn.SQLUser != nil {
		return PGUsername(app.Namespace, app.Name, *fn.SQLUser)
	}
	return ""
}

// sqlUsersForApp returns the list of logical SQL user names for an Application.
// When spec.sql.users is absent or empty, a single implicit 'app' user is returned.
func sqlUsersForApp(sql *wasmplatformv1alpha1.SQLSpec) []string {
	if len(sql.Users) == 0 {
		return []string{"app"}
	}
	names := make([]string, len(sql.Users))
	for i, u := range sql.Users {
		names[i] = u.Name
	}
	return names
}

// sqlPermissionsForApp returns a map from user name to the DatabasePermissionEntry
// that should be used in the PostgresCredential spec.
// Absent or empty spec.sql.users → all users get ALL on the app's database.
func sqlPermissionsForApp(sql *wasmplatformv1alpha1.SQLSpec) map[string][]wasmplatformv1alpha1.SQLTablePermission {
	out := make(map[string][]wasmplatformv1alpha1.SQLTablePermission)
	if len(sql.Users) == 0 {
		// Implicit 'app' user: ALL on all tables (empty Tables slice → all).
		out["app"] = []wasmplatformv1alpha1.SQLTablePermission{
			{Grant: []wasmplatformv1alpha1.SQLGrant{wasmplatformv1alpha1.SQLGrantAll}},
		}
		return out
	}
	for _, u := range sql.Users {
		perms := u.Permissions
		if len(perms) == 0 {
			perms = []wasmplatformv1alpha1.SQLTablePermission{
				{Grant: []wasmplatformv1alpha1.SQLGrant{wasmplatformv1alpha1.SQLGrantAll}},
			}
		}
		out[u.Name] = perms
	}
	return out
}

func buildPostgresCredentialForUser(
	name, namespace, secretName, pgUsername, dbName string,
	tablePerms []wasmplatformv1alpha1.SQLTablePermission,
	pgdbRef string,
) *dboperator.PostgresCredential {
	perms := make([]dboperator.DatabasePermission, 0)
	for _, tp := range tablePerms {
		for _, g := range tp.Grant {
			perms = append(perms, dboperator.DatabasePermission(g))
		}
	}
	return &dboperator.PostgresCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: dboperator.PostgresCredentialSpec{
			DatabaseRef: pgdbRef,
			Username:    pgUsername,
			SecretName:  secretName,
			Permissions: []dboperator.DatabasePermissionEntry{
				{
					Databases:   []string{dbName},
					Permissions: perms,
				},
			},
		},
	}
}
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&wasmplatformv1alpha1.Application{},
		topicIndexField,
		func(obj client.Object) []string {
			app := obj.(*wasmplatformv1alpha1.Application)
			var topics []string
			for _, fn := range app.Spec.Functions {
				if fn.Trigger.Topic != "" {
					topics = append(topics, fn.Trigger.Topic)
				}
			}
			return topics
		},
	); err != nil {
		return fmt.Errorf("registering topic field index: %w", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&wasmplatformv1alpha1.Application{},
		metricNameIndexField,
		func(obj client.Object) []string {
			app := obj.(*wasmplatformv1alpha1.Application)
			var names []string
			for _, m := range app.Spec.Metrics {
				names = append(names, m.Name)
			}
			return names
		},
	); err != nil {
		return fmt.Errorf("registering metric name field index: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&wasmplatformv1alpha1.Application{}).
		Watches(&wasmplatformv1alpha1.ResourceInventory{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			inventory := obj.(*wasmplatformv1alpha1.ResourceInventory)
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: inventory.Spec.ApplicationRef.Namespace, Name: inventory.Spec.ApplicationRef.Name}}}
		})).
		Owns(&dboperator.PostgresCredential{}).
		Watches(&dboperator.PostgresCredential{}, handler.EnqueueRequestsFromMapFunc(r.applicationsForInventoryResource)).
		Watches(&dboperator.PostgresMigrationSet{}, handler.EnqueueRequestsFromMapFunc(r.applicationsForInventoryResource)).
		Owns(&dboperator.PostgresMigrationSet{}).
		Watches(
			&dboperator.PostgresMigrationSet{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				labels := obj.GetLabels()
				ns := labels["wasm-platform.io/app-namespace"]
				name := labels["wasm-platform.io/app-name"]
				if ns == "" || name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
			}),
		).
		// Re-enqueue Applications in the affected namespace when a PostgresDatabase changes.
		Watches(
			&dboperator.PostgresDatabase{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				return r.applicationsForPostgresDatabase(ctx, obj)
			}),
		).
		// Re-enqueue all Applications with spec.kv when a RedisDatabase changes.
		Watches(
			&dboperator.RedisDatabase{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
				return r.allKVApplicationRequests(ctx)
			}),
		).
		// Re-enqueue all Applications with spec.kv when a RedisCredential changes.
		Watches(
			&dboperator.RedisCredential{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
				return r.allKVApplicationRequests(ctx)
			}),
		).
		Complete(r)
}

func (r *ApplicationReconciler) allKVApplicationRequests(ctx context.Context) []reconcile.Request {
	var list wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.KV {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: list.Items[i].Namespace,
					Name:      list.Items[i].Name,
				},
			})
		}
	}
	return reqs
}

// The CR name format is "wasm-<namespace>-postgres".
func (r *ApplicationReconciler) applicationsForPostgresDatabase(ctx context.Context, obj client.Object) []reconcile.Request {
	crName := obj.GetName()
	const prefix = "wasm-"
	const suffix = "-postgres"
	if len(crName) <= len(prefix)+len(suffix) {
		return nil
	}
	if crName[:len(prefix)] != prefix || crName[len(crName)-len(suffix):] != suffix {
		return nil
	}
	namespace := crName[len(prefix) : len(crName)-len(suffix)]

	var list wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.SQL != nil {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: list.Items[i].Namespace,
					Name:      list.Items[i].Name,
				},
			})
		}
	}
	return reqs
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (r *ApplicationReconciler) setReadyCondition(app *wasmplatformv1alpha1.Application, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: app.Generation,
	})
}

// ── topic ownership ───────────────────────────────────────────────────────────

// findTopicOwner returns the Application that rightfully owns the given topic,
// or nil if app itself is the rightful owner (or the sole claimant). The owner
// is determined by oldest creationTimestamp; ties break on namespace/name
// lexicographic order (lower sorts first).
func findTopicOwner(ctx context.Context, c client.Client, topic string, self *wasmplatformv1alpha1.Application) (*wasmplatformv1alpha1.Application, error) {
	var list wasmplatformv1alpha1.ApplicationList
	if err := c.List(ctx, &list, client.MatchingFields{topicIndexField: topic}); err != nil {
		return nil, fmt.Errorf("listing applications for topic %q: %w", topic, err)
	}

	var owner *wasmplatformv1alpha1.Application
	for i := range list.Items {
		app := &list.Items[i]
		if topicOwnerLess(app, owner) {
			owner = app
		}
	}

	if owner == nil || (owner.Namespace == self.Namespace && owner.Name == self.Name) {
		return nil, nil // self is the rightful owner
	}
	return owner, nil
}

// topicOwnerLess reports whether a should rank before b in topic ownership
// order (older timestamp wins; ties broken by namespace/name lex order).
// b == nil is treated as "no current candidate", so a always wins.
func topicOwnerLess(a, b *wasmplatformv1alpha1.Application) bool {
	if b == nil {
		return true
	}
	aTS := a.CreationTimestamp.Time
	bTS := b.CreationTimestamp.Time
	if aTS.Before(bTS) {
		return true
	}
	if aTS.Equal(bTS) {
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	}
	return false
}

// ── metric name ownership ─────────────────────────────────────────────────────

// Ownership follows the same tiebreak as topics: oldest creationTimestamp wins;
// ties break on namespace/name lexicographic order.
func findMetricOwner(ctx context.Context, c client.Client, metricName string, self *wasmplatformv1alpha1.Application) (*wasmplatformv1alpha1.Application, error) {
	var list wasmplatformv1alpha1.ApplicationList
	if err := c.List(ctx, &list, client.MatchingFields{metricNameIndexField: metricName}); err != nil {
		return nil, fmt.Errorf("listing applications for metric %q: %w", metricName, err)
	}

	var owner *wasmplatformv1alpha1.Application
	for i := range list.Items {
		app := &list.Items[i]
		if topicOwnerLess(app, owner) {
			owner = app
		}
	}

	if owner == nil || (owner.Namespace == self.Namespace && owner.Name == self.Name) {
		return nil, nil // self is the rightful owner
	}
	return owner, nil
}

func buildUpsertUpdate(store *configstore.Store, cfg *configsync.ApplicationConfig) *configsync.IncrementalConfig {
	now := time.Now().UnixMilli()
	return &configsync.IncrementalConfig{
		Version:   fmt.Sprintf("%d", now),
		Updates:   []*configsync.AppUpdate{{AppConfig: cfg, Delete: false}},
		Timestamp: now,
		Redis:     store.RedisConfig(),
	}
}

func buildDeleteUpdate(store *configstore.Store, app *wasmplatformv1alpha1.Application) *configsync.IncrementalConfig {
	now := time.Now().UnixMilli()
	return &configsync.IncrementalConfig{
		Version: fmt.Sprintf("%d", now),
		Updates: []*configsync.AppUpdate{
			{
				AppConfig: &configsync.ApplicationConfig{
					Name:      app.Name,
					Namespace: app.Namespace,
				},
				Delete: true,
			},
		},
		Timestamp: now,
		Redis:     store.RedisConfig(),
	}
}

// buildInfraUpdate constructs an incremental config carrying only the current
// infrastructure connection state (no app updates).  Used when NATS or Redis
// presence changes independently of any application reconcile.
func buildInfraUpdate(store *configstore.Store) *configsync.IncrementalConfig {
	now := time.Now().UnixMilli()
	return &configsync.IncrementalConfig{
		Version:   fmt.Sprintf("%d", now),
		Timestamp: now,
		Redis:     store.RedisConfig(),
	}
}

// internalFunctionTopic returns the fully-prefixed NATS subject for a function.
// Message-triggered functions use the "fn." prefix; HTTP-triggered functions
// use "http.<namespace>.<app-name>.<function-name>".
func internalFunctionTopic(app *wasmplatformv1alpha1.Application, fn *wasmplatformv1alpha1.FunctionSpec) string {
	if fn.Trigger.Topic != "" {
		return "fn." + fn.Trigger.Topic
	}
	return fmt.Sprintf("http.%s.%s.%s", app.Namespace, app.Name, fn.Name)
}

func buildRouteUpsertUpdate(cfgs []*routestore.RouteConfig) *routestore.RouteUpdateBatch {
	now := time.Now().UnixMilli()
	updates := make([]*routestore.RouteUpdate, len(cfgs))
	for i, cfg := range cfgs {
		updates[i] = &routestore.RouteUpdate{Config: cfg, Delete: false}
	}
	return &routestore.RouteUpdateBatch{
		Version:   fmt.Sprintf("%d", now),
		Updates:   updates,
		Timestamp: now,
	}
}

func buildMetricDefs(metrics []wasmplatformv1alpha1.MetricDefinition) []*configsync.MetricDefinition {
	defs := make([]*configsync.MetricDefinition, len(metrics))
	for i := range metrics {
		m := &metrics[i]
		mt := configsync.MetricType_METRIC_TYPE_COUNTER
		if m.Type == wasmplatformv1alpha1.MetricTypeGauge {
			mt = configsync.MetricType_METRIC_TYPE_GAUGE
		}
		defs[i] = &configsync.MetricDefinition{
			Name:      m.Name,
			Type:      mt,
			LabelKeys: m.Labels,
		}
	}
	return defs
}

func buildRouteDeleteUpdate(cfgs []*routestore.RouteConfig) *routestore.RouteUpdateBatch {
	now := time.Now().UnixMilli()
	updates := make([]*routestore.RouteUpdate, len(cfgs))
	for i, cfg := range cfgs {
		updates[i] = &routestore.RouteUpdate{Config: cfg, Delete: true}
	}
	return &routestore.RouteUpdateBatch{
		Version:   fmt.Sprintf("%d", now),
		Updates:   updates,
		Timestamp: now,
	}
}
