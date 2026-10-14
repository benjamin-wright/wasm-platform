// Package artifacts resolves approved OCI modules to immutable identities.
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	MaxManifestBytes = 1 << 20
	MaxWasmBytes     = 16 << 20
	RequestTimeout   = 10 * time.Second
)

var (
	digestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagPattern        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// Identity identifies an OCI manifest and its single raw Wasm layer.
type Identity struct {
	ModuleRef      string
	ManifestDigest string
	WasmDigest     string
}

// Resolver is the artifact boundary consumed by application reconciliation.
type Resolver interface {
	Resolve(context.Context, string) (Identity, error)
	Validate(string) error
}

// ValidateIdentity checks that an identity contains a pinned manifest and raw digest.
func ValidateIdentity(identity Identity) error {
	ref, err := parseReference(identity.ModuleRef)
	if err != nil || !digestPattern.MatchString(identity.ManifestDigest) ||
		!digestPattern.MatchString(identity.WasmDigest) || ref.selector != identity.ManifestDigest {
		return errors.New("invalid resolved OCI module identity")
	}
	return nil
}

// Registry resolves modules anonymously through a finite, origin-restricted client.
type Registry struct {
	approved  map[string]bool
	plaintext map[string]bool
	client    *http.Client
}

// NewRegistry validates exact registry authorities and the local plaintext subset.
func NewRegistry(approved, plaintext string) (*Registry, error) {
	approvedSet, err := registrySet(approved)
	if err != nil || len(approvedSet) == 0 {
		return nil, errors.New("APPROVED_REGISTRIES must contain valid registry authorities")
	}
	plaintextSet, err := registrySet(plaintext)
	if err != nil {
		return nil, errors.New("LOCAL_PLAINTEXT_REGISTRIES must contain valid registry authorities")
	}
	for authority := range plaintextSet {
		if !approvedSet[authority] || !localAuthority(authority) {
			return nil, errors.New("LOCAL_PLAINTEXT_REGISTRIES must be an approved local registry subset")
		}
	}
	return &Registry{
		approved:  approvedSet,
		plaintext: plaintextSet,
		client: &http.Client{
			Timeout: RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func registrySet(value string) (map[string]bool, error) {
	set := make(map[string]bool)
	if value == "" {
		return set, nil
	}
	for _, authority := range strings.Split(value, ",") {
		if !validAuthority(authority) {
			return nil, errors.New("invalid registry authority")
		}
		set[authority] = true
	}
	return set, nil
}

func validAuthority(authority string) bool {
	if authority == "" || authority != strings.ToLower(authority) || strings.ContainsAny(authority, "/@?#%\\ \t\r\n") {
		return false
	}
	u, err := url.Parse("https://" + authority)
	if err != nil || u.Host != authority || u.Hostname() == "" || u.User != nil {
		return false
	}
	if strings.Contains(u.Hostname(), ":") && net.ParseIP(u.Hostname()) == nil {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return !strings.HasSuffix(authority, ":")
}

func localAuthority(authority string) bool {
	u, _ := url.Parse("https://" + authority)
	host := u.Hostname()
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host).IsLoopback()
}

type reference struct {
	authority  string
	repository string
	selector   string
}

func parseReference(value string) (reference, error) {
	bare := strings.TrimPrefix(value, "oci://")
	if len(bare) > 2048 || strings.ContainsAny(bare, "?#%\\ \t\r\n") {
		return reference{}, errors.New("invalid OCI module reference")
	}
	slash := strings.IndexByte(bare, '/')
	if slash < 1 {
		return reference{}, errors.New("invalid OCI module reference")
	}
	ref := reference{authority: bare[:slash], repository: bare[slash+1:], selector: "latest"}
	if !validAuthority(ref.authority) {
		return reference{}, errors.New("invalid OCI module reference")
	}
	if at := strings.IndexByte(ref.repository, '@'); at >= 0 {
		ref.selector = ref.repository[at+1:]
		ref.repository = ref.repository[:at]
		if !digestPattern.MatchString(ref.selector) {
			return reference{}, errors.New("invalid OCI module digest")
		}
	} else if colon := strings.LastIndexByte(ref.repository, ':'); colon >= 0 {
		ref.selector = ref.repository[colon+1:]
		ref.repository = ref.repository[:colon]
		if !tagPattern.MatchString(ref.selector) {
			return reference{}, errors.New("invalid OCI module tag")
		}
	}
	if len(ref.repository) > 255 || !repositoryPattern.MatchString(ref.repository) {
		return reference{}, errors.New("invalid OCI module repository")
	}
	return ref, nil
}

// Validate checks the reference and its origin without fetching registry data.
func (r *Registry) Validate(value string) error {
	ref, err := parseReference(value)
	if err != nil {
		return err
	}
	if !r.approved[ref.authority] {
		return errors.New("OCI module registry is not approved")
	}
	return nil
}

// Resolve verifies the manifest bytes and bounded raw Wasm descriptor before pinning.
func (r *Registry) Resolve(ctx context.Context, value string) (Identity, error) {
	if err := r.Validate(value); err != nil {
		return Identity{}, err
	}
	ref, _ := parseReference(value)
	scheme := "https"
	if r.plaintext[ref.authority] {
		scheme = "http"
	}
	endpoint := url.URL{Scheme: scheme, Host: ref.authority, Path: "/v2/" + ref.repository + "/manifests/" + ref.selector}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Identity{}, errors.New("OCI manifest request failed")
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	response, err := r.client.Do(req)
	if err != nil {
		return Identity{}, errors.New("OCI manifest request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Identity{}, errors.New("OCI manifest request was rejected")
	}
	if response.ContentLength > MaxManifestBytes {
		return Identity{}, errors.New("OCI manifest exceeds byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxManifestBytes+1))
	if err != nil {
		return Identity{}, errors.New("OCI manifest response failed")
	}
	if len(data) > MaxManifestBytes {
		return Identity{}, errors.New("OCI manifest exceeds byte limit")
	}
	hash := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(hash[:])
	if strings.HasPrefix(ref.selector, "sha256:") && ref.selector != digest {
		return Identity{}, errors.New("OCI manifest digest mismatch")
	}
	if supplied := response.Header.Get("Docker-Content-Digest"); supplied != "" && supplied != digest {
		return Identity{}, errors.New("OCI manifest digest mismatch")
	}
	var manifest struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		ArtifactType  string `json:"artifactType"`
		Config        *struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      *int64 `json:"size"`
		} `json:"config"`
		Layers []struct {
			MediaType string   `json:"mediaType"`
			Digest    string   `json:"digest"`
			Size      int64    `json:"size"`
			URLs      []string `json:"urls"`
		} `json:"layers"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 2 ||
		(manifest.MediaType != "application/vnd.oci.image.manifest.v1+json" && manifest.MediaType != "application/vnd.docker.distribution.manifest.v2+json") || len(manifest.Layers) != 1 {
		return Identity{}, errors.New("OCI module requires a single-layer image manifest")
	}
	if manifest.Config == nil || manifest.Config.MediaType == "" || !digestPattern.MatchString(manifest.Config.Digest) || manifest.Config.Size == nil || *manifest.Config.Size < 0 {
		return Identity{}, errors.New("OCI module has an invalid config descriptor")
	}
	layer := manifest.Layers[0]
	wasmLayer := layer.MediaType == "application/wasm" || layer.MediaType == "application/vnd.wasm.content.layer.v1+wasm" || layer.MediaType == "application/octet-stream" ||
		(layer.MediaType == "application/vnd.oci.image.layer.v1.tar" && manifest.ArtifactType == "application/vnd.wasm.content.layer.v1+wasm")
	if !wasmLayer ||
		!digestPattern.MatchString(layer.Digest) || layer.Size < 8 || layer.Size > MaxWasmBytes || len(layer.URLs) != 0 {
		return Identity{}, errors.New("OCI module has an unsupported Wasm layer")
	}
	return Identity{
		ModuleRef:      "oci://" + ref.authority + "/" + ref.repository + "@" + digest,
		ManifestDigest: digest,
		WasmDigest:     layer.Digest,
	}, nil
}
