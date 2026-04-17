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
// Application objects and the spec.functions.topic field index registered.
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
				var topics []string
				for _, fn := range a.Spec.Functions {
					if fn.Trigger.Topic != "" {
						topics = append(topics, fn.Trigger.Topic)
					}
				}
				return topics
			},
		).
		Build()
}

// fakeClientWithMetricIndex builds a fake client pre-loaded with the given
// Application objects and the spec.metrics.name field index registered.
func fakeClientWithMetricIndex(apps ...*wasmplatformv1alpha1.Application) client.Client {
	objs := make([]client.Object, len(apps))
	for i, a := range apps {
		objs[i] = a
	}
	return fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objs...).
		WithIndex(
			&wasmplatformv1alpha1.Application{},
			metricNameIndexField,
			func(obj client.Object) []string {
				a := obj.(*wasmplatformv1alpha1.Application)
				var names []string
				for _, m := range a.Spec.Metrics {
					names = append(names, m.Name)
				}
				return names
			},
		).
		Build()
}

// makeTopicApp creates an Application with a single message-triggered function.
func makeTopicApp(ns, name, topic string, ts time.Time) *wasmplatformv1alpha1.Application {
	return &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(ts),
		},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: "oci://example.com/app@sha256:aaaa",
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						Topic: topic,
					},
				},
			},
		},
	}
}

// makeHTTPApp creates an Application with a single HTTP-triggered function.
func makeHTTPApp(ns, name, path string, ts time.Time) *wasmplatformv1alpha1.Application {
	return &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(ts),
		},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: "oci://example.com/app@sha256:aaaa",
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						HTTP: &wasmplatformv1alpha1.HttpTrigger{
							Path: path,
						},
					},
				},
			},
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
		topic     string
		wantOwner string // "ns/name", or "" if self is the rightful owner
	}{
		{
			name:      "sole owner — no conflict",
			self:      makeTopicApp("default", "my-app", "sole.topic", t0),
			others:    nil,
			topic:     "sole.topic",
			wantOwner: "",
		},
		{
			name:      "older app owns the topic",
			self:      makeTopicApp("default", "my-app", "shared.topic", t1),
			others:    []*wasmplatformv1alpha1.Application{makeTopicApp("default", "other-app", "shared.topic", t0)},
			topic:     "shared.topic",
			wantOwner: "default/other-app",
		},
		{
			name:      "self is older — self owns the topic",
			self:      makeTopicApp("default", "my-app", "shared.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeTopicApp("default", "other-app", "shared.topic", t1)},
			topic:     "shared.topic",
			wantOwner: "",
		},
		{
			name:      "same timestamp, lower name wins — self blocked",
			self:      makeTopicApp("default", "z-app", "tied.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeTopicApp("default", "a-app", "tied.topic", t0)},
			topic:     "tied.topic",
			wantOwner: "default/a-app",
		},
		{
			name:      "same timestamp, self has lower name — self owns",
			self:      makeTopicApp("default", "a-app", "tied.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeTopicApp("default", "z-app", "tied.topic", t0)},
			topic:     "tied.topic",
			wantOwner: "",
		},
		{
			name: "three-way race — oldest wins",
			self: makeTopicApp("ns2", "app2", "race.topic", t2),
			others: []*wasmplatformv1alpha1.Application{
				makeTopicApp("ns1", "app1", "race.topic", t0), // oldest — wins
				makeTopicApp("ns3", "app3", "race.topic", t1),
			},
			topic:     "race.topic",
			wantOwner: "ns1/app1",
		},
		{
			name: "three-way race — self is oldest",
			self: makeTopicApp("ns1", "app1", "race.topic", t0),
			others: []*wasmplatformv1alpha1.Application{
				makeTopicApp("ns2", "app2", "race.topic", t1),
				makeTopicApp("ns3", "app3", "race.topic", t2),
			},
			topic:     "race.topic",
			wantOwner: "",
		},
		{
			name:      "cross-namespace, lexicographic namespace tiebreak",
			self:      makeTopicApp("z-ns", "app", "ns.topic", t0),
			others:    []*wasmplatformv1alpha1.Application{makeTopicApp("a-ns", "app", "ns.topic", t0)},
			topic:     "ns.topic",
			wantOwner: "a-ns/app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			all := append([]*wasmplatformv1alpha1.Application{tt.self}, tt.others...)
			c := fakeClientWithIndex(all...)

			owner, err := findTopicOwner(context.Background(), c, tt.topic, tt.self)
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

func TestInternalFunctionTopic(t *testing.T) {
	tests := []struct {
		name string
		app  *wasmplatformv1alpha1.Application
		fn   wasmplatformv1alpha1.FunctionSpec
		want string
	}{
		{
			name: "message function receives fn. prefix",
			app:  makeTopicApp("default", "my-app", "my-app.events", t0),
			fn: wasmplatformv1alpha1.FunctionSpec{
				Name:   "handler",
				Module: "oci://example.com/app@sha256:aaaa",
				Trigger: wasmplatformv1alpha1.FunctionTrigger{
					Topic: "my-app.events",
				},
			},
			want: "fn.my-app.events",
		},
		{
			name: "http function generates http.<namespace>.<app>.<function>",
			app:  makeHTTPApp("default", "my-app", "/api/orders", t0),
			fn: wasmplatformv1alpha1.FunctionSpec{
				Name:   "handler",
				Module: "oci://example.com/app@sha256:aaaa",
				Trigger: wasmplatformv1alpha1.FunctionTrigger{
					HTTP: &wasmplatformv1alpha1.HttpTrigger{Path: "/api/orders"},
				},
			},
			want: "http.default.my-app.handler",
		},
		{
			name: "http function in different namespace",
			app:  makeHTTPApp("production", "order-service", "/orders", t0),
			fn: wasmplatformv1alpha1.FunctionSpec{
				Name:   "process",
				Module: "oci://example.com/app@sha256:aaaa",
				Trigger: wasmplatformv1alpha1.FunctionTrigger{
					HTTP: &wasmplatformv1alpha1.HttpTrigger{Path: "/orders"},
				},
			},
			want: "http.production.order-service.process",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := internalFunctionTopic(tt.app, &tt.fn)
			if got != tt.want {
				t.Errorf("internalFunctionTopic() = %q, want %q", got, tt.want)
			}
		})
	}
}

// makeMetricApp creates an Application with a single user-defined metric.
func makeMetricApp(ns, name, metricName string, ts time.Time) *wasmplatformv1alpha1.Application {
	return &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(ts),
		},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: "oci://example.com/app@sha256:aaaa",
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						Topic: name + ".events",
					},
				},
			},
			Metrics: []wasmplatformv1alpha1.MetricDefinition{
				{
					Name: metricName,
					Type: wasmplatformv1alpha1.MetricTypeCounter,
				},
			},
		},
	}
}

