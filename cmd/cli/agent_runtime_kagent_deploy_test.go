/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const kagentTestNamespace = "orka-system"

// kagentTestCluster is a fake API server that assigns UIDs, rolls out
// Deployments at once, and reports registrations Ready, as Orka would.
func kagentTestCluster(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	epoch := &corev1alpha1.ControllerEpoch{ObjectMeta: metav1.ObjectMeta{Name: "orka-controller", Namespace: kagentTestNamespace},
		Spec: corev1alpha1.ControllerEpochSpec{Name: kagentControllerEpoch}, Status: corev1alpha1.ControllerEpochStatus{Epoch: 3}}
	assignUID := func(ctx context.Context, c client.WithWatch, obj client.Object) error {
		if obj.GetUID() != "" {
			return nil
		}
		stored := obj.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
			return err
		}
		stored.SetUID(types.UID("uid-" + obj.GetName()))
		if err := c.Update(ctx, stored); err != nil {
			return err
		}
		obj.SetUID(stored.GetUID())
		return nil
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, epoch)...).
		WithStatusSubresource(&corev1alpha1.ControllerEpoch{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				return assignUID(ctx, c, obj)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				return assignUID(ctx, c, obj)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				switch o := obj.(type) {
				case *appsv1.Deployment:
					o.Status = appsv1.DeploymentStatus{ObservedGeneration: o.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
				case *corev1alpha1.AgentRuntime:
					o.Status.Ready, o.Status.ObservedGeneration = true, o.Generation
				}
				return nil
			},
		}).Build()
}

const testKagentPinnedImage = "registry.example/acp-kagent-runtime@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"

// newTestKagentRuntimes deploys bundles into c. The runtime answers the
// capabilities of the profile it was rendered with.
func newTestKagentRuntimes(t *testing.T, c client.Client) (*kagentRuntimes, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	k := &kagentRuntimes{
		c: c, namespace: kagentTestNamespace, out: &out, holder: "test", poll: time.Millisecond,
		options: kagentRuntimeOptions{ControllerNamespace: kagentTestNamespace, ReadyTimeout: time.Second, RetireTimeout: time.Second,
			ModelUpstreamTokenFile: writeTempFile(t, "upstream-token\n")},
		resolve: func(context.Context, string) (string, string, error) {
			return testKagentPinnedImage, testAdapterDigest, nil
		},
	}
	k.capabilities = func(ctx context.Context, namespace, service string) ([]byte, error) {
		deployment := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: service}, deployment); err != nil {
			return nil, err
		}
		digest := ""
		for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST" {
				digest = env.Value
			}
		}
		return json.Marshal(map[string]any{"runtimeProfileDigest": digest, "limits": map[string]any{"maxResidentSessions": 10},
			"supportsDrain": true, "workspaceGovernance": map[string]any{"mode": "strict-governed"}})
	}
	return k, &out
}

// writeTestKagentBundle writes the governed v0 fixture's bundle.
func writeTestKagentBundle(t *testing.T) (string, kagentBundle) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := runKagentImport(t, kagentBundleArgs(writeTempFile(t, governedKagentV0Fixture), dir, "--dry-run")...); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(dir, "probe-gov")
	bundle, _ := readBundle(t, bundleDir)
	return bundleDir, bundle
}

func relayObjectsFor(bundle kagentBundle, serving string) (*corev1.Service, *networkingv1.NetworkPolicy) {
	meta := metav1.ObjectMeta{Name: "probe-gov-relay", Namespace: kagentTestNamespace,
		Labels: map[string]string{kagentAgentLabel: bundle.Agent}, Annotations: map[string]string{kagentSourceAnnotation: kagentSource(bundle)}}
	return &corev1.Service{ObjectMeta: meta, Spec: corev1.ServiceSpec{Selector: map[string]string{kagentRuntimeLabel: serving},
			Ports: []corev1.ServicePort{{Name: "relay", Port: 8092}}}},
		&networkingv1.NetworkPolicy{ObjectMeta: *meta.DeepCopy(), Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"old": "policy"}}}}
}

