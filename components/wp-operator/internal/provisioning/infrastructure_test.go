package provisioning

import (
	"testing"
	"time"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func infrastructureTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dboperator.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestSharedInfrastructureReclaimsMatchingLegacyResources(t *testing.T) {
	postgres := &dboperator.PostgresDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "wasm-apps-postgres", UID: "postgres-uid", Labels: map[string]string{managedByLabel: platformManager}},
		Spec:       dboperator.PostgresDatabaseSpec{PostgresVersion: "16", StorageSize: resource.MustParse("1024Mi")},
	}
	redis := &dboperator.RedisDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: redisDatabaseName, UID: "redis-uid", Labels: map[string]string{managedByLabel: platformManager}},
		Spec:       dboperator.RedisDatabaseSpec{StorageSize: resource.MustParse("1Gi")},
	}
	credential := &dboperator.RedisCredential{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: redisCredentialName, UID: "credential-uid", Labels: map[string]string{managedByLabel: platformManager}},
		Spec:       dboperator.RedisCredentialSpec{DatabaseRef: redisDatabaseName, Username: "execution-host", SecretName: redisSecretName, KeyPatterns: []string{"*"}, ACLCategories: []dboperator.RedisACLCategory{dboperator.RedisACLCategoryAll}},
	}
	kubernetes := infrastructureTestClient(t, postgres, redis, credential)
	resources := New(kubernetes)
	pg, err := resources.EnsurePostgres(t.Context(), "apps", "16", "1Gi", "")
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := resources.EnsureRedis(t.Context(), "platform", "1Gi", "")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := resources.EnsureRedisCredential(t.Context(), "platform", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		object client.Object
		uid    types.UID
		scope  string
	}{
		{pg, postgres.UID, "postgres/apps"},
		{rdb, redis.UID, "redis/platform"},
		{cred, credential.UID, "redis-credential/platform"},
	} {
		if check.object.GetUID() != check.uid || check.object.GetAnnotations()[resourceScopeAnnotation] != check.scope {
			t.Fatalf("legacy resource %s was replaced or not scoped", client.ObjectKeyFromObject(check.object))
		}
		observed := check.object.DeepCopyObject().(client.Object)
		if err := kubernetes.Get(t.Context(), client.ObjectKeyFromObject(check.object), observed); err != nil {
			t.Fatal(err)
		}
		if observed.GetAnnotations()[resourceScopeAnnotation] != check.scope {
			t.Fatal("scope was not persisted")
		}
	}
}

func TestSharedInfrastructureRejectsReplacedUIDBeforeAdoption(t *testing.T) {
	postgres := &dboperator.PostgresDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "wasm-apps-postgres", UID: "replacement", Labels: map[string]string{managedByLabel: platformManager}},
		Spec:       dboperator.PostgresDatabaseSpec{PostgresVersion: "16", StorageSize: resource.MustParse("1Gi")},
	}
	kubernetes := infrastructureTestClient(t, postgres)
	if _, err := New(kubernetes).EnsurePostgres(t.Context(), "apps", "16", "1Gi", "original"); err == nil {
		t.Fatal("replaced shared resource accepted")
	}
	if err := kubernetes.Get(t.Context(), client.ObjectKeyFromObject(postgres), postgres); err != nil {
		t.Fatal(err)
	}
	if postgres.Annotations[resourceScopeAnnotation] != "" {
		t.Fatal("replaced shared resource was adopted")
	}
}

func TestSharedInfrastructureRejectsUnverifiedReclamation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*dboperator.PostgresDatabase)
	}{
		{"unmanaged", func(database *dboperator.PostgresDatabase) { database.Labels = nil }},
		{"wrong-manager", func(database *dboperator.PostgresDatabase) { database.Labels[managedByLabel] = "another-operator" }},
		{"wrong-scope", func(database *dboperator.PostgresDatabase) {
			database.Annotations = map[string]string{resourceScopeAnnotation: "postgres/other"}
		}},
		{"foreign-controller", func(database *dboperator.PostgresDatabase) {
			controller := true
			database.OwnerReferences = []metav1.OwnerReference{{APIVersion: "wasm-platform.io/v1alpha1", Kind: "Application", Name: "other", UID: "other", Controller: &controller}}
		}},
		{"non-controller-owner", func(database *dboperator.PostgresDatabase) {
			database.OwnerReferences = []metav1.OwnerReference{{APIVersion: "wasm-platform.io/v1alpha1", Kind: "Application", Name: "other", UID: "other"}}
		}},
		{"wrong-version", func(database *dboperator.PostgresDatabase) { database.Spec.PostgresVersion = "17" }},
		{"wrong-storage", func(database *dboperator.PostgresDatabase) { database.Spec.StorageSize = resource.MustParse("2Gi") }},
		{"deleting", func(database *dboperator.PostgresDatabase) {
			database.Finalizers = []string{"test.example/hold"}
			now := metav1.NewTime(time.Now())
			database.DeletionTimestamp = &now
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := &dboperator.PostgresDatabase{
				ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "wasm-apps-postgres", UID: "existing", Labels: map[string]string{managedByLabel: platformManager}},
				Spec:       dboperator.PostgresDatabaseSpec{PostgresVersion: "16", StorageSize: resource.MustParse("1Gi")},
			}
			test.mutate(database)
			kubernetes := infrastructureTestClient(t, database)
			if _, err := New(kubernetes).EnsurePostgres(t.Context(), "apps", "16", "1Gi", ""); err == nil {
				t.Fatal("unverified shared database was reclaimed")
			}
			observed := &dboperator.PostgresDatabase{}
			if err := kubernetes.Get(t.Context(), client.ObjectKeyFromObject(database), observed); err != nil {
				t.Fatal(err)
			}
			if observed.ResourceVersion != database.ResourceVersion && database.ResourceVersion != "" {
				t.Fatal("rejected database was changed")
			}
			if observed.Annotations[resourceScopeAnnotation] != database.Annotations[resourceScopeAnnotation] {
				t.Fatal("rejected database was stamped with ownership")
			}
		})
	}
}

