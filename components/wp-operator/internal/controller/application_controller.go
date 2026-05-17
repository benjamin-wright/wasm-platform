package controller

import (
	"context"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/routestore"
)

const applicationFinalizer = "wasm-platform.io/application-protection"

// topicIndexField is the cache field index key for function topics within an Application.
// Used by findTopicOwner and the topic-peer watch handler to avoid full-list scans.
const topicIndexField = "spec.functions.topic"

// metricNameIndexField is the cache field index key for metric names within an Application.
// Used by findMetricOwner and the metric-peer watch handler to avoid full-list scans.
const metricNameIndexField = "spec.metrics.name"

// Infrastructure CR names. All infra CRs are created in the operator's own namespace
// (PostgresCredentialNamespace) unless otherwise noted.
const (
	// natsClusterName is the name of the cluster-wide NatsCluster CR.
	natsClusterName = "wasm-platform-nats"
	// natsAccountExecHostName is the NatsAccount CR for execution-host users.
	natsAccountExecHostName = "wasm-platform-nats-execution-host"
	// natsAccountGatewayName is the NatsAccount CR for gateway users.
	natsAccountGatewayName = "wasm-platform-nats-gateway"
	// natsExecHostSecretName is the Secret produced by the execution-host NatsAccount.
	natsExecHostSecretName = "execution-host-nats-credentials"
	// natsGatewaySecretName is the Secret produced by the gateway NatsAccount.
	natsGatewaySecretName = "gateway-nats-credentials"

	// redisDatabaseName is the name of the cluster-wide RedisDatabase CR.
	redisDatabaseName = "wasm-platform-redis"
	// redisCredExecHostName is the RedisCredential CR for execution-host.
	redisCredExecHostName = "wasm-platform-redis-execution-host"
	// redisExecHostSecretName is the Secret produced by the execution-host RedisCredential.
	redisExecHostSecretName = "execution-host-redis-credentials"
)

// postgresDatabaseCRName returns the name of the PostgresDatabase CR for a namespace.
// The CR is created in the operator namespace.
func postgresDatabaseCRName(appNamespace string) string {
	return fmt.Sprintf("wasm-%s-postgres", appNamespace)
}

// Config holds environment-driven settings injected into the reconciler at
// startup. Values are sourced from env vars (see cmd/main.go).
type Config struct {
	// PostgresCredentialNamespace is the namespace in which PostgresCredential
	// CRs, PostgresDatabase CRs, NatsCluster CRs, and RedisDatabase CRs are
	// created. Defaults to POD_NAMESPACE.
	PostgresCredentialNamespace string

	// NatsVersion is the NATS server version for provisioned NatsCluster CRs
	// (e.g. "2.10").
	NatsVersion string
	// NatsJetStreamStorageSize is the JetStream PVC size for NatsCluster CRs
	// (e.g. "1Gi").
	NatsJetStreamStorageSize string

	// PostgresVersion is the PostgreSQL version for provisioned PostgresDatabase
	// CRs (e.g. "16").
	PostgresVersion string
	// PostgresStorageSize is the PVC size for PostgresDatabase CRs (e.g. "1Gi").
	PostgresStorageSize string

	// RedisStorageSize is the PVC size for RedisDatabase CRs (e.g. "1Gi").
	RedisStorageSize string
}

// ApplicationReconciler reconciles Application resources.
//
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=list;watch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgrescredentials,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgresdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgresmigrationsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=natsclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=natsaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=redisdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=rediscredentials,verbs=get;list;watch;create;update;patch;delete
type ApplicationReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Store      *configstore.Store
	RouteStore *routestore.Store
	Config     Config
}

// Reconcile is the main reconciliation loop for Application resources.
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var app wasmplatformv1alpha1.Application
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !app.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &app)
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
	return result, err
}

