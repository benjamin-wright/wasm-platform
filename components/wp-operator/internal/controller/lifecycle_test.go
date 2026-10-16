package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/artifacts"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/provisioning"
)

type lifecycleClient struct {
	client.Client
	nextUID int
}

func (c *lifecycleClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		c.nextUID++
		obj.SetUID(types.UID(fmt.Sprintf("created-resource-%d", c.nextUID)))
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *lifecycleClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	return c.collectCredentialSecrets(ctx, obj)
}

func (c *lifecycleClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.Client.Update(ctx, obj, opts...); err != nil {
		return err
	}
	return c.collectCredentialSecrets(ctx, obj)
}

func (c *lifecycleClient) collectCredentialSecrets(ctx context.Context, obj client.Object) error {
	var kind string
	switch obj.(type) {
	case *dboperator.PostgresCredential:
		kind = "PostgresCredential"
	case *dboperator.RedisCredential:
		kind = "RedisCredential"
	default:
		return nil
	}
	if obj.GetUID() == "" {
		return nil
	}
	parent := obj.DeepCopyObject().(client.Object)
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), parent); !apierrors.IsNotFound(err) {
		return err
	}
	var secrets corev1.SecretList
	if err := c.Client.List(ctx, &secrets, client.InNamespace(obj.GetNamespace())); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		owner := metav1.GetControllerOf(secret)
		if owner == nil || owner.APIVersion != dboperator.GroupVersion.String() || owner.Kind != kind || owner.Name != obj.GetName() || owner.UID != obj.GetUID() {
			continue
		}
		if err := c.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func lifecycleApp(name string) *wasmplatformv1alpha1.Application {
	digest := moduleDigest([]byte("lifecycle module"))
	moduleRef := "oci://approved.example/lifecycle@" + digest
	app := artifactApp(moduleRef)
	app.Name = name
	app.UID = types.UID("application-" + name)
	app.Spec.Functions[0].Trigger.Topic = name + ".requests"
	app.Status.ResolvedModules = []wasmplatformv1alpha1.ResolvedModuleStatus{{
		Name: "handler", Generation: 1, RequestedRef: moduleRef,
		ModuleRef: moduleRef, ManifestDigest: digest, WasmDigest: digest,
	}}
	return app
}

func lifecycleReconciler(t *testing.T, apps ...*wasmplatformv1alpha1.Application) (*ApplicationReconciler, *lifecycleClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{wasmplatformv1alpha1.AddToScheme, dboperator.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := make([]client.Object, len(apps))
	for i, app := range apps {
		objects[i] = app
	}
	c := &lifecycleClient{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&wasmplatformv1alpha1.ResourceInventory{}, &wasmplatformv1alpha1.Application{}, &dboperator.PostgresDatabase{}, &dboperator.PostgresCredential{}, &dboperator.PostgresMigrationSet{}, &dboperator.RedisDatabase{}, &dboperator.RedisCredential{}).
		Build()}
	registry, err := artifacts.NewRegistry("approved.example", "")
	if err != nil {
		t.Fatal(err)
	}
	r := artifactReconciler(c, registry)
	r.Scheme = scheme
	r.Config.PostgresCredentialNamespace = "platform"
	return r, c
}

func reconcileLifecycle(t *testing.T, r *ApplicationReconciler, key types.NamespacedName) (ctrl.Result, error) {
	t.Helper()
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		return result, err
	}
	var inventories wasmplatformv1alpha1.ResourceInventoryList
	if err := r.List(t.Context(), &inventories); err != nil {
		return result, err
	}
	cleanup := &ResourceInventoryReconciler{Client: r.Client, APIReader: r.reader()}
	for _, inventory := range inventories.Items {
		if _, err := cleanup.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&inventory)}); err != nil {
			return result, err
		}
	}
	return result, nil
}