func TestKagentDeployRegistersAGovernedRuntime(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	c := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	k.unlock()
	secret := &corev1.Secret{}
	if err := c.Get(ctx, k.key(bundle.Runtime+"-auth"), secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["model-upstream-token"]) != "upstream-token" || len(secret.Data["controller-token"]) != 64 ||
		secret.Labels["orka.ai/agent-runtime-auth"] != "true" || secret.Annotations[kagentSourceAnnotation] != kagentSource(bundle) {
		t.Fatalf("auth secret labels %v annotations %v keys %d", secret.Labels, secret.Annotations, len(secret.Data))
	}
	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, k.key(bundle.Runtime+"-config"), configMap); err != nil || len(configMap.BinaryData[kagentBundleConfigFile]) == 0 {
		t.Fatalf("config map = %v, %v", configMap, err)
	}
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, k.key(bundle.Runtime), deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != testKagentPinnedImage ||
		deployment.Annotations[kagentRecoveryAnnotation] != "uid-"+bundle.Runtime {
		t.Fatalf("deployment image %s recovery %q", deployment.Spec.Template.Spec.Containers[0].Image, deployment.Annotations[kagentRecoveryAnnotation])
	}
	relay := &corev1.Service{}
	if err := c.Get(ctx, k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != bundle.Runtime {
		t.Fatalf("relay = %v, %v", relay.Spec.Selector, err)
	}
	if err := c.Get(ctx, k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("relay policy: %v", err)
	}
	registration := &corev1alpha1.AgentRuntime{}
	if err := c.Get(ctx, k.key(bundle.Runtime), registration); err != nil || registration.Spec.Deployment.KubernetesRecovery == nil {
		t.Fatalf("registration = %v, %v", registration.Spec.Deployment, err)
	}
	if uid, err := os.ReadFile(filepath.Join(dir, kagentBundleRegistrationFile)); err != nil || string(uid) != "uid-"+bundle.Runtime+"\n" {
		t.Fatalf("registration.uid = %q, %v", uid, err)
	}
	if err := c.Get(ctx, k.key(bundle.Agent+kagentLockSuffix), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the lock outlived the run: %v", err)
	}
	// Deploying again keeps the runtime's image and secret.
	k.resolve = func(context.Context, string) (string, string, error) {
		return "", "", errors.New("a deployed runtime resolved an image")
	}
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatalf("deploying again: %v", err)
	}
}

// Until the runtime is Ready, a failure moves the relay, and its policy,
// back to what they served before.
func TestKagentDeployMovesTheRelayBackWhenItFails(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, policy := relayObjectsFor(bundle, "probe-gov-kagent-old")
	c := kagentTestCluster(t, service, policy)
	k, out := newTestKagentRuntimes(t, c)
	k.capabilities = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("the runtime does not answer")
	}
	ctx := context.Background()
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("deploy = %v", err)
	}
	k.unlock()
	relay := &corev1.Service{}
	if err := c.Get(ctx, k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-old" {
		t.Fatalf("relay after a failed deploy = %v, %v; log %s", relay.Spec.Selector, err, out)
	}
	restored := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, k.key("probe-gov-relay"), restored); err != nil || restored.Spec.PodSelector.MatchLabels["old"] != "policy" {
		t.Fatalf("relay policy after a failed deploy = %v, %v", restored.Spec, err)
	}
}

// A policy the failed deploy created is deleted again.
func TestKagentDeployDeletesARelayPolicyItCreated(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, "probe-gov-kagent-old")
	c := kagentTestCluster(t, service)
	k, _ := newTestKagentRuntimes(t, c)
	k.capabilities = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("the runtime does not answer")
	}
	if err := k.deploy(context.Background(), dir); err == nil {
		t.Fatal("deploy succeeded without capabilities")
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the created policy stayed: %v", err)
	}
}

