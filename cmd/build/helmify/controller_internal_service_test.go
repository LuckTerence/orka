package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// In-cluster callers use the internal Service, which must keep reaching a
// controller whose readiness is false during the preStop drain, while the
// external Service stays readiness-gated.
func TestControllerInternalServiceKeepsDrainingControllerReachable(t *testing.T) {
	rendered := requireHelmRender(t,
		"--set", "service.type=LoadBalancer",
		"--show-only", "templates/controller-internal-service.yaml",
	)
	internal := corev1.Service{}
	if err := yaml.Unmarshal([]byte(rendered), &internal); err != nil {
		t.Fatalf("decode controller internal Service: %v", err)
	}
	if internal.Name != "test-orka-internal" || internal.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("controller internal Service = %s type %q, want test-orka-internal ClusterIP", internal.Name, internal.Spec.Type)
	}
	if !internal.Spec.PublishNotReadyAddresses {
		t.Fatal("controller internal Service must publish unready addresses so a draining controller keeps serving in-cluster callers")
	}
	if internal.Spec.Selector["app.kubernetes.io/component"] != "controller" {
		t.Fatalf("controller internal Service selector = %#v, want the controller", internal.Spec.Selector)
	}

	rendered = requireHelmRender(t,
		"--set", "service.type=LoadBalancer",
		"--show-only", "templates/service.yaml",
	)
	external := corev1.Service{}
	if err := yaml.Unmarshal([]byte(rendered), &external); err != nil {
		t.Fatalf("decode controller Service: %v", err)
	}
	if external.Spec.PublishNotReadyAddresses {
		t.Fatal("external controller Service must stay readiness-gated during the drain")
	}
}
