package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
)

var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(wasmplatformv1alpha1.AddToScheme(s))
	return s
}()

// fakeClientWithIndex builds a fake client pre-loaded with the given
// Application objects and the spec.topic field index registered.
func fakeClientWithIndex(apps ...*wasmplatformv1alpha1.Application) client.Client {
	objs := make([]client.Object, len(apps))
	for i, a := range apps {
		objs[i] = a
	}
	return fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objs...).
		WithIndex(
			&wasmplatformv1alpha1.Application{},
			topicIndexField,
			func(obj client.Object) []string {
				a := obj.(*wasmplatformv1alpha1.Application)
				return []string{a.Spec.Topic}
			},
		).
		Build()
}

func makeApp(ns, name, topic string, ts time.Time) *wasmplatformv1alpha1.Application {
	return &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(ts),
		},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Module: "oci://example.com/app@sha256:aaaa",
			Topic:  topic,
		},
	}
}

var (
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
)

func TestFindTopicOwner(t *testing.T) {
	tests := []struct {
		name      string
		self      *wasmplatformv1alpha1.Application
		others    []*wasmplatformv1alpha1.Application
		wantOwner string // "ns/name", or "" if self is the rightful owner
	}{
		{
			name:      "sole owner — no conflict",
			self:      makeApp("default", "my-app", "sole.topic", t0),
			others:    nil,
			wantOwner: "",
		},
		{
			name:      "older app owns the topic",
			self:      makeApp("default", "my-app", "shared.topic", t1),
			others:    []*wasmplatformv1alpha1.Application{makeApp("default", "other-app", "shared.topic", t0)},
			wantOwner: "default/other-app",
		},
		{
			name:      "self is older — self owns the topic",
			self:      makeApp("default", "my-app", "shared.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeApp("default", "other-app", "shared.topic", t1)},
			wantOwner: "",
		},
		{
			name:      "same timestamp, lower name wins — self blocked",
			self:      makeApp("default", "z-app", "tied.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeApp("default", "a-app", "tied.topic", t0)},
			wantOwner: "default/a-app",
		},
		{
			name:      "same timestamp, self has lower name — self owns",
			self:      makeApp("default", "a-app", "tied.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeApp("default", "z-app", "tied.topic", t0)},
			wantOwner: "",
		},
		{
			name: "three-way race — oldest wins",
			self: makeApp("ns2", "app2", "race.topic", t2),
			others: []*wasmplatformv1alpha1.Application{
				makeApp("ns1", "app1", "race.topic", t0), // oldest — wins
				makeApp("ns3", "app3", "race.topic", t1),
			},
			wantOwner: "ns1/app1",
		},
		{
			name: "three-way race — self is oldest",
			self: makeApp("ns1", "app1", "race.topic", t0),
			others: []*wasmplatformv1alpha1.Application{
				makeApp("ns2", "app2", "race.topic", t1),
				makeApp("ns3", "app3", "race.topic", t2),
			},
			wantOwner: "",
		},
		{
			name:      "cross-namespace, lexicographic namespace tiebreak",
			self:      makeApp("z-ns", "app", "ns.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeApp("a-ns", "app", "ns.topic", t0)},
			wantOwner: "a-ns/app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			all := append([]*wasmplatformv1alpha1.Application{tt.self}, tt.others...)
			c := fakeClientWithIndex(all...)

			owner, err := findTopicOwner(context.Background(), c, tt.self.Spec.Topic, tt.self)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantOwner == "" {
				if owner != nil {
					t.Errorf("expected self to be owner, got %s/%s", owner.Namespace, owner.Name)
				}
				return
			}

			if owner == nil {
				t.Fatalf("expected owner %s, got nil (self is being returned as owner)", tt.wantOwner)
			}
			got := owner.Namespace + "/" + owner.Name
			if got != tt.wantOwner {
				t.Errorf("expected owner %s, got %s", tt.wantOwner, got)
			}
		})
	}
}

//			}				t.Errorf("expected owner %s, got %s", tt.wantOwner, got)			if got != tt.wantOwner {			got := owner.Namespace + "/" + owner.Name			}				t.Fatalf("expected owner %s, got nil (self is being returned as owner)", tt.wantOwner)			if owner == nil {			}				return				}					t.Errorf("expected self to be owner, got %s/%s", owner.Namespace, owner.Name)				if owner != nil {			if tt.wantOwner == "" {			}				t.Fatalf("unexpected error: %v", err)			if err != nil {			owner, err := findTopicOwner(context.Background(), c, tt.self.Spec.Topic, tt.self)			c := fakeClientWithIndex(all...)			all := append([]*wasmplatformv1alpha1.Application{tt.self}, tt.others...)		t.Run(tt.name, func(t *testing.T) {	for _, tt := range tests {	}		},			wantOwner: "a-ns/app",			},				makeApp("a-ns", "app", "ns.topic", t0),			others: []*wasmplatformv1alpha1.Application{			self: makeApp("z-ns", "app", "ns.topic", t0),			name: "cross-namespace, lexicographic namespace tiebreak",		{		},			wantOwner: "",			},				makeApp("ns3", "app3", "race.topic", t2),				makeApp("ns2", "app2", "race.topic", t1),			others: []*wasmplatformv1alpha1.Application{			self: makeApp("ns1", "app1", "race.topic", t0),			name: "three-way race — self is oldest",		{		},			wantOwner: "ns1/app1",			},				makeApp("ns3", "app3", "race.topic", t1),				makeApp("ns1", "app1", "race.topic", t0), // oldest — wins			others: []*wasmplatformv1alpha1.Application{			self: makeApp("ns2", "app2", "race.topic", t2),			name: "three-way race — oldest wins",		{		},			wantOwner: "",			others:    []*wasmplatformv1alpha1.Application{makeApp("default", "z-app", "tied.topic", t0)},			self:      makeApp("default", "a-app", "tied.topic", t0),			name:      "same timestamp, self has lower name — self owns",		{		},			wantOwner: "default/a-app",			others:    []*wasmplatformv1alpha1.Application{makeApp("default", "a-app", "tied.topic", t0)},			self:      makeApp("default", "z-app", "tied.topic", t0),			name:      "same timestamp, lower name wins — self blocked",		{		},			wantOwner: "",			others: []*wasmplatformv1alpha1.Application{makeApp("default", "other-app", "shared.topic", t1)},			self:   makeApp("default", "my-app", "shared.topic", t0),			name:   "self is older — self owns the topic",		{		},			wantOwner: "default/other-app",			others: []*wasmplatformv1alpha1.Application{makeApp("default", "other-app", "shared.topic", t0)},			self:   makeApp("default", "my-app", "shared.topic", t1),			name:   "older app owns the topic",		{		},			wantOwner: "",			others:    nil,			self:      makeApp("default", "my-app", "sole.topic", t0),			name:      "sole owner — no conflict",		{	}{		wantOwner string // "ns/name", or "" if self is the rightful owner		others    []*wasmplatformv1alpha1.Application		self      *wasmplatformv1alpha1.Application		name      string	tests := []struct {func TestFindTopicOwner(t *testing.T) {)	t2 = time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)	t1 = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)var (}	}		},			Topic:  topic,			Module: "oci://example.com/app@sha256:aaaa",		Spec: wasmplatformv1alpha1.ApplicationSpec{		},			CreationTimestamp: metav1.NewTime(ts),			Namespace:         ns,			Name:              name,		ObjectMeta: metav1.ObjectMeta{	return &wasmplatformv1alpha1.Application{func makeApp(ns, name, topic string, ts time.Time) *wasmplatformv1alpha1.Application {}		Build()		).			},				return []string{a.Spec.Topic}				a := obj.(*wasmplatformv1alpha1.Application)			func(obj client.Object) []string {			topicIndexField,			&wasmplatformv1alpha1.Application{},		WithIndex(		WithObjects(objs...).		WithScheme(testScheme).	return fake.NewClientBuilder().	}		objs[i] = a	for i, a := range apps {	objs := make([]client.Object, len(apps))func fakeClientWithIndex(apps ...*wasmplatformv1alpha1.Application) client.Client {// Application objects and the spec.topic field index registered.// fakeClientWithIndex builds a fake client pre-loaded with the given}()	return s	utilruntime.Must(wasmplatformv1alpha1.AddToScheme(s))