// reconcileDelete removes the Application's config from the stores, broadcasts
// delete updates to connected hosts and gateways, cleans up infra CRs when no
// longer needed, and strips the finalizer.
func (r *ApplicationReconciler) reconcileDelete(ctx context.Context, app *wasmplatformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	infraNS := r.Config.PostgresCredentialNamespace

	if app.Spec.SQL != nil {
		for _, userName := range sqlUsersForApp(app.Spec.SQL) {
			credName := K8sCredentialName(app.Namespace, app.Name, userName)
			var cred dboperator.PostgresCredential
			err := r.Get(ctx, types.NamespacedName{
				Namespace: infraNS,
				Name:      credName,
			}, &cred)
			if err == nil {
				if delErr := r.Delete(ctx, &cred); delErr != nil && !apierrors.IsNotFound(delErr) {
					return ctrl.Result{}, fmt.Errorf("deleting PostgresCredential %q: %w", credName, delErr)
				}
			} else if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("getting PostgresCredential %q for deletion: %w", credName, err)
			}
		}

		if app.Spec.SQL.Migrations != nil {
			msName := MigrationSetName(app.Namespace, app.Name)
			var ms dboperator.PostgresMigrationSet
			err := r.Get(ctx, types.NamespacedName{
				Namespace: infraNS,
				Name:      msName,
			}, &ms)
			if err == nil {
				if delErr := r.Delete(ctx, &ms); delErr != nil && !apierrors.IsNotFound(delErr) {
					return ctrl.Result{}, fmt.Errorf("deleting PostgresMigrationSet %q: %w", msName, delErr)
				}
			} else if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("getting PostgresMigrationSet %q for deletion: %w", msName, err)
			}
		}

		// If this is the last Application with spec.sql in this namespace, delete the PostgresDatabase.
		if err := r.maybeDeletePostgresDatabase(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
	}

	// If this is the last Application with spec.kv, delete the RedisDatabase and credentials.
	if app.Spec.KV {
		if err := r.maybeDeleteRedisDatabase(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
	}

	// If this is the last Application of any kind, delete the NatsCluster and accounts.
	if err := r.maybeDeleteNatsCluster(ctx, app); err != nil {
		return ctrl.Result{}, err
	}

	r.Store.Delete(key)
	r.Store.BroadcastUpdate(buildDeleteUpdate(r.Store, app))

	oldRoutes := r.RouteStore.Get(key)
	if len(oldRoutes) > 0 {
		r.RouteStore.Delete(key)
		r.RouteStore.BroadcastUpdate(buildRouteDeleteUpdate(oldRoutes))
	}

	controllerutil.RemoveFinalizer(app, applicationFinalizer)
	if err := r.Update(ctx, app); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("application deleted from config store", "name", app.Name, "namespace", app.Namespace)
	return ctrl.Result{}, nil
}

// maybeDeleteNatsCluster deletes the NatsCluster and NatsAccounts if this is
// the last Application being deleted.
func (r *ApplicationReconciler) maybeDeleteNatsCluster(ctx context.Context, app *wasmplatformv1alpha1.Application) error {
	var allApps wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &allApps); err != nil {
		return fmt.Errorf("listing applications for NATS cleanup: %w", err)
	}
	remaining := 0
	for i := range allApps.Items {
		a := &allApps.Items[i]
		if a.Namespace == app.Namespace && a.Name == app.Name {
			continue
		}
		if a.DeletionTimestamp.IsZero() {
			remaining++
		}
	}
	if remaining > 0 {
		return nil
	}

	infraNS := r.Config.PostgresCredentialNamespace
	for _, accountName := range []string{natsAccountExecHostName, natsAccountGatewayName} {
		var account dboperator.NatsAccount
		err := r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: accountName}, &account)
		if err == nil {
			if delErr := r.Delete(ctx, &account); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("deleting NatsAccount %q: %w", accountName, delErr)
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting NatsAccount %q for deletion: %w", accountName, err)
		}
	}

	var cluster dboperator.NatsCluster
	err := r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: natsClusterName}, &cluster)
	if err == nil {
		if delErr := r.Delete(ctx, &cluster); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting NatsCluster %q: %w", natsClusterName, delErr)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting NatsCluster %q for deletion: %w", natsClusterName, err)
	}

	if r.Store.SetNatsConfig(nil) {
		r.Store.BroadcastUpdate(buildInfraUpdate(r.Store))
	}
	return nil
}

