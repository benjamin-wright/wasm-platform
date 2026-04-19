package controller

import (
	"context"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
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

// Config holds environment-driven settings injected into the reconciler at
// startup. Values are sourced from env vars (see cmd/main.go).
type Config struct {
	// PostgresDatabaseName is the name of the PostgresDatabase CR that
	// PostgresCredential CRs will reference.
	PostgresDatabaseName string
	// PostgresCredentialNamespace is the namespace in which PostgresCredential
	// CRs and their resulting Secrets are created. Defaults to POD_NAMESPACE.
	PostgresCredentialNamespace string
}

// ApplicationReconciler reconciles Application resources.
//
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=list;watch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgrescredentials,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgresdatabases,verbs=get;list;watch
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
// delete updates to connected hosts and gateways, and strips the finalizer.
func (r *ApplicationReconciler) reconcileDelete(ctx context.Context, app *wasmplatformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	if app.Spec.SQL != nil {
		for _, userName := range sqlUsersForApp(app.Spec.SQL) {
			credName := K8sCredentialName(app.Namespace, app.Name, userName)
			var cred dboperator.PostgresCredential
			err := r.Get(ctx, types.NamespacedName{
				Namespace: r.Config.PostgresCredentialNamespace,
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
	}

	r.Store.Delete(key)
	r.Store.BroadcastUpdate(buildDeleteUpdate(app))

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

// reconcileUpsert builds the ApplicationConfig from the spec, pushes it to the
// store, and broadcasts an incremental update.
func (r *ApplicationReconciler) reconcileUpsert(ctx context.Context, app *wasmplatformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	if app.Spec.SQL != nil {
		if err := ValidatePGInputs(app.Namespace, app.Name); err != nil {
			msg := fmt.Sprintf("cannot derive PG identifiers: %s", err)
			r.setReadyCondition(app, metav1.ConditionFalse, "InvalidIdentifier", msg)
			_ = r.Status().Update(ctx, app)
			return ctrl.Result{}, nil
		}
		if r.Config.PostgresDatabaseName == "" {
			msg := "spec.sql is set but PostgresDatabaseName is not configured"
			r.setReadyCondition(app, metav1.ConditionFalse, "DatabaseConfigMissing", msg)
			_ = r.Status().Update(ctx, app)
			return ctrl.Result{}, nil
		}
		var pgdb dboperator.PostgresDatabase
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: r.Config.PostgresCredentialNamespace,
			Name:      r.Config.PostgresDatabaseName,
		}, &pgdb); err != nil {
			if apierrors.IsNotFound(err) {
				msg := fmt.Sprintf("PostgresDatabase %q not found", r.Config.PostgresDatabaseName)
				r.setReadyCondition(app, metav1.ConditionFalse, "DatabaseNotFound", msg)
				_ = r.Status().Update(ctx, app)
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("getting PostgresDatabase %q: %w", r.Config.PostgresDatabaseName, err)
		}
	}
	for i := range app.Spec.Functions {
		fn := &app.Spec.Functions[i]
		if fn.Trigger.Topic == "" {
			continue
		}
		owner, err := findTopicOwner(ctx, r.Client, fn.Trigger.Topic, app)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking topic ownership for function %q: %w", fn.Name, err)
		}
		if owner != nil {
			msg := fmt.Sprintf("function %q: topic %q is already claimed by %s/%s", fn.Name, fn.Trigger.Topic, owner.Namespace, owner.Name)
			logger.Info("topic conflict detected", "function", fn.Name, "topic", fn.Trigger.Topic, "owner", owner.Namespace+"/"+owner.Name)
			apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
				Type:               "TopicConflict",
				Status:             metav1.ConditionTrue,
				Reason:             "TopicConflict",
				Message:            msg,
				ObservedGeneration: app.Generation,
			})
			r.setReadyCondition(app, metav1.ConditionFalse, "TopicConflict", msg)
			if err := r.Status().Update(ctx, app); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
			}
			// No requeue — healed via the topic-peer watch when the owner is deleted or changes topic.
			return ctrl.Result{}, nil
		}
	}

	for i := range app.Spec.Metrics {
		m := &app.Spec.Metrics[i]
		owner, err := findMetricOwner(ctx, r.Client, m.Name, app)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking metric ownership for %q: %w", m.Name, err)
		}
		if owner != nil {
			msg := fmt.Sprintf("metric %q is already claimed by %s/%s", m.Name, owner.Namespace, owner.Name)
			logger.Info("metric conflict detected", "metric", m.Name, "owner", owner.Namespace+"/"+owner.Name)
			apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
				Type:               "MetricConflict",
				Status:             metav1.ConditionTrue,
				Reason:             "MetricConflict",
				Message:            msg,
				ObservedGeneration: app.Generation,
			})
			r.setReadyCondition(app, metav1.ConditionFalse, "MetricConflict", msg)
			if err := r.Status().Update(ctx, app); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
			}
			// No requeue — healed via the metric-peer watch when the owner is deleted or changes metric names.
			return ctrl.Result{}, nil
		}
	}

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
	}
	cfg.Metrics = buildMetricDefs(app.Spec.Metrics)

	if app.Spec.SQL != nil {
		sqlUsers, requeue, err := r.reconcileSQLBinding(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue {
			// db-operator hasn't finished provisioning the Secrets yet.
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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
		r.Store.BroadcastUpdate(buildUpsertUpdate(cfg))
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

	// Clear any stale TopicConflict and MetricConflict conditions from a previous blocked state.
	apimeta.RemoveStatusCondition(&app.Status.Conditions, "TopicConflict")
	apimeta.RemoveStatusCondition(&app.Status.Conditions, "MetricConflict")

	r.setReadyCondition(app, metav1.ConditionTrue, "ConfigPushed", "Application config pushed to execution hosts.")
	if err := r.Status().Update(ctx, app); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	logger.Info("application config pushed", "name", app.Name, "namespace", app.Namespace)
	return ctrl.Result{}, nil
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
				r.Config.PostgresDatabaseName,
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
			&wasmplatformv1alpha1.Application{},
			handler.Funcs{
				// On delete, wake up all apps sharing the deleted app's topics or metric names.
				// They may now be the rightful owner.
				DeleteFunc: func(ctx context.Context, de event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					app, ok := de.Object.(*wasmplatformv1alpha1.Application)
					if !ok {
						return
					}
					for _, fn := range app.Spec.Functions {
						if fn.Trigger.Topic != "" {
							r.enqueueTopicPeers(ctx, q, fn.Trigger.Topic, app.Namespace, app.Name)
						}
					}
					for _, m := range app.Spec.Metrics {
						r.enqueueMetricPeers(ctx, q, m.Name, app.Namespace, app.Name)
					}
				},
				// On update, wake up apps sharing any *old* topic or metric name that changed —
				// they may now be unblocked.
				UpdateFunc: func(ctx context.Context, ue event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					oldApp, ok := ue.ObjectOld.(*wasmplatformv1alpha1.Application)
					if !ok {
						return
					}
					newApp, ok := ue.ObjectNew.(*wasmplatformv1alpha1.Application)
					if !ok {
						return
					}
					oldTopics := functionTopicSet(oldApp)
					newTopics := functionTopicSet(newApp)
					for topic := range oldTopics {
						if !newTopics[topic] {
							// This topic was removed — wake peers that may now own it.
							r.enqueueTopicPeers(ctx, q, topic, newApp.Namespace, newApp.Name)
						}
					}
					oldMetrics := metricNameSet(oldApp)
					newMetrics := metricNameSet(newApp)
					for name := range oldMetrics {
						if !newMetrics[name] {
							// This metric name was removed — wake peers that may now own it.
							r.enqueueMetricPeers(ctx, q, name, newApp.Namespace, newApp.Name)
						}
					}
				},
			},
		).
		Complete(r)
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

// postgresCredentialName returns a deterministic name for the PostgresCredential
// CR owned by a given Application.
func postgresCredentialName(app *wasmplatformv1alpha1.Application) string {
	return fmt.Sprintf("wp-%s-%s", app.Namespace, app.Name)
}

// postgresCredentialSecretName returns the name of the Secret the db-operator
// will populate for the PostgresCredential.
func postgresCredentialSecretName(app *wasmplatformv1alpha1.Application) string {
	return fmt.Sprintf("wp-%s-%s-pg", app.Namespace, app.Name)
}

// buildPostgresCredential constructs a PostgresCredential CR for a given Application.
func buildPostgresCredential(name, namespace, secretName string, app *wasmplatformv1alpha1.Application, pgdbName string) *dboperator.PostgresCredential {
	// PostgreSQL usernames are limited to 63 characters.
	username := fmt.Sprintf("%s_%s", app.Namespace, app.Name)
	if len(username) > 63 {
		username = username[:63]
	}
	return &dboperator.PostgresCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: dboperator.PostgresCredentialSpec{
			DatabaseRef: pgdbName,
			Username:    username,
			SecretName:  secretName,
			// TODO(Phase 9.2 operator task): populate per-user credentials via PG identifier algorithm.
			Permissions: []dboperator.DatabasePermissionEntry{},
		},
	}
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

// enqueueTopicPeers lists all Applications with the given topic and adds them
// to the work queue, excluding the app identified by (excludeNS, excludeName).
func (r *ApplicationReconciler) enqueueTopicPeers(
	ctx context.Context,
	q workqueue.TypedRateLimitingInterface[reconcile.Request],
	topic, excludeNS, excludeName string,
) {
	var list wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &list, client.MatchingFields{topicIndexField: topic}); err != nil {
		log.FromContext(ctx).Error(err, "enqueueTopicPeers: listing applications", "topic", topic)
		return
	}
	for i := range list.Items {
		app := &list.Items[i]
		if app.Namespace == excludeNS && app.Name == excludeName {
			continue
		}
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: app.Namespace,
			Name:      app.Name,
		}})
	}
}

