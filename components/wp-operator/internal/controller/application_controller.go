package controller

import (
	"context"
	"fmt"
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

// topicIndexField is the cache field index key for Application.spec.topic.
// Used by findTopicOwner and the topic-peer watch handler to avoid full-list scans.
const topicIndexField = "spec.topic"

// Config holds environment-driven settings injected into the reconciler at
// startup. Values are sourced from env vars (see cmd/main.go).
type Config struct {
	// PostgresDatabaseName is the name of the PostgresDatabase CR that
	// PostgresCredential CRs will reference.
	PostgresDatabaseName string
	// PostgresCredentialNamespace is the namespace in which PostgresCredential
	// CRs and their resulting Secrets are created. Defaults to POD_NAMESPACE.
	PostgresCredentialNamespace string
	// RedisSecretName is the name of the Secret holding Redis credentials for
	// the wp-operator user provisioned by the wp-databases chart.
	RedisSecretName string
	// RedisSecretNamespace is the namespace containing the Redis Secret.
	// Defaults to POD_NAMESPACE.
	RedisSecretNamespace string
}

// ApplicationReconciler reconciles Application resources.
//
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=list;watch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=db-operator.benjamin-wright.github.com,resources=postgrescredentials,verbs=get;list;watch;create;update;patch;delete
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

	// Ensure the finalizer is present before doing any meaningful work.
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

// reconcileDelete removes the Application's config from the store, broadcasts
// a delete update to connected hosts, and strips the finalizer.
func (r *ApplicationReconciler) reconcileDelete(ctx context.Context, app *wasmplatformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	// Remove the PostgresCredential CR (and its Secret) that we own for this app.
	if app.Spec.SQL != "" {
		credName := postgresCredentialName(app)
		var cred dboperator.PostgresCredential
		err := r.Get(ctx, types.NamespacedName{
			Namespace: r.Config.PostgresCredentialNamespace,
			Name:      credName,
		}, &cred)
		if err == nil {
			if delErr := r.Delete(ctx, &cred); delErr != nil && !apierrors.IsNotFound(delErr) {
				return ctrl.Result{}, fmt.Errorf("deleting PostgresCredential: %w", delErr)
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("getting PostgresCredential for deletion: %w", err)
		}
	}

	r.Store.Delete(key)
	r.Store.BroadcastUpdate(buildDeleteUpdate(app, internalTopic(app)))

	// For HTTP apps, also remove the route from the gateway route store.
	if app.Spec.HTTP != nil {
		r.RouteStore.Delete(key)
		r.RouteStore.BroadcastUpdate(buildRouteDeleteUpdate(app.Spec.HTTP.Path))
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

	// Phase 2: topic uniqueness — short-circuit if another app owns this topic.
	// HTTP apps derive their topic from (namespace, name) so are inherently unique; skip the check.
	if app.Spec.Topic != "" {
		owner, err := findTopicOwner(ctx, r.Client, app.Spec.Topic, app)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking topic ownership: %w", err)
		}
		if owner != nil {
			msg := fmt.Sprintf("topic %q is already claimed by %s/%s", app.Spec.Topic, owner.Namespace, owner.Name)
			logger.Info("topic conflict detected", "topic", app.Spec.Topic, "owner", owner.Namespace+"/"+owner.Name)
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

	cfg := &configsync.ApplicationConfig{
		Name:      app.Name,
		Namespace: app.Namespace,
		// TODO: resolve mutable OCI tags via registry; copy spec.module directly for now.
		ModuleRef: app.Spec.Module,
		Topic:     internalTopic(app),
		Env:       app.Spec.Env,
		WorldType: configsync.WorldType_WORLD_TYPE_MESSAGE,
	}

	if app.Spec.HTTP != nil {
		cfg.WorldType = configsync.WorldType_WORLD_TYPE_HTTP
		cfg.Http = &configsync.HttpConfig{
			Path:    app.Spec.HTTP.Path,
			Methods: app.Spec.HTTP.MethodStrings(),
		}
	}

	if app.Spec.SQL != "" {
		sqlCfg, requeue, err := r.reconcileSQLBinding(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue {
			// db-operator hasn't finished provisioning the Secret yet.
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		cfg.Sql = sqlCfg
	}

	if app.Spec.KeyValue != "" {
		kvCfg, err := r.reconcileKVBinding(ctx, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		cfg.KeyValue = kvCfg
	}

	if r.Store.Set(key, cfg) {
		r.Store.BroadcastUpdate(buildUpsertUpdate(cfg))
	}

	// For HTTP apps, keep the gateway route store in sync.
	if app.Spec.HTTP != nil {
		routeCfg := &routestore.RouteConfig{
			Path:        app.Spec.HTTP.Path,
			Methods:     app.Spec.HTTP.MethodStrings(),
			NatsSubject: internalTopic(app),
		}
		if r.RouteStore.Set(key, routeCfg) {
			r.RouteStore.BroadcastUpdate(buildRouteUpsertUpdate(routeCfg))
		}
	}

	// Clear any stale TopicConflict condition from a previous blocked state.
	apimeta.RemoveStatusCondition(&app.Status.Conditions, "TopicConflict")

	// TODO: replace with real digest resolution from the OCI registry.
	app.Status.ResolvedImage = app.Spec.Module
	r.setReadyCondition(app, metav1.ConditionTrue, "ConfigPushed", "Application config pushed to execution hosts.")
	if err := r.Status().Update(ctx, app); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	logger.Info("application config pushed", "name", app.Name, "namespace", app.Namespace)
	return ctrl.Result{}, nil
}

// reconcileSQLBinding ensures a PostgresCredential CR exists for the app and
// returns the resolved SqlConfig once the db-operator has populated its Secret.
// Returns (nil, true, nil) when the Secret is not yet available; the caller
// should requeue.
func (r *ApplicationReconciler) reconcileSQLBinding(ctx context.Context, app *wasmplatformv1alpha1.Application) (*configsync.SqlConfig, bool, error) {
	credName := postgresCredentialName(app)
	credNS := r.Config.PostgresCredentialNamespace
	secretName := postgresCredentialSecretName(app)

	var cred dboperator.PostgresCredential
	err := r.Get(ctx, types.NamespacedName{Namespace: credNS, Name: credName}, &cred)
	if apierrors.IsNotFound(err) {
		desired := buildPostgresCredential(credName, credNS, secretName, app, r.Config.PostgresDatabaseName)
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return nil, false, fmt.Errorf("creating PostgresCredential: %w", createErr)
		}
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("getting PostgresCredential: %w", err)
	}

	var secret corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: credNS, Name: secretName}, &secret)
	if apierrors.IsNotFound(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("getting postgres credential Secret %q: %w", secretName, err)
	}

	user := string(secret.Data["PGUSER"])
	password := string(secret.Data["PGPASSWORD"])
	host := string(secret.Data["PGHOST"])
	port := string(secret.Data["PGPORT"])
	connURL := fmt.Sprintf("postgres://%s:%s@%s:%s/%s", user, password, host, port, app.Spec.SQL)

	return &configsync.SqlConfig{
		DatabaseName:  app.Spec.SQL,
		ConnectionUrl: connURL,
	}, false, nil
}

// reconcileKVBinding reads the wp-operator Redis credentials Secret provisioned
// by the wp-databases chart and returns a KeyValueConfig.
func (r *ApplicationReconciler) reconcileKVBinding(ctx context.Context, app *wasmplatformv1alpha1.Application) (*configsync.KeyValueConfig, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: r.Config.RedisSecretNamespace,
		Name:      r.Config.RedisSecretName,
	}, &secret); err != nil {
		return nil, fmt.Errorf("getting Redis credentials Secret %q: %w", r.Config.RedisSecretName, err)
	}

	username := string(secret.Data["REDIS_USERNAME"])
	password := string(secret.Data["REDIS_PASSWORD"])
	host := string(secret.Data["REDIS_HOST"])
	port := string(secret.Data["REDIS_PORT"])
	connURL := fmt.Sprintf("redis://%s:%s@%s:%s", username, password, host, port)

	return &configsync.KeyValueConfig{
		// Prefix namespaces keys as <namespace>/<spec.keyValue>/ to prevent
		// conflicts between applications in different namespaces.
		Prefix:        fmt.Sprintf("%s/%s/", app.Namespace, app.Spec.KeyValue),
		ConnectionUrl: connURL,
	}, nil
}

// SetupWithManager registers the ApplicationReconciler with the controller manager.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&wasmplatformv1alpha1.Application{},
		topicIndexField,
		func(obj client.Object) []string {
			app := obj.(*wasmplatformv1alpha1.Application)
			if app.Spec.Topic == "" {
				return nil // HTTP apps have no spec.topic to index
			}
			return []string{app.Spec.Topic}
		},
	); err != nil {
		return fmt.Errorf("registering topic field index: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&wasmplatformv1alpha1.Application{}).
		Watches(
			&wasmplatformv1alpha1.Application{},
			handler.Funcs{
				// On delete, wake up all apps sharing the deleted app's topic.
				// They may now be the rightful owner.
				DeleteFunc: func(ctx context.Context, de event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					app, ok := de.Object.(*wasmplatformv1alpha1.Application)
					if !ok {
						return
					}
					r.enqueueTopicPeers(ctx, q, app.Spec.Topic, app.Namespace, app.Name)
				},
				// On update, wake up apps sharing the *old* topic when the topic
				// has changed — they may now be unblocked.
				UpdateFunc: func(ctx context.Context, ue event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					oldApp, ok := ue.ObjectOld.(*wasmplatformv1alpha1.Application)
					if !ok {
						return
					}
					newApp, ok := ue.ObjectNew.(*wasmplatformv1alpha1.Application)
					if !ok {
						return
					}
					if oldApp.Spec.Topic == newApp.Spec.Topic {
						return // topic unchanged; no peers need waking
					}
					r.enqueueTopicPeers(ctx, q, oldApp.Spec.Topic, newApp.Namespace, newApp.Name)
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
			Permissions: []dboperator.DatabasePermissionEntry{
				{
					Databases:   []string{app.Spec.SQL},
					Permissions: []dboperator.DatabasePermission{dboperator.PermissionSelect, dboperator.PermissionInsert, dboperator.PermissionUpdate, dboperator.PermissionDelete},
				},
			},
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

	// Find the single rightful owner across all claimants (including self).
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

func buildUpsertUpdate(cfg *configsync.ApplicationConfig) *configsync.IncrementalConfig {
	now := time.Now().UnixMilli()
	return &configsync.IncrementalConfig{
		Version:   fmt.Sprintf("%d", now),
		Updates:   []*configsync.AppUpdate{{AppConfig: cfg, Delete: false}},
		Timestamp: now,
	}
}

func buildDeleteUpdate(app *wasmplatformv1alpha1.Application, topic string) *configsync.IncrementalConfig {
	now := time.Now().UnixMilli()
	return &configsync.IncrementalConfig{
		Version: fmt.Sprintf("%d", now),
		Updates: []*configsync.AppUpdate{
			{
				AppConfig: &configsync.ApplicationConfig{
					Name:      app.Name,
					Namespace: app.Namespace,
					Topic:     topic,
				},
				Delete: true,
			},
		},
		Timestamp: now,
	}
}

// internalTopic returns the fully-prefixed NATS subject for the given Application.
// Topic-only apps use the "fn." prefix; HTTP apps use "http.<namespace>.<name>".
func internalTopic(app *wasmplatformv1alpha1.Application) string {
	if app.Spec.Topic != "" {
		return "fn." + app.Spec.Topic
	}
	return fmt.Sprintf("http.%s.%s", app.Namespace, app.Name)
}

func buildRouteUpsertUpdate(cfg *routestore.RouteConfig) *routestore.RouteUpdateBatch {
	now := time.Now().UnixMilli()
	return &routestore.RouteUpdateBatch{
		Version:   fmt.Sprintf("%d", now),
		Updates:   []*routestore.RouteUpdate{{Config: cfg, Delete: false}},
		Timestamp: now,
	}
}

func buildRouteDeleteUpdate(path string) *routestore.RouteUpdateBatch {
	now := time.Now().UnixMilli()
	return &routestore.RouteUpdateBatch{
		Version:   fmt.Sprintf("%d", now),
		Updates:   []*routestore.RouteUpdate{{Config: &routestore.RouteConfig{Path: path}, Delete: true}},
		Timestamp: now,
	}
}