func TestFindMetricOwner(t *testing.T) {
	tests := []struct {
		name       string
		self       *wasmplatformv1alpha1.Application
		others     []*wasmplatformv1alpha1.Application
		metricName string
		wantOwner  string // "ns/name", or "" if self is the rightful owner
	}{
		{
			name:       "sole owner — no conflict",
			self:       makeMetricApp("default", "my-app", "requests_total", t0),
			others:     nil,
			metricName: "requests_total",
			wantOwner:  "",
		},
		{
			name:       "older app owns the metric",
			self:       makeMetricApp("default", "my-app", "shared_metric", t1),
			others:     []*wasmplatformv1alpha1.Application{makeMetricApp("default", "other-app", "shared_metric", t0)},
			metricName: "shared_metric",
			wantOwner:  "default/other-app",
		},
		{
			name:       "self is older — self owns the metric",
			self:       makeMetricApp("default", "my-app", "shared_metric", t0),
			others:     []*wasmplatformv1alpha1.Application{makeMetricApp("default", "other-app", "shared_metric", t1)},
			metricName: "shared_metric",
			wantOwner:  "",
		},
		{
			name:       "same timestamp, lower name wins — self blocked",
			self:       makeMetricApp("default", "z-app", "tied_metric", t0),
			others:     []*wasmplatformv1alpha1.Application{makeMetricApp("default", "a-app", "tied_metric", t0)},
			metricName: "tied_metric",
			wantOwner:  "default/a-app",
		},
		{
			name:       "same timestamp, self has lower name — self owns",
			self:       makeMetricApp("default", "a-app", "tied_metric", t0),
			others:     []*wasmplatformv1alpha1.Application{makeMetricApp("default", "z-app", "tied_metric", t0)},
			metricName: "tied_metric",
			wantOwner:  "",
		},
		{
			name: "three-way race — oldest wins",
			self: makeMetricApp("ns2", "app2", "race_metric", t2),
			others: []*wasmplatformv1alpha1.Application{
				makeMetricApp("ns1", "app1", "race_metric", t0), // oldest — wins
				makeMetricApp("ns3", "app3", "race_metric", t1),
			},
			metricName: "race_metric",
			wantOwner:  "ns1/app1",
		},
		{
			name: "three-way race — self is oldest",
			self: makeMetricApp("ns1", "app1", "race_metric", t0),
			others: []*wasmplatformv1alpha1.Application{
				makeMetricApp("ns2", "app2", "race_metric", t1),
				makeMetricApp("ns3", "app3", "race_metric", t2),
			},
			metricName: "race_metric",
			wantOwner:  "",
		},
		{
			name:       "cross-namespace, lexicographic namespace tiebreak",
			self:       makeMetricApp("z-ns", "app", "ns_metric", t0),
			others:     []*wasmplatformv1alpha1.Application{makeMetricApp("a-ns", "app", "ns_metric", t0)},
			metricName: "ns_metric",
			wantOwner:  "a-ns/app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			all := append([]*wasmplatformv1alpha1.Application{tt.self}, tt.others...)
			c := fakeClientWithMetricIndex(all...)

			owner, err := findMetricOwner(context.Background(), c, tt.metricName, tt.self)
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
