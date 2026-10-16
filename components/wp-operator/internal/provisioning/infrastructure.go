package provisioning

import (
	"context"
	"fmt"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	platform "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	resourceScopeAnnotation = "wasm-platform.io/resource-scope"
	managedByLabel          = "app.kubernetes.io/managed-by"
	platformManager         = "wp-operator"
	redisDatabaseName       = "wasm-platform-redis"
	redisCredentialName     = "wasm-platform-redis-execution-host"
	redisSecretName         = "execution-host-redis-credentials"
)

// Resources owns Kubernetes provisioning and observation of platform resources.
type Resources struct {
	inventory *platform.ResourceInventory
	client    client.Client
	apiReader client.Reader
}

func New(kubernetes client.Client) *Resources {
	return NewWithReader(kubernetes, kubernetes)
}

// NewWithReader confirms cached absence against the API before creation or reclamation.
func NewWithReader(kubernetes client.Client, apiReader client.Reader) *Resources {
	return &Resources{client: kubernetes, apiReader: apiReader}
}

func (r *Resources) getObserved(ctx context.Context, key client.ObjectKey, object client.Object) error {
	err := r.client.Get(ctx, key, object)
	if apierrors.IsNotFound(err) {
		return r.apiReader.Get(ctx, key, object)
	}
	return err
}

// EnsurePostgres verifies or creates the retained PostgreSQL instance for a namespace.
func (r *Resources) EnsurePostgres(ctx context.Context, namespace, version, storage, expectedUID string) (*dboperator.PostgresDatabase, error) {
	if version == "" {
		version = "16"
	}
	if storage == "" {
		storage = "1Gi"
	}
	quantity, err := resource.ParseQuantity(storage)
	if err != nil {
		return nil, fmt.Errorf("parsing PostgreSQL storage size: %w", err)
	}
	scope := "postgres/" + namespace
	expected := dboperator.PostgresDatabaseSpec{PostgresVersion: version, StorageSize: quantity}
	desired := &dboperator.PostgresDatabase{
		ObjectMeta: sharedResourceMetadata(namespace, "wasm-"+namespace+"-postgres", scope),
		Spec:       expected,
	}
	database := &dboperator.PostgresDatabase{}
	created, err := r.ensureSharedResource(ctx, database, desired, scope, expectedUID, nil, func() bool {
		return database.Spec.PostgresVersion == expected.PostgresVersion && database.Spec.StorageSize.Cmp(expected.StorageSize) == 0
	})
	if err != nil {
		return nil, err
	}
	if created {
		return desired, nil
	}
	return database, nil
}

// EnsureRedis verifies or creates the retained shared Redis instance.
func (r *Resources) EnsureRedis(ctx context.Context, namespace, storage, expectedUID string) (*dboperator.RedisDatabase, error) {
	if storage == "" {
		storage = "1Gi"
	}
	quantity, err := resource.ParseQuantity(storage)
	if err != nil {
		return nil, fmt.Errorf("parsing Redis storage size: %w", err)
	}
	scope := "redis/" + namespace
	desired := &dboperator.RedisDatabase{
		ObjectMeta: sharedResourceMetadata(namespace, redisDatabaseName, scope),
		Spec:       dboperator.RedisDatabaseSpec{StorageSize: quantity},
	}
	database := &dboperator.RedisDatabase{}
	created, err := r.ensureSharedResource(ctx, database, desired, scope, expectedUID, nil, func() bool {
		return database.Spec.StorageSize.Cmp(quantity) == 0
	})
	if err != nil {
		return nil, err
	}
	if created {
		return desired, nil
	}
	return database, nil
}

// EnsureRedisCredential verifies or creates the shared execution-host credential.
func (r *Resources) EnsureRedisCredential(ctx context.Context, namespace, expectedUID string) (*dboperator.RedisCredential, error) {
	scope := "redis-credential/" + namespace
	expected := dboperator.RedisCredentialSpec{
		DatabaseRef:   redisDatabaseName,
		Username:      "execution-host",
		SecretName:    redisSecretName,
		KeyPatterns:   []string{"*"},
		ACLCategories: []dboperator.RedisACLCategory{dboperator.RedisACLCategoryAll},
	}
	desired := &dboperator.RedisCredential{
		ObjectMeta: sharedResourceMetadata(namespace, redisCredentialName, scope),
		Spec:       expected,
	}
	credential := &dboperator.RedisCredential{}
	created, err := r.ensureSharedResource(ctx, credential, desired, scope, expectedUID, func() error {
		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: namespace, Name: redisSecretName}
		if err := r.getObserved(ctx, key, secret); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("checking Redis credential Secret %s: %w", key, err)
		}
		return fmt.Errorf("Redis credential Secret %s exists without its expected credential", key)
	}, func() bool {
		return apiequality.Semantic.DeepEqual(credential.Spec, expected)
	})
	if err != nil {
		return nil, err
	}
	if created {
		return desired, nil
	}
	return credential, nil
}