// maybeDeletePostgresDatabase deletes the PostgresDatabase for this app's namespace
// if this is the last Application with spec.sql in that namespace.
func (r *ApplicationReconciler) maybeDeletePostgresDatabase(ctx context.Context, app *wasmplatformv1alpha1.Application) error {
	var allApps wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &allApps, client.InNamespace(app.Namespace)); err != nil {
		return fmt.Errorf("listing applications in namespace %q for Postgres cleanup: %w", app.Namespace, err)
	}
	for i := range allApps.Items {
		a := &allApps.Items[i]
		if a.Name == app.Name {
			continue
		}
		if a.DeletionTimestamp.IsZero() && a.Spec.SQL != nil {
			return nil // still needed
		}
	}

	pgdbName := postgresDatabaseCRName(app.Namespace)
	infraNS := r.Config.PostgresCredentialNamespace
	var pgdb dboperator.PostgresDatabase
	err := r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: pgdbName}, &pgdb)
	if err == nil {
		if delErr := r.Delete(ctx, &pgdb); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting PostgresDatabase %q: %w", pgdbName, delErr)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting PostgresDatabase %q for deletion: %w", pgdbName, err)
	}
	return nil
}

// maybeDeleteRedisDatabase deletes the RedisDatabase and credentials if this is
// the last Application with spec.kv being deleted.
func (r *ApplicationReconciler) maybeDeleteRedisDatabase(ctx context.Context, app *wasmplatformv1alpha1.Application) error {
	var allApps wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &allApps); err != nil {
		return fmt.Errorf("listing applications for Redis cleanup: %w", err)
	}
	for i := range allApps.Items {
		a := &allApps.Items[i]
		if a.Namespace == app.Namespace && a.Name == app.Name {
			continue
		}
		if a.DeletionTimestamp.IsZero() && a.Spec.KV {
			return nil // still needed
		}
	}

	infraNS := r.Config.PostgresCredentialNamespace

	var cred dboperator.RedisCredential
	err := r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: redisCredExecHostName}, &cred)
	if err == nil {
		if delErr := r.Delete(ctx, &cred); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting RedisCredential %q: %w", redisCredExecHostName, delErr)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting RedisCredential %q for deletion: %w", redisCredExecHostName, err)
	}

	var rdb dboperator.RedisDatabase
	err = r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: redisDatabaseName}, &rdb)
	if err == nil {
		if delErr := r.Delete(ctx, &rdb); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting RedisDatabase %q: %w", redisDatabaseName, delErr)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting RedisDatabase %q for deletion: %w", redisDatabaseName, err)
	}

	if r.Store.SetRedisConfig(nil) {
		r.Store.BroadcastUpdate(buildInfraUpdate(r.Store))
	}
	return nil
}