func getLifecycleApp(t *testing.T, c client.Client, key types.NamespacedName) *wasmplatformv1alpha1.Application {
	t.Helper()
	var app wasmplatformv1alpha1.Application
	if err := c.Get(t.Context(), key, &app); err != nil {
		t.Fatal(err)
	}
	return &app
}

func readyLifecycleBackend(t *testing.T, r *ApplicationReconciler) {
	t.Helper()
	ctx := t.Context()
	var postgres dboperator.PostgresDatabaseList
	if err := r.List(ctx, &postgres); err != nil {
		t.Fatal(err)
	}
	for i := range postgres.Items {
		postgres.Items[i].Status.Phase = dboperator.DatabasePhaseReady
		if err := r.Status().Update(ctx, &postgres.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	var credentials dboperator.PostgresCredentialList
	if err := r.List(ctx, &credentials); err != nil {
		t.Fatal(err)
	}
	for i := range credentials.Items {
		cred := &credentials.Items[i]
		cred.Status.Phase = dboperator.CredentialPhaseReady
		cred.Status.SecretName = cred.Spec.SecretName
		if err := r.Status().Update(ctx, cred); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cred.Namespace, Name: cred.Spec.SecretName}, Data: map[string][]byte{
			"PGUSER": []byte(cred.Spec.Username), "PGPASSWORD": []byte("fixture-password"),
			"PGHOST": []byte("postgres.example"), "PGPORT": []byte("5432"),
		}}
		if err := controllerutil.SetControllerReference(cred, secret, r.Scheme); err != nil {
			t.Fatal(err)
		}
		if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	var migrations dboperator.PostgresMigrationSetList
	if err := r.List(ctx, &migrations); err != nil {
		t.Fatal(err)
	}
	for i := range migrations.Items {
		ms := &migrations.Items[i]
		ms.Status.Phase = dboperator.MigrationSetPhaseReady
		ms.Status.ObservedGeneration = ms.Generation
		ms.Status.CurrentRevision = &ms.Spec.TargetRevision
		ms.Status.ObservedArtifact = ms.Spec.Artifact
		if err := r.Status().Update(ctx, ms); err != nil {
			t.Fatal(err)
		}
	}
	var redis dboperator.RedisDatabaseList
	if err := r.List(ctx, &redis); err != nil {
		t.Fatal(err)
	}
	for i := range redis.Items {
		redis.Items[i].Status.Phase = dboperator.RedisDatabasePhaseReady
		if err := r.Status().Update(ctx, &redis.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	var redisCredentials dboperator.RedisCredentialList
	if err := r.List(ctx, &redisCredentials); err != nil {
		t.Fatal(err)
	}
	for i := range redisCredentials.Items {
		cred := &redisCredentials.Items[i]
		cred.Status.Phase = dboperator.RedisCredentialPhaseReady
		if err := r.Status().Update(ctx, cred); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cred.Namespace, Name: cred.Spec.SecretName}, Data: map[string][]byte{
			"REDIS_USERNAME": []byte(cred.Spec.Username), "REDIS_PASSWORD": []byte("fixture-password"),
			"REDIS_HOST": []byte("redis.example"), "REDIS_PORT": []byte("6379"),
		}}
		if err := controllerutil.SetControllerReference(cred, secret, r.Scheme); err != nil {
			t.Fatal(err)
		}
		if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

func settleLifecycle(t *testing.T, r *ApplicationReconciler, key types.NamespacedName) *wasmplatformv1alpha1.Application {
	t.Helper()
	for range 15 {
		result, err := reconcileLifecycle(t, r, key)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Requeue && result.RequeueAfter == 0 {
			app := getLifecycleApp(t, r.Client, key)
			for _, condition := range app.Status.Conditions {
				if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
					return app
				}
			}
		}
		readyLifecycleBackend(t, r)
	}
	t.Fatal("application did not finish provisioning")
	return nil
}

func inventoryResource(t *testing.T, app *wasmplatformv1alpha1.Application, kind, logicalName string) wasmplatformv1alpha1.ResourceBinding {
	t.Helper()
	for _, record := range app.Status.ResourceInventory {
		if record.Kind == kind && record.LogicalName == logicalName {
			if record.UID == "" {
				t.Fatalf("%s %q has no persisted resource UID", kind, logicalName)
			}
			return record
		}
	}
	t.Fatalf("missing %s %q from inventory: %+v", kind, logicalName, app.Status.ResourceInventory)
	return wasmplatformv1alpha1.ResourceBinding{}
}

func finishLifecycleDeletion(t *testing.T, r *ApplicationReconciler, key types.NamespacedName) {
	t.Helper()
	for range 10 {
		if _, err := reconcileLifecycle(t, r, key); err != nil {
			t.Fatal(err)
		}
		var app wasmplatformv1alpha1.Application
		if err := r.Get(t.Context(), key, &app); apierrors.IsNotFound(err) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("application deletion did not finish")
}

func TestReconcileDeletesRecordedResourcesAfterSQLAndKVRemoval(t *testing.T) {
	app := lifecycleApp("removed-capabilities")
	app.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{Users: []wasmplatformv1alpha1.SQLUserSpec{{Name: "app"}, {Name: "reader"}}, Migrations: &wasmplatformv1alpha1.MigrationsSpec{Artifact: "approved.example/migrations@" + moduleDigest([]byte("migrations")), TargetRevision: 1}}
	app.Spec.KV = true
	r, c := lifecycleReconciler(t, app)
	key := client.ObjectKeyFromObject(app)
	persisted := settleLifecycle(t, r, key)
	appCredential := inventoryResource(t, persisted, "PostgresCredential", "app")
	reader := inventoryResource(t, persisted, "PostgresCredential", "reader")
	migrations := inventoryResource(t, persisted, "PostgresMigrationSet", "migrations")
	postgres := inventoryResource(t, persisted, "PostgresDatabase", "infrastructure")
	redis := inventoryResource(t, persisted, "RedisDatabase", "infrastructure")
	redisCredential := inventoryResource(t, persisted, "RedisCredential", "infrastructure")
	for _, record := range []wasmplatformv1alpha1.ResourceBinding{postgres, redis, redisCredential} {
		if !record.Retained {
			t.Fatalf("shared infrastructure is not retained: %+v", record)
		}
	}
	persisted.Spec.SQL = nil
	persisted.Spec.KV = false
	if err := c.Update(t.Context(), persisted); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), persisted); err != nil {
		t.Fatal(err)
	}
	finishLifecycleDeletion(t, r, key)
	for _, record := range []wasmplatformv1alpha1.ResourceBinding{appCredential, reader} {
		var cred dboperator.PostgresCredential
		if err := c.Get(t.Context(), types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, &cred); !apierrors.IsNotFound(err) {
			t.Fatalf("recorded credential survived SQL removal and deletion: %v", err)
		}
	}
	var ms dboperator.PostgresMigrationSet
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: migrations.Namespace, Name: migrations.Name}, &ms); !apierrors.IsNotFound(err) {
		t.Fatalf("recorded migration set survived SQL removal and deletion: %v", err)
	}
	for _, object := range []client.Object{&dboperator.PostgresDatabase{}, &dboperator.RedisDatabase{}, &dboperator.RedisCredential{}} {
		record := postgres
		switch object.(type) {
		case *dboperator.RedisDatabase:
			record = redis
		case *dboperator.RedisCredential:
			record = redisCredential
		}
		if err := c.Get(t.Context(), types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, object); err != nil {
			t.Fatalf("retained infrastructure was removed: %v", err)
		}
		if object.GetUID() != types.UID(record.UID) {
			t.Fatal("retained infrastructure identity changed")
		}
	}
}