// RedisSecret reads a credential only after verifying the downstream owner and username.
func (r *Resources) RedisSecret(ctx context.Context, credential *dboperator.RedisCredential) (*corev1.Secret, error) {
	if credential.UID == "" || !credential.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("RedisCredential %s/%s has no usable identity", credential.Namespace, credential.Name)
	}
	if err := verifySharedResource(credential, "redis-credential/"+credential.Namespace); err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: credential.Namespace, Name: credential.Spec.SecretName}
	if err := r.client.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("getting Redis credential Secret %s: %w", key, err)
	}
	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.UID != credential.UID || owner.Name != credential.Name || owner.Kind != "RedisCredential" || owner.APIVersion != dboperator.GroupVersion.String() {
		return nil, fmt.Errorf("Redis credential Secret %s has an unexpected owner", key)
	}
	if !secret.DeletionTimestamp.IsZero() || string(secret.Data["REDIS_USERNAME"]) != credential.Spec.Username {
		return nil, fmt.Errorf("Redis credential Secret %s has an unexpected identity", key)
	}
	return secret, nil
}

func sharedResourceMetadata(namespace, name, scope string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Namespace:   namespace,
		Name:        name,
		Labels:      map[string]string{managedByLabel: platformManager},
		Annotations: map[string]string{resourceScopeAnnotation: scope},
	}
}

func (r *Resources) ensureSharedResource(ctx context.Context, object, desired client.Object, scope, expectedUID string, validateCreate func() error, matchesExpectedSpec func() bool) (bool, error) {
	key := client.ObjectKeyFromObject(desired)
	err := r.getObserved(ctx, key, object)
	if apierrors.IsNotFound(err) {
		if expectedUID != "" {
			return false, fmt.Errorf("inventoried shared resource %s disappeared", key)
		}
		if validateCreate != nil {
			if err := validateCreate(); err != nil {
				return false, err
			}
		}
		if err = r.client.Create(ctx, desired); err == nil {
			return true, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("creating shared resource %s: %w", key, err)
		}
		err = r.getObserved(ctx, key, object)
	}
	if err != nil {
		return false, fmt.Errorf("getting shared resource %s: %w", key, err)
	}
	if expectedUID != "" && string(object.GetUID()) != expectedUID {
		return false, fmt.Errorf("shared resource %s UID differs from inventory", key)
	}
	if err := verifySharedResource(object, scope); err != nil {
		return false, err
	}
	if !matchesExpectedSpec() {
		return false, fmt.Errorf("shared resource %s does not match the expected specification", key)
	}
	if object.GetAnnotations()[resourceScopeAnnotation] == scope {
		return false, nil
	}
	previous := object.DeepCopyObject().(client.Object)
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[resourceScopeAnnotation] = scope
	object.SetAnnotations(annotations)
	if err := r.client.Patch(ctx, object, client.MergeFromWithOptions(previous, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("claiming shared resource %s: %w", key, err)
	}
	return false, nil
}

func verifySharedResource(object client.Object, scope string) error {
	key := client.ObjectKeyFromObject(object)
	if !object.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("shared resource %s is being deleted", key)
	}
	if object.GetLabels()[managedByLabel] != platformManager {
		return fmt.Errorf("shared resource %s is not managed by wp-operator", key)
	}
	if len(object.GetOwnerReferences()) != 0 {
		return fmt.Errorf("shared resource %s has a foreign owner", key)
	}
	if ownerScope := object.GetAnnotations()[resourceScopeAnnotation]; ownerScope != "" && ownerScope != scope {
		return fmt.Errorf("shared resource %s belongs to another platform scope", key)
	}
	return nil
}
