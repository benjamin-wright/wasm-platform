package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const Audience = "wasm-platform"

var (
	ErrUnauthenticated = errors.New("workload token is not valid")
	ErrUnavailable     = errors.New("workload authentication is unavailable")
)

type Recipient struct {
	Username  string
	ExpiresAt time.Time
}

// Reviewer authenticates a projected workload token with the Kubernetes API.
type Reviewer interface {
	Review(context.Context, string) (Recipient, error)
}

type TokenReviewClient interface {
	Create(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

type KubernetesReviewer struct {
	client TokenReviewClient
}

func NewKubernetesReviewer(client TokenReviewClient) *KubernetesReviewer {
	return &KubernetesReviewer{client: client}
}

func (r *KubernetesReviewer) Review(ctx context.Context, token string) (Recipient, error) {
	if len(token) == 0 || len(token) > 16384 {
		return Recipient{}, ErrUnauthenticated
	}
	reviewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	review, err := r.client.Create(reviewCtx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return Recipient{}, ErrUnavailable
	}
	if !review.Status.Authenticated || review.Status.Error != "" || !slices.Contains(review.Status.Audiences, Audience) {
		return Recipient{}, ErrUnauthenticated
	}
	expiresAt, err := authenticatedExpiration(token)
	if err != nil || !expiresAt.After(time.Now()) {
		return Recipient{}, ErrUnauthenticated
	}
	return Recipient{Username: review.Status.User.Username, ExpiresAt: expiresAt}, nil
}

// The API has already verified the JWT. Its expiration only shortens session lifetime;
// local claim parsing never establishes authentication or recipient permissions.
func authenticatedExpiration(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, ErrUnauthenticated
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, ErrUnauthenticated
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 {
		return time.Time{}, ErrUnauthenticated
	}
	return time.Unix(claims.ExpiresAt, 0), nil
}