func TestReconcileDeletionWaitsForDownstreamFinalizer(t *testing.T) {
	app := lifecycleApp("pending-cleanup")
	app.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	key := client.ObjectKeyFromObject(app)
	persisted := settleLifecycle(t, r, key)
	record := inventoryResource(t, persisted, "PostgresCredential", "app")
	var cred dboperator.PostgresCredential
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, &cred); err != nil {
		t.Fatal(err)
	}
	cred.Finalizers = []string{"db-operator.example/role-cleanup"}
	if err := c.Update(t.Context(), &cred); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), persisted); err != nil {
		t.Fatal(err)
	}
	result, err := reconcileLifecycle(t, r, key)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Requeue && result.RequeueAfter == 0 {
		t.Fatal("pending backend cleanup did not request a retry")
	}
	persisted = getLifecycleApp(t, c, key)
	if !controllerutil.ContainsFinalizer(persisted, applicationFinalizer) {
		t.Fatal("application finalizer was removed before role cleanup")
	}
	if inventoryResource(t, persisted, "PostgresCredential", "app").UID != record.UID {
		t.Fatal("pending cleanup obligation was discarded")
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(&cred), &cred); err != nil {
		t.Fatal(err)
	}
	if cred.DeletionTimestamp.IsZero() {
		t.Fatal("credential deletion was not requested")
	}
	cred.Finalizers = nil
	if err := c.Update(t.Context(), &cred); err != nil {
		t.Fatal(err)
	}
	finishLifecycleDeletion(t, r, key)
}