// reconcileUpsert builds the ApplicationConfig from the spec, pushes it to the
// store, and broadcasts an incremental update.
func (r *ApplicationReconciler) reconcileUpsert(ctx context.Context, app *wasmplatformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	// ── Step 1: Ensure NATS infrastructure ──────────────────────────────────────
	natsRequeue, err := r.reconcileNatsCluster(ctx, app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if natsRequeue {
		r.setReadyCondition(app, metav1.ConditionFalse, "NatsProvisioningPending", "Waiting for NatsCluster and accounts to reach Ready phase.")
		_ = r.Status().Update(ctx, app)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// ── Step 2: Ensure Postgres infrastructure (if spec.sql is set) ─────────────
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

	// ── Step 3: Ensure Redis infrastructure (if spec.kv is set) ─────────────────
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

	// ── Step 4: Build function configs ───────────────────────────────────────────
	functions := make([]*configsync.FunctionConfig, 0, len(app.Spec.Functions))
	for i := range app.Spec.Functions {
		fn := &app.Spec.Functions[i]
		topic := internalFunctionTopic(app, fn)
		fnCfg := &configsync.FunctionConfig{
			Name:      fn.Name,
			ModuleRef: fn.Module,
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

// reconcileNatsCluster ensures the NatsCluster and NatsAccounts exist and are Ready.
// Returns (true, nil) when provisioning is pending (caller should requeue).
func (r *ApplicationReconciler) reconcileNatsCluster(ctx context.Context, app *wasmplatformv1alpha1.Application) (requeue bool, err error) {
	infraNS := r.Config.PostgresCredentialNamespace
	natsVer := r.Config.NatsVersion
	if natsVer == "" {
		natsVer = "2.10"
	}
	jetStreamSize := r.Config.NatsJetStreamStorageSize
	if jetStreamSize == "" {
		jetStreamSize = "1Gi"
	}

	// Ensure NatsCluster.
	var cluster dboperator.NatsCluster
	err = r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: natsClusterName}, &cluster)
	if apierrors.IsNotFound(err) {
		desired := &dboperator.NatsCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      natsClusterName,
				Namespace: infraNS,
				Labels:    infraLabels(),
			},
			Spec: dboperator.NatsClusterSpec{
				NatsVersion: natsVer,
				JetStream: &dboperator.NatsJetStreamConfig{
					StorageSize: resource.MustParse(jetStreamSize),
				},
			},
		}
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return false, fmt.Errorf("creating NatsCluster %q: %w", natsClusterName, createErr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting NatsCluster %q: %w", natsClusterName, err)
	}
	if cluster.Status.Phase != dboperator.NatsClusterPhaseReady {
		return true, nil
	}

	// Ensure NatsAccount for execution-host (with fn.> + http.> exports for gateway).
	execHostAccountReady, err := r.ensureNatsAccount(ctx, infraNS, natsAccountExecHostName, natsClusterName,
		[]dboperator.NatsUser{
			{Username: "execution-host", SecretName: natsExecHostSecretName},
		},
		[]dboperator.NatsExport{
			{Subject: "http.>", Type: dboperator.NatsExportTypeService},
			{Subject: "fn.>", Type: dboperator.NatsExportTypeService},
		},
		nil,
	)
	if err != nil {
		return false, err
	}
	if !execHostAccountReady {
		return true, nil
	}

	// Ensure NatsAccount for gateway (imports http.> from execution-host account).
	gatewayAccountReady, err := r.ensureNatsAccount(ctx, infraNS, natsAccountGatewayName, natsClusterName,
		[]dboperator.NatsUser{
			{Username: "gateway", SecretName: natsGatewaySecretName},
		},
		nil,
		[]dboperator.NatsImport{
			{Account: natsAccountExecHostName, Subject: "http.>", Type: dboperator.NatsExportTypeService},
		},
	)
	if err != nil {
		return false, err
	}
	if !gatewayAccountReady {
		return true, nil
	}

	// Read execution-host NATS secret and update configstore.
	var natsSecret corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: natsExecHostSecretName}, &natsSecret)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting NATS secret %q: %w", natsExecHostSecretName, err)
	}

	natsHost := string(natsSecret.Data["NATS_HOST"])
	natsPort := string(natsSecret.Data["NATS_PORT"])
	natsUser := string(natsSecret.Data["NATS_USERNAME"])
	natsPass := string(natsSecret.Data["NATS_PASSWORD"])
	if natsHost == "" || natsPort == "" || natsUser == "" {
		return true, nil
	}

	natsCfg := &configsync.NatsConnectionConfig{
		Url:      fmt.Sprintf("nats://%s:%s", natsHost, natsPort),
		Username: natsUser,
		Password: natsPass,
	}
	if r.Store.SetNatsConfig(natsCfg) {
		r.Store.BroadcastUpdate(buildInfraUpdate(r.Store))
	}
	return false, nil
}