// functionTopicSet returns a set of all user-supplied topics across all functions.
func functionTopicSet(app *wasmplatformv1alpha1.Application) map[string]bool {
	topics := make(map[string]bool)
	for _, fn := range app.Spec.Functions {
		if fn.Trigger.Topic != "" {
			topics[fn.Trigger.Topic] = true
		}
	}
	return topics
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

// metricNameSet returns a set of all metric names declared by the Application.
func metricNameSet(app *wasmplatformv1alpha1.Application) map[string]bool {
	names := make(map[string]bool)
	for _, m := range app.Spec.Metrics {
		names[m.Name] = true
	}
	return names
}

// enqueueMetricPeers lists all Applications with the given metric name and adds
// them to the work queue, excluding the app identified by (excludeNS, excludeName).
func (r *ApplicationReconciler) enqueueMetricPeers(
	ctx context.Context,
	q workqueue.TypedRateLimitingInterface[reconcile.Request],
	metricName, excludeNS, excludeName string,
) {
	var list wasmplatformv1alpha1.ApplicationList
	if err := r.List(ctx, &list, client.MatchingFields{metricNameIndexField: metricName}); err != nil {
		log.FromContext(ctx).Error(err, "enqueueMetricPeers: listing applications", "metric", metricName)
		return
	}
	for i := range list.Items {
		app := &list.Items[i]
		if app.Namespace == excludeNS && app.Name == excludeName {
			continue
		}
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: app.Namespace,
			Name:      app.Name,
		}})
	}
}

func buildUpsertUpdate(cfg *configsync.ApplicationConfig) *configsync.IncrementalConfig {
	now := time.Now().UnixMilli()
	return &configsync.IncrementalConfig{
		Version:   fmt.Sprintf("%d", now),
		Updates:   []*configsync.AppUpdate{{AppConfig: cfg, Delete: false}},
		Timestamp: now,
	}
}

func buildDeleteUpdate(app *wasmplatformv1alpha1.Application) *configsync.IncrementalConfig {
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