func TestReconcileDeletionRejectsForeignAndSubstitutedResources(t *testing.T) {
	for _, kind := range []string{"PostgresCredential", "PostgresMigrationSet"} {
		for _, change := range []string{"foreign controller", "substituted UID", "changed database"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				app := lifecycleApp("ownership-check")
				app.Spec.Functions[0].Trigger.Topic = ""
				app.Spec.Functions[0].Trigger.HTTP = &wasmplatformv1alpha1.HttpTrigger{Path: "/ownership-check"}
				app.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{Migrations: &wasmplatformv1alpha1.MigrationsSpec{Artifact: "approved.example/migrations@" + moduleDigest([]byte("migrations")), TargetRevision: 1}}
				r, c := lifecycleReconciler(t, app)
				key := client.ObjectKeyFromObject(app)
				persisted := settleLifecycle(t, r, key)
				if len(r.Store.Snapshot()) != 1 || len(r.RouteStore.Get(key)) != 1 {
					t.Fatal("application configuration and route were not published")
				}
				hostUpdates := r.Store.RegisterHost("cleanup-test")
				gatewayUpdates := r.RouteStore.RegisterGateway("cleanup-test")
				logicalName := "app"
				var child client.Object = &dboperator.PostgresCredential{}
				if kind == "PostgresMigrationSet" {
					logicalName = "migrations"
					child = &dboperator.PostgresMigrationSet{}
				}
				record := inventoryResource(t, persisted, kind, logicalName)
				childKey := types.NamespacedName{Namespace: record.Namespace, Name: record.Name}
				if err := c.Get(t.Context(), childKey, child); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "foreign controller":
					owners := child.GetOwnerReferences()
					owners[0].UID = "another-application"
					child.SetOwnerReferences(owners)
				case "changed database":
					switch resource := child.(type) {
					case *dboperator.PostgresCredential:
						resource.Spec.Permissions[0].Databases = []string{"another_database"}
					case *dboperator.PostgresMigrationSet:
						resource.Spec.Database = "another_database"
					}
				case "substituted UID":
					if err := c.Delete(t.Context(), child); err != nil {
						t.Fatal(err)
					}
					child.SetResourceVersion("")
					child.SetUID("substituted-resource")
					if err := c.Create(t.Context(), child); err != nil {
						t.Fatal(err)
					}
				}
				if change != "substituted UID" {
					if err := c.Update(t.Context(), child); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.Delete(t.Context(), persisted); err != nil {
					t.Fatal(err)
				}
				if _, err := reconcileLifecycle(t, r, key); err == nil {
					t.Fatal("ownership mismatch did not block deletion")
				}
				if len(r.Store.Snapshot()) != 0 || len(r.RouteStore.Get(key)) != 0 {
					t.Fatal("cleanup conflict kept the deleting application configuration or route published")
				}
				select {
				case update := <-hostUpdates:
					if len(update.Updates) != 1 || !update.Updates[0].Delete {
						t.Fatal("execution host did not receive application deletion")
					}
				default:
					t.Fatal("execution host received no deletion update")
				}
				select {
				case update := <-gatewayUpdates:
					if len(update.Updates) != 1 || !update.Updates[0].Delete {
						t.Fatal("gateway did not receive route deletion")
					}
				default:
					t.Fatal("gateway received no deletion update")
				}
				if err := c.Get(t.Context(), childKey, child); err != nil || !child.GetDeletionTimestamp().IsZero() {
					t.Fatalf("unverified child was deleted: %v", err)
				}
				if !controllerutil.ContainsFinalizer(getLifecycleApp(t, c, key), applicationFinalizer) {
					t.Fatal("ownership failure released the application finalizer")
				}
			})
		}
	}
}