// ensureNatsAccount creates or updates a NatsAccount CR and reports whether it
// has reached Ready phase.
func (r *ApplicationReconciler) ensureNatsAccount(
	ctx context.Context,
	namespace, name, clusterRef string,
	users []dboperator.NatsUser,
	exports []dboperator.NatsExport,
	imports []dboperator.NatsImport,
) (ready bool, err error) {
	var account dboperator.NatsAccount
	err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &account)
	if apierrors.IsNotFound(err) {
		desired := &dboperator.NatsAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    infraLabels(),
			},
			Spec: dboperator.NatsAccountSpec{
				ClusterRef: clusterRef,
				Users:      users,
				Exports:    exports,
				Imports:    imports,
			},
		}
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return false, fmt.Errorf("creating NatsAccount %q: %w", name, createErr)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting NatsAccount %q: %w", name, err)
	}
	return account.Status.Phase == dboperator.NatsAccountPhaseReady, nil
}

// reconcilePostgresDatabase ensures the PostgresDatabase CR for this app's
// namespace exists and is Ready. Returns (true, nil) when pending.
func (r *ApplicationReconciler) reconcilePostgresDatabase(ctx context.Context, app *wasmplatformv1alpha1.Application) (requeue bool, err error) {
	infraNS := r.Config.PostgresCredentialNamespace
	pgdbName := postgresDatabaseCRName(app.Namespace)
	pgVersion := r.Config.PostgresVersion
	if pgVersion == "" {
		pgVersion = "16"
	}
	pgStorageSize := r.Config.PostgresStorageSize
	if pgStorageSize == "" {
		pgStorageSize = "1Gi"
	}

	var pgdb dboperator.PostgresDatabase
	err = r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: pgdbName}, &pgdb)
	if apierrors.IsNotFound(err) {
		desired := &dboperator.PostgresDatabase{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pgdbName,
				Namespace: infraNS,
				Labels:    infraLabels(),
			},
			Spec: dboperator.PostgresDatabaseSpec{
				PostgresVersion: pgVersion,
				StorageSize:     resource.MustParse(pgStorageSize),
			},
		}
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return false, fmt.Errorf("creating PostgresDatabase %q: %w", pgdbName, createErr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting PostgresDatabase %q: %w", pgdbName, err)
	}
	return pgdb.Status.Phase != dboperator.DatabasePhaseReady, nil
}

// reconcileRedisDatabase ensures the RedisDatabase and execution-host
// RedisCredential exist and are Ready. Returns (true, nil) when pending.
func (r *ApplicationReconciler) reconcileRedisDatabase(ctx context.Context, app *wasmplatformv1alpha1.Application) (requeue bool, err error) {
	infraNS := r.Config.PostgresCredentialNamespace
	redisStorageSize := r.Config.RedisStorageSize
	if redisStorageSize == "" {
		redisStorageSize = "1Gi"
	}

	// Ensure RedisDatabase.
	var rdb dboperator.RedisDatabase
	err = r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: redisDatabaseName}, &rdb)
	if apierrors.IsNotFound(err) {
		desired := &dboperator.RedisDatabase{
			ObjectMeta: metav1.ObjectMeta{
				Name:      redisDatabaseName,
				Namespace: infraNS,
				Labels:    infraLabels(),
			},
			Spec: dboperator.RedisDatabaseSpec{
				StorageSize: resource.MustParse(redisStorageSize),
			},
		}
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return false, fmt.Errorf("creating RedisDatabase %q: %w", redisDatabaseName, createErr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting RedisDatabase %q: %w", redisDatabaseName, err)
	}
	if rdb.Status.Phase != dboperator.RedisDatabasePhaseReady {
		return true, nil
	}

	// Ensure RedisCredential for execution-host.
	var rcred dboperator.RedisCredential
	err = r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: redisCredExecHostName}, &rcred)
	if apierrors.IsNotFound(err) {
		desired := &dboperator.RedisCredential{
			ObjectMeta: metav1.ObjectMeta{
				Name:      redisCredExecHostName,
				Namespace: infraNS,
				Labels:    infraLabels(),
			},
			Spec: dboperator.RedisCredentialSpec{
				DatabaseRef:   redisDatabaseName,
				Username:      "execution-host",
				SecretName:    redisExecHostSecretName,
				KeyPatterns:   []string{"*"},
				ACLCategories: []dboperator.RedisACLCategory{dboperator.RedisACLCategoryAll},
			},
		}
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return false, fmt.Errorf("creating RedisCredential %q: %w", redisCredExecHostName, createErr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting RedisCredential %q: %w", redisCredExecHostName, err)
	}
	if rcred.Status.Phase != dboperator.RedisCredentialPhaseReady {
		return true, nil
	}

	// Read Redis secret and update configstore.
	var redisSecret corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: infraNS, Name: redisExecHostSecretName}, &redisSecret)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Redis secret %q: %w", redisExecHostSecretName, err)
	}

	redisUser := string(redisSecret.Data["REDIS_USERNAME"])
	redisPass := string(redisSecret.Data["REDIS_PASSWORD"])
	redisHost := string(redisSecret.Data["REDIS_HOST"])
	redisPort := string(redisSecret.Data["REDIS_PORT"])
	if redisHost == "" || redisPort == "" {
		return true, nil
	}

	redisURL := (&url.URL{
		Scheme: "redis",
		User:   url.UserPassword(redisUser, redisPass),
		Host:   redisHost + ":" + redisPort,
	}).String()

	redisCfg := &configsync.RedisConnectionConfig{Url: redisURL}
	if r.Store.SetRedisConfig(redisCfg) {
		r.Store.BroadcastUpdate(buildInfraUpdate(r.Store))
	}
	return false, nil
}