// A policy patch whose answer was lost may still have committed, so the
// rollback stays armed and restores the policy.
func TestKagentDeployRestoresAPolicyWhosePatchAnswerWasLost(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, policy := relayObjectsFor(bundle, "probe-gov-kagent-old")
	base := kagentTestCluster(t, service, policy)
	lost := false
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok && patch.Type() == types.JSONPatchType && !lost {
				lost = true
				return errors.New("the connection was reset")
			}
			return nil
		},
	})
	k, _ := newTestKagentRuntimes(t, c)
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "connection was reset") {
		t.Fatalf("deploy = %v", err)
	}
	restored := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), restored); err != nil || restored.Spec.PodSelector.MatchLabels["old"] != "policy" {
		t.Fatalf("relay policy after a lost answer = %v, %v", restored.Spec, err)
	}
	relay := &corev1.Service{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-old" {
		t.Fatalf("relay after a lost answer = %v, %v", relay.Spec.Selector, err)
	}
}

// A policy created by a request whose answer was lost is the deploy's own
// too: the rollback finds it by the deploy's nonce and deletes it.
func TestKagentDeployDeletesAPolicyWhoseCreateAnswerWasLost(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, "probe-gov-kagent-old")
	base := kagentTestCluster(t, service)
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
				return errors.New("the connection was reset")
			}
			return nil
		},
	})
	k, _ := newTestKagentRuntimes(t, c)
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "connection was reset") {
		t.Fatalf("deploy = %v", err)
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the policy whose create answer was lost stayed: %v", err)
	}
	relay := &corev1.Service{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-old" {
		t.Fatalf("relay after a lost answer = %v, %v", relay.Spec.Selector, err)
	}
}

func TestKagentDeployRefusesAnotherAgentsRelay(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, "other")
	service.Annotations[kagentSourceAnnotation] = "elsewhere/probe-gov"
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, service))
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "is not probe-gov's relay") {
		t.Fatalf("deploy onto another kagent Agent's relay = %v", err)
	}
}

func TestKagentDeployRefusesARotatedCredential(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bundle.Runtime + "-auth", Namespace: kagentTestNamespace},
		Data: map[string][]byte{"model-upstream-token": []byte("an older token")}}
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, secret))
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "new --runtime-revision") {
		t.Fatalf("deploy with another credential = %v", err)
	}
}

func TestKagentCommandsHoldAPerAgentLock(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	lock := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: bundle.Agent + kagentLockSuffix, Namespace: kagentTestNamespace}}
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, lock))
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "holds ConfigMap probe-gov-kagent-lock") {
		t.Fatalf("deploy while locked = %v", err)
	}
	if err := k.retire(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "holds ConfigMap") {
		t.Fatalf("retire while locked = %v", err)
	}
}

func agentUsing(bundle kagentBundle, name string) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: bundle.Agent, Namespace: kagentTestNamespace},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: name}}}}
}

func runtimeObjects(bundle kagentBundle, name, source string) []client.Object {
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: kagentTestNamespace,
			Labels:      map[string]string{kagentRuntimeLabel: name, kagentAgentLabel: bundle.Agent},
			Annotations: map[string]string{kagentSourceAnnotation: source}}
	}
	return []client.Object{
		&corev1alpha1.AgentRuntime{ObjectMeta: meta(name)},
		&appsv1.Deployment{ObjectMeta: meta(name)},
		&corev1.Service{ObjectMeta: meta(name)},
		&corev1.ConfigMap{ObjectMeta: meta(name + "-config")},
		&corev1.Secret{ObjectMeta: meta(name + "-auth")},
	}
}

// Retire deletes the Agent's other runtimes, and only those of the bundle's
// kagent Agent: a same-named kagent Agent of another namespace maps to the
// same Orka Agent.
func TestKagentRetireDeletesTheAgentsOtherRuntimes(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	objects := append([]client.Object{agentUsing(bundle, bundle.Runtime), service},
		runtimeObjects(bundle, "probe-gov-kagent-old", kagentSource(bundle))...)
	objects = append(objects, runtimeObjects(bundle, "probe-gov-kagent-other", "elsewhere/probe-gov")...)
	// An old runtime whose registration an earlier retire already deleted.
	leftover := runtimeObjects(bundle, "probe-gov-kagent-leftover", kagentSource(bundle))
	objects = append(objects, leftover[1:]...)
	c := kagentTestCluster(t, objects...)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.retire(ctx, dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"probe-gov-kagent-old", "probe-gov-kagent-leftover"} {
		if err := c.Get(ctx, k.key(name), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s was not retired: %v", name, err)
		}
		if err := c.Get(ctx, k.key(name+"-config"), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s's configuration was not deleted: %v", name, err)
		}
	}
	if err := c.Get(ctx, k.key("probe-gov-kagent-old"), &corev1alpha1.AgentRuntime{}); !apierrors.IsNotFound(err) {
		t.Errorf("the old registration stayed: %v", err)
	}
	if err := c.Get(ctx, k.key("probe-gov-kagent-other"), &appsv1.Deployment{}); err != nil {
		t.Errorf("another kagent Agent's runtime was retired: %v", err)
	}
}