func TestSharedRedisCredentialRejectsUnexpectedPrivileges(t *testing.T) {
	credential := &dboperator.RedisCredential{
		ObjectMeta: sharedResourceMetadata("platform", redisCredentialName, "redis-credential/platform"),
		Spec:       dboperator.RedisCredentialSpec{DatabaseRef: redisDatabaseName, Username: "execution-host", SecretName: redisSecretName, KeyPatterns: []string{"*"}, ACLCategories: []dboperator.RedisACLCategory{dboperator.RedisACLCategoryAll}, Commands: []string{"flushall"}},
	}
	kubernetes := infrastructureTestClient(t, credential)
	if _, err := New(kubernetes).EnsureRedisCredential(t.Context(), "platform", ""); err == nil {
		t.Fatal("Redis credential with unexpected commands was accepted")
	}
}

func TestSharedRedisCredentialRejectsExistingSecretWithoutCredential(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: redisSecretName, UID: "existing-secret"}}
	kubernetes := infrastructureTestClient(t, secret)
	if _, err := New(kubernetes).EnsureRedisCredential(t.Context(), "platform", ""); err == nil {
		t.Fatal("Redis credential was created over a preexisting Secret")
	}
	credentials := &dboperator.RedisCredentialList{}
	if err := kubernetes.List(t.Context(), credentials); err != nil {
		t.Fatal(err)
	}
	if len(credentials.Items) != 0 {
		t.Fatal("rejected Redis credential was created")
	}
	observed := &corev1.Secret{}
	if err := kubernetes.Get(t.Context(), client.ObjectKeyFromObject(secret), observed); err != nil {
		t.Fatal(err)
	}
	if observed.UID != secret.UID || len(observed.OwnerReferences) != 0 {
		t.Fatal("existing Secret was claimed or replaced")
	}
}

func TestRedisSecretRequiresExactCredentialOwnership(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*corev1.Secret)
		wantError bool
	}{
		{"matching", func(_ *corev1.Secret) {}, false},
		{"unowned", func(secret *corev1.Secret) { secret.OwnerReferences = nil }, true},
		{"prior-credential", func(secret *corev1.Secret) { secret.OwnerReferences[0].UID = "old-credential" }, true},
		{"wrong-kind", func(secret *corev1.Secret) { secret.OwnerReferences[0].Kind = "PostgresCredential" }, true},
		{"wrong-api", func(secret *corev1.Secret) { secret.OwnerReferences[0].APIVersion = "foreign.example/v1" }, true},
		{"wrong-name", func(secret *corev1.Secret) { secret.OwnerReferences[0].Name = "another-credential" }, true},
		{"non-controller", func(secret *corev1.Secret) { controller := false; secret.OwnerReferences[0].Controller = &controller }, true},
		{"wrong-username", func(secret *corev1.Secret) { secret.Data["REDIS_USERNAME"] = []byte("another-user") }, true},
		{"deleting", func(secret *corev1.Secret) {
			secret.Finalizers = []string{"test.example/hold"}
			now := metav1.NewTime(time.Now())
			secret.DeletionTimestamp = &now
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credential := &dboperator.RedisCredential{ObjectMeta: sharedResourceMetadata("platform", redisCredentialName, "redis-credential/platform"), Spec: dboperator.RedisCredentialSpec{Username: "execution-host", SecretName: redisSecretName}}
			credential.UID = "current-credential"
			controller := true
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: credential.Namespace, Name: credential.Spec.SecretName, OwnerReferences: []metav1.OwnerReference{{APIVersion: dboperator.GroupVersion.String(), Kind: "RedisCredential", Name: credential.Name, UID: credential.UID, Controller: &controller}}},
				Data:       map[string][]byte{"REDIS_USERNAME": []byte("execution-host")},
			}
			test.mutate(secret)
			_, err := New(infrastructureTestClient(t, secret)).RedisSecret(t.Context(), credential)
			if (err != nil) != test.wantError {
				t.Fatalf("Redis Secret ownership check error = %v, want error = %v", err, test.wantError)
			}
		})
	}
}