// infraLabels returns the standard label set applied to all operator-managed
// infrastructure CRs.
func infraLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "wp-operator",
	}
}

// reconcileSQLBinding ensures one PostgresCredential CR exists per SQL user
// (or the implicit 'app' user) and returns resolved SqlUserConfig entries once
// all db-operator Secrets are available.
// Returns (nil, true, nil) when any credential or Secret is not yet ready.
func (r *ApplicationReconciler) reconcileSQLBinding(ctx context.Context, app *wasmplatformv1alpha1.Application) ([]*configsync.SqlUserConfig, bool, error) {
	credNS := r.Config.PostgresCredentialNamespace
	dbName := PGDatabaseName(app.Namespace, app.Name)
	userNames := sqlUsersForApp(app.Spec.SQL)
	userPermissions := sqlPermissionsForApp(app.Spec.SQL)
	pgdbName := postgresDatabaseCRName(app.Namespace)

	var sqlUsers []*configsync.SqlUserConfig

	for _, userName := range userNames {
		pgUsername := PGUsername(app.Namespace, app.Name, userName)
		credName := K8sCredentialName(app.Namespace, app.Name, userName)
		secretName := credName + "-creds"

		var cred dboperator.PostgresCredential
		err := r.Get(ctx, types.NamespacedName{Namespace: credNS, Name: credName}, &cred)
		if apierrors.IsNotFound(err) {
			desired := buildPostgresCredentialForUser(
				credName, credNS, secretName,
				pgUsername, dbName,
				userPermissions[userName],
				pgdbName,
			)
			if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
				return nil, false, fmt.Errorf("creating PostgresCredential %q: %w", credName, createErr)
			}
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("getting PostgresCredential %q: %w", credName, err)
		}

		if cred.Status.Phase != dboperator.CredentialPhaseReady {
			return nil, true, nil
		}

		var secret corev1.Secret
		err = r.Get(ctx, types.NamespacedName{Namespace: credNS, Name: secretName}, &secret)
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("getting Secret %q: %w", secretName, err)
		}

		pgUser := string(secret.Data["PGUSER"])
		pgPass := string(secret.Data["PGPASSWORD"])
		pgHost := string(secret.Data["PGHOST"])
		pgPort := string(secret.Data["PGPORT"])

		connURL := &url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(pgUser, pgPass),
			Host:   pgHost + ":" + pgPort,
			Path:   "/" + dbName,
		}

		sqlUsers = append(sqlUsers, &configsync.SqlUserConfig{
			Username:      pgUsername,
			ConnectionUrl: connURL.String(),
		})
	}

	return sqlUsers, false, nil
}

// ── SQL helpers ───────────────────────────────────────────────────────────────