func TestKagentRetireRefusesBeforeActivation(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, agentUsing(bundle, "probe-gov-kagent-old")))
	if err := k.retire(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "activate it first") {
		t.Fatalf("retire before activation = %v", err)
	}
	service, _ := relayObjectsFor(bundle, "probe-gov-kagent-old")
	k, _ = newTestKagentRuntimes(t, kagentTestCluster(t, agentUsing(bundle, bundle.Runtime), service))
	if err := k.retire(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "relay probe-gov-relay selects") {
		t.Fatalf("retire while the relay serves the old runtime = %v", err)
	}
}

// Activate previews the import, which must include the bundle's Agent, then
// runs it into the bundle's directory, and requires the Agent to use the
// bundle's runtime afterwards.
func TestKagentActivateSwitchesTheAgent(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	c := kagentTestCluster(t, agentUsing(bundle, "probe-gov-kagent-old"), service)
	k, _ := newTestKagentRuntimes(t, c)
	var calls [][]string
	var input []byte
	k.importer = func(ctx context.Context, args []string, quiet bool) error {
		calls = append(calls, args)
		input, _ = os.ReadFile(args[1])
		bundleDir := args[len(args)-1]
		if quiet {
			if err := os.MkdirAll(filepath.Join(bundleDir, bundle.Agent), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(bundleDir, bundle.Agent, kagentBundleRuntimeFile), []byte("{}"), 0o600)
		}
		return c.Update(ctx, withRuntime(t, c, bundle, bundle.Runtime))
	}
	err := k.activate(context.Background(), dir, []string{"-f", "-", "--allowed-tool", "probe"}, strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.Contains(strings.Join(calls[0], " "), "--dry-run --bundle-dir") ||
		strings.Contains(strings.Join(calls[1], " "), "--dry-run") || calls[1][len(calls[1])-1] != filepath.Dir(dir) {
		t.Fatalf("import calls = %v", calls)
	}
	if string(input) != "input" {
		t.Fatalf("standard input reached the import as %q", input)
	}
	if err := k.activate(context.Background(), dir, []string{"-f", "x", "--dry-run"}, nil); err == nil || !strings.Contains(err.Error(), "without --dry-run") {
		t.Fatalf("activate --dry-run = %v", err)
	}
}

func withRuntime(t *testing.T, c client.Client, bundle kagentBundle, name string) *corev1alpha1.Agent {
	t.Helper()
	agent := &corev1alpha1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: kagentTestNamespace, Name: bundle.Agent}, agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Runtime.RuntimeRef.Name = name
	return agent
}

func TestKagentActivateRefusesAnImportWithoutTheAgent(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, agentUsing(bundle, "old"), service))
	k.importer = func(context.Context, []string, bool) error { return nil }
	if err := k.activate(context.Background(), dir, []string{"-f", "other.yaml"}, nil); err == nil || !strings.Contains(err.Error(), "do not import Agent probe-gov") {
		t.Fatalf("activate with an import that skips the Agent = %v", err)
	}
}

func TestKagentDefaultImageFollowsTheRelease(t *testing.T) {
	if image, err := kagentDefaultImage("v0.4.0"); err != nil || image != "ghcr.io/orka-agents/orka/acp-kagent-runtime:0.4.0" {
		t.Fatalf("release image = %q, %v", image, err)
	}
	for _, build := range []string{"dev", "a49ac96ce-dirty", "v0.4.0-12-gabc"} {
		if _, err := kagentDefaultImage(build); err == nil || !strings.Contains(err.Error(), "pass --image") {
			t.Errorf("%s: err = %v", build, err)
		}
	}
}