func TestReconcileCredentialCollisionPairStaysIndependent(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			first, second := lifecycleApp("foo-bar"), lifecycleApp("foo")
			first.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{Users: []wasmplatformv1alpha1.SQLUserSpec{{Name: "baz"}}}
			second.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{Users: []wasmplatformv1alpha1.SQLUserSpec{{Name: "bar-baz"}}}
			r, c := lifecycleReconciler(t, first, second)
			first = settleLifecycle(t, r, client.ObjectKeyFromObject(first))
			second = settleLifecycle(t, r, client.ObjectKeyFromObject(second))
			firstRecord := inventoryResource(t, first, "PostgresCredential", "baz")
			secondRecord := inventoryResource(t, second, "PostgresCredential", "bar-baz")
			if firstRecord.Name == secondRecord.Name || firstRecord.UID == secondRecord.UID || firstRecord.SecretName == secondRecord.SecretName {
				t.Fatal("ambiguous tuple shared credential or Secret identity")
			}
			if reverse {
				first, second = second, first
				firstRecord, secondRecord = secondRecord, firstRecord
			}
			if err := c.Delete(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			finishLifecycleDeletion(t, r, client.ObjectKeyFromObject(first))
			var surviving dboperator.PostgresCredential
			if err := c.Get(t.Context(), types.NamespacedName{Namespace: secondRecord.Namespace, Name: secondRecord.Name}, &surviving); err != nil {
				t.Fatalf("deleting first application removed second credential: %v", err)
			}
			if surviving.UID != types.UID(secondRecord.UID) || !surviving.DeletionTimestamp.IsZero() {
				t.Fatal("deleting first application modified second credential identity")
			}
			if err := c.Delete(t.Context(), second); err != nil {
				t.Fatal(err)
			}
			finishLifecycleDeletion(t, r, client.ObjectKeyFromObject(second))
			if err := c.Get(t.Context(), types.NamespacedName{Namespace: firstRecord.Namespace, Name: firstRecord.Name}, &surviving); !apierrors.IsNotFound(err) {
				t.Fatalf("first application credential survived deletion: %v", err)
			}
		})
	}
}