// reconcileMigrationSet ensures a PostgresMigrationSet CR exists for the Application
// and reports whether it has reached Ready phase. On failure it returns a non-empty
// failMsg and no automatic requeue is performed — the user must correct the artifact.
func (r *ApplicationReconciler) reconcileMigrationSet(ctx context.Context, app *wasmplatformv1alpha1.Application) (done bool, failMsg string, err error) {
	msName := MigrationSetName(app.Namespace, app.Name)
	msNS := r.Config.PostgresCredentialNamespace
	dbName := PGDatabaseName(app.Namespace, app.Name)

	desiredSpec := dboperator.PostgresMigrationSetSpec{
		DatabaseRef:    postgresDatabaseCRName(app.Namespace),
		Database:       dbName,
		Artifact:       app.Spec.SQL.Migrations.Artifact,
		TargetRevision: app.Spec.SQL.Migrations.TargetRevision,
	}

	var existing dboperator.PostgresMigrationSet
	err = r.Get(ctx, types.NamespacedName{Namespace: msNS, Name: msName}, &existing)
	if apierrors.IsNotFound(err) {
		desired := &dboperator.PostgresMigrationSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      msName,
				Namespace: msNS,
				Labels: map[string]string{
					"wasm-platform.io/app-namespace": app.Namespace,
					"wasm-platform.io/app-name":      app.Name,
				},
			},
			Spec: desiredSpec,
		}
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return false, "", fmt.Errorf("creating PostgresMigrationSet %q: %w", msName, createErr)
		}
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("getting PostgresMigrationSet %q: %w", msName, err)
	}

	// Patch if the artifact or target revision changed.
	if existing.Spec.Artifact != desiredSpec.Artifact || existing.Spec.TargetRevision != desiredSpec.TargetRevision {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.Artifact = desiredSpec.Artifact
		existing.Spec.TargetRevision = desiredSpec.TargetRevision
		if patchErr := r.Patch(ctx, &existing, patch); patchErr != nil {
			return false, "", fmt.Errorf("patching PostgresMigrationSet %q: %w", msName, patchErr)
		}
		return false, "", nil // requeue to observe new status
	}

	switch existing.Status.Phase {
	case dboperator.MigrationSetPhaseReady:
		return true, "", nil
	case dboperator.MigrationSetPhaseFailed:
		msg := fmt.Sprintf("PostgresMigrationSet %s failed", msName)
		for _, c := range existing.Status.Conditions {
			if c.Status == metav1.ConditionFalse && c.Message != "" {
				msg = fmt.Sprintf("PostgresMigrationSet %s: %s", msName, c.Message)
				break
			}
		}
		return false, msg, nil
	default:
		// Pending or Running — wait.
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

// buildPostgresCredentialForUser constructs a PostgresCredential for a single
// Application SQL user.
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
		// Re-enqueue all Applications when NATS cluster state changes.
		Watches(
			&dboperator.NatsCluster{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
				return r.allApplicationRequests(ctx)
			}),
		).
		// Re-enqueue all Applications when a NatsAccount state changes.
		Watches(
			&dboperator.NatsAccount{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
				return r.allApplicationRequests(ctx)
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

// allApplicationRequests returns reconcile.Requests for every Application across
// all namespaces.  Used to re-enqueue Applications when a cluster-wide infra CR
// (NatsCluster, NatsAccount) changes state.
func (r *ApplicationReconciler) allApplicationRequests(ctx context.Context) []reconcile.Request {
	var list wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
	return reqs
}

// allKVApplicationRequests returns reconcile.Requests for every Application
// that has spec.kv set.
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

// applicationsForPostgresDatabase returns reconcile.Requests for Applications
// with spec.sql in the namespace derived from the PostgresDatabase CR name.
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

// findMetricOwner returns the Application that rightfully owns the given metric
// name, or nil if app itself is the rightful owner (or the sole claimant).
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
		Nats:      store.NatsConfig(),
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
		Nats:      store.NatsConfig(),
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
		Nats:      store.NatsConfig(),
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
