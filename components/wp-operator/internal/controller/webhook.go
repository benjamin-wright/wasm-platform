package controller

import (
	"context"
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
)

// ApplicationValidator is a validating admission webhook handler for Application resources.
// It enforces TopicConflict, MetricConflict, and InvalidIdentifier constraints at admission
// time so that violations are rejected immediately on kubectl apply rather than surfacing
// only as status conditions post-reconciliation.
//
// +kubebuilder:webhook:path=/validate-wasm-platform-io-v1alpha1-application,mutating=false,failurePolicy=fail,sideEffects=None,groups=wasm-platform.io,resources=applications,verbs=create;update,versions=v1alpha1,name=vapplication.wasm-platform.io,admissionReviewVersions=v1
type ApplicationValidator struct {
	Client  client.Client
	Decoder admission.Decoder
}

// Handle validates an incoming Application create or update request.
func (v *ApplicationValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	app := &wasmplatformv1alpha1.Application{}
	if err := v.Decoder.Decode(req, app); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if app.Spec.SQL != nil {
		if err := ValidatePGInputs(app.Namespace, app.Name); err != nil {
			return admission.Denied(fmt.Sprintf("invalid identifier: %s", err))
		}
	}

	for i := range app.Spec.Functions {
		fn := &app.Spec.Functions[i]
		if fn.Trigger.Topic == "" {
			continue
		}
		owner, err := findTopicOwner(ctx, v.Client, fn.Trigger.Topic, app)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("checking topic ownership for function %q: %w", fn.Name, err))
		}
		if owner != nil {
			return admission.Denied(fmt.Sprintf("function %q: topic %q is already claimed by %s/%s", fn.Name, fn.Trigger.Topic, owner.Namespace, owner.Name))
		}
	}

	for i := range app.Spec.Metrics {
		m := &app.Spec.Metrics[i]
		owner, err := findMetricOwner(ctx, v.Client, m.Name, app)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("checking metric ownership for %q: %w", m.Name, err))
		}
		if owner != nil {
			return admission.Denied(fmt.Sprintf("metric %q is already claimed by %s/%s", m.Name, owner.Namespace, owner.Name))
		}
	}

	return admission.Allowed("")
}