func TestReconcileSameNameRecreationRetainsDataWithFreshOwnership(t *testing.T) {
	app := lifecycleApp("recreated-application")
	app.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	key := client.ObjectKeyFromObject(app)
	persisted := settleLifecycle(t, r, key)
	previousCredential := inventoryResource(t, persisted, "PostgresCredential", "app")
	previousDatabase := inventoryResource(t, persisted, "PostgresDatabase", "infrastructure")
	previousDataName := persisted.Status.SQLDatabaseName
	if err := c.Delete(t.Context(), persisted); err != nil {
		t.Fatal(err)
	}
	finishLifecycleDeletion(t, r, key)
	replacement := lifecycleApp(app.Name)
	replacement.UID = "replacement-application"
	replacement.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{}
	if err := c.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	persisted = settleLifecycle(t, r, key)
	credential := inventoryResource(t, persisted, "PostgresCredential", "app")
	database := inventoryResource(t, persisted, "PostgresDatabase", "infrastructure")
	if credential.Name == previousCredential.Name || credential.UID == previousCredential.UID {
		t.Fatal("same-name recreation reused previous ephemeral resource ownership")
	}
	if database.Name != previousDatabase.Name || database.UID != previousDatabase.UID || previousDataName == "" || persisted.Status.SQLDatabaseName != previousDataName {
		t.Fatal("same-name recreation replaced persistent database identity")
	}
	var created dboperator.PostgresCredential
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: credential.Namespace, Name: credential.Name}, &created); err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(&created); owner == nil || owner.Kind != "ResourceInventory" || owner.Name != provisioning.InventoryName(replacement.UID) {
		t.Fatal("replacement credential is not owned by the new Application UID")
	}
}

type lifecycleFailingInventoryClient struct {
	client.Client
	fail func(*wasmplatformv1alpha1.ResourceInventory) bool
}

func (c lifecycleFailingInventoryClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if inventory, ok := obj.(*wasmplatformv1alpha1.ResourceInventory); ok && c.fail(inventory) {
		return errors.New("inventory persistence failed")
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestReconcileRequiresInventoryIntentBeforeCreation(t *testing.T) {
	app := lifecycleApp("intent-failure")
	app.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	key := client.ObjectKeyFromObject(app)
	if _, err := reconcileLifecycle(t, r, key); err != nil {
		t.Fatal(err)
	}
	readyLifecycleBackend(t, r)
	r.Client = lifecycleFailingInventoryClient{Client: c, fail: func(inventory *wasmplatformv1alpha1.ResourceInventory) bool {
		for _, record := range inventory.Spec.Resources {
			if record.Kind == "PostgresCredential" {
				return true
			}
		}
		return false
	}}
	if _, err := reconcileLifecycle(t, r, key); err == nil {
		t.Fatal("expected inventory persistence to fail")
	}
	var credentials dboperator.PostgresCredentialList
	if err := c.List(t.Context(), &credentials); err != nil {
		t.Fatal(err)
	}
	if len(credentials.Items) != 0 || len(r.Store.Snapshot()) != 0 {
		t.Fatal("unpersisted intent created credentials or published configuration")
	}
	r.Client = c
	persisted := settleLifecycle(t, r, key)
	inventoryResource(t, persisted, "PostgresCredential", "app")
}

func TestReconcileRecoversCreationBeforeUIDPersistence(t *testing.T) {
	app := lifecycleApp("uid-persistence-failure")
	app.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	key := client.ObjectKeyFromObject(app)
	if _, err := reconcileLifecycle(t, r, key); err != nil {
		t.Fatal(err)
	}
	readyLifecycleBackend(t, r)
	r.Client = lifecycleFailingInventoryClient{Client: c, fail: func(inventory *wasmplatformv1alpha1.ResourceInventory) bool {
		for _, record := range inventory.Spec.Resources {
			if record.Kind == "PostgresCredential" && record.UID != "" {
				return true
			}
		}
		return false
	}}
	if _, err := reconcileLifecycle(t, r, key); err == nil {
		t.Fatal("expected resource UID persistence to fail")
	}
	var credentials dboperator.PostgresCredentialList
	if err := c.List(t.Context(), &credentials); err != nil {
		t.Fatal(err)
	}
	if len(credentials.Items) != 1 || len(r.Store.Snapshot()) != 0 {
		t.Fatal("failed resource UID persistence created extra credentials or published configuration")
	}
	persisted := getLifecycleApp(t, c, key)
	inventory, err := r.readInventory(t.Context(), persisted)
	if err != nil {
		t.Fatal(err)
	}
	var intent *wasmplatformv1alpha1.ResourceBinding
	for i := range inventory.Spec.Resources {
		if inventory.Spec.Resources[i].Kind == "PostgresCredential" {
			intent = &inventory.Spec.Resources[i]
		}
	}
	if intent == nil || intent.UID != "" || intent.Name != credentials.Items[0].Name {
		t.Fatalf("creation intent was not durable: %+v", intent)
	}
	restarted := artifactReconciler(c, r.Artifacts)
	restarted.Scheme, restarted.Config = r.Scheme, r.Config
	persisted = settleLifecycle(t, restarted, key)
	record := inventoryResource(t, persisted, "PostgresCredential", "app")
	if record.Name != credentials.Items[0].Name || types.UID(record.UID) != credentials.Items[0].UID {
		t.Fatal("restart did not recover the created credential from its persisted intent")
	}
	if err := c.List(t.Context(), &credentials); err != nil || len(credentials.Items) != 1 {
		t.Fatalf("restart created another credential: %v", err)
	}
}

func TestReconcileLegacyCredentialRequiresAttestationAndExactIdentity(t *testing.T) {
	for _, scenario := range []string{"unattested", "attested", "changed privileges", "foreign controller"} {
		t.Run(scenario, func(t *testing.T) {
			app := lifecycleApp("legacy-credential")
			app.Spec.SQL = &wasmplatformv1alpha1.SQLSpec{}
			r, c := lifecycleReconciler(t, app)
			key := client.ObjectKeyFromObject(app)
			if _, err := reconcileLifecycle(t, r, key); err != nil {
				t.Fatal(err)
			}
			readyLifecycleBackend(t, r)
			name := K8sCredentialName(app.Namespace, app.Name, "app")
			legacy := &dboperator.PostgresCredential{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace, UID: "legacy-resource"}, Spec: dboperator.PostgresCredentialSpec{
				DatabaseRef: postgresDatabaseCRName(app.Namespace), Username: PGUsername(app.Namespace, app.Name, "app"), SecretName: name + "-creds",
				Permissions: []dboperator.DatabasePermissionEntry{{Databases: []string{PGDatabaseName(app.Namespace, app.Name)}, Permissions: []dboperator.DatabasePermission{dboperator.PermissionAll}}},
			}}
			if scenario != "unattested" {
				legacy.Annotations = map[string]string{"wasm-platform.io/legacy-application-uid": string(app.UID)}
			}
			if scenario == "changed privileges" {
				legacy.Spec.Permissions[0].Permissions = []dboperator.DatabasePermission{dboperator.PermissionSelect}
			}
			if scenario == "foreign controller" {
				foreign := app.DeepCopy()
				foreign.UID = "foreign-application"
				if err := controllerutil.SetControllerReference(foreign, legacy, r.Scheme); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Create(t.Context(), legacy); err != nil {
				t.Fatal(err)
			}
			before := legacy.DeepCopy()
			if scenario == "attested" {
				persisted := settleLifecycle(t, r, key)
				record := inventoryResource(t, persisted, "PostgresCredential", "app")
				if record.Name != name || types.UID(record.UID) != legacy.UID {
					t.Fatal("attested legacy credential was replaced rather than reclaimed")
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(legacy), legacy); err != nil {
					t.Fatal(err)
				}
				if owner := metav1.GetControllerOf(legacy); owner == nil || owner.Kind != "ResourceInventory" || owner.Name != provisioning.InventoryName(app.UID) {
					t.Fatal("reclaimed legacy credential has no verified Application owner")
				}
				return
			}
			if _, err := reconcileLifecycle(t, r, key); err == nil {
				t.Fatal("unverified legacy credential did not block reconciliation")
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(legacy), legacy); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(legacy.Spec, before.Spec) || !reflect.DeepEqual(legacy.OwnerReferences, before.OwnerReferences) || !legacy.DeletionTimestamp.IsZero() {
				t.Fatal("unverified legacy credential was changed")
			}
			if len(r.Store.Snapshot()) != 0 {
				t.Fatal("unverified legacy credential was published")
			}
		})
	}
}
