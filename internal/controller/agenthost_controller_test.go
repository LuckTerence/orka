/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agenthost"
)

const (
	testHostNamespace = "orka-agent-hosts"
	testUserNamespace = "team"
	testAgentImage    = "registry.example/orka/controller@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	testHostImage     = "registry.example/host@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
)

type fakeIdleStatus struct {
	status agenthost.IdleStatus
	err    error
}

func (f *fakeIdleStatus) ReadIdleStatus(context.Context, string) (agenthost.IdleStatus, error) {
	return f.status, f.err
}

func agentHostScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testTemplate(access corev1alpha1.AgentHostAccess, policy corev1alpha1.AgentHostCredentialPolicy) *corev1alpha1.AgentHostTemplate {
	return &corev1alpha1.AgentHostTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "copilotd", Namespace: testUserNamespace, UID: "template-uid"},
		Spec: corev1alpha1.AgentHostTemplateSpec{
			SecurityContext: corev1alpha1.AgentHostSecurityContext{RunAsUser: 1000, RunAsGroup: 1000},
			Containers: []corev1alpha1.AgentHostContainer{{
				Name:         corev1alpha1.AgentHostContainerName,
				Image:        testHostImage,
				VolumeMounts: []corev1alpha1.AgentHostVolumeMount{{Name: "workspace", MountPath: "/workspace"}},
			}},
			Storage:        corev1alpha1.AgentHostStorage{Size: resource.MustParse("20Gi")},
			ListenPort:     7777,
			Access:         access,
			GitHubToken:    policy,
			IdleStatusFile: "idle.json",
		},
	}
}

func testHost() *corev1alpha1.AgentHost {
	return &corev1alpha1.AgentHost{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: testUserNamespace, UID: "host-uid", Generation: 1},
		Spec: corev1alpha1.AgentHostSpec{
			TemplateRef:          corev1alpha1.AgentHostTemplateReference{Name: "copilotd"},
			GitHubTokenSecretRef: &corev1alpha1.AgentHostSecretKeyReference{Name: "alice-github", Key: "token"},
		},
	}
}

func userToken(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-github", Namespace: testUserNamespace},
		Data:       map[string][]byte{"token": []byte(value)},
	}
}

type agentHostHarness struct {
	t          *testing.T
	client     client.Client
	reconciler *AgentHostReconciler
	idle       *fakeIdleStatus
	now        time.Time
}

func newAgentHostHarness(t *testing.T, objects ...client.Object) *agentHostHarness {
	t.Helper()
	h := &agentHostHarness{t: t, idle: &fakeIdleStatus{}, now: time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)}
	h.client = fake.NewClientBuilder().
		WithScheme(agentHostScheme(t)).
		WithStatusSubresource(&corev1alpha1.AgentHost{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(uuid.NewUUID())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	h.reconciler = &AgentHostReconciler{
		Client:        h.client,
		Scheme:        h.client.Scheme(),
		HostNamespace: testHostNamespace,
		AgentImage:    testAgentImage,
		Controller: agenthost.ControllerPeer{
			Namespace: "orka-system",
			PodLabels: map[string]string{"control-plane": "controller-manager"},
		},
		IdleStatus: h.idle,
		Now:        func() time.Time { return h.now },
	}
	return h
}

// settle reconciles until the result stops asking for an immediate retry.
func (h *agentHostHarness) settle() {
	h.t.Helper()
	for range 20 {
		result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: testUserNamespace, Name: "alice"},
		})
		if err != nil {
			h.t.Fatalf("reconcile: %v", err)
		}
		if result.RequeueAfter > 5*time.Second || result.RequeueAfter == 0 {
			return
		}
	}
}

func (h *agentHostHarness) host() *corev1alpha1.AgentHost {
	h.t.Helper()
	host := &corev1alpha1.AgentHost{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "alice"}, host); err != nil {
		h.t.Fatal(err)
	}
	return host
}

func (h *agentHostHarness) names() agenthost.Names {
	return agenthost.ObjectNames(h.host(), testHostNamespace)
}

func (h *agentHostHarness) pod() (*corev1.Pod, bool) {
	h.t.Helper()
	pod := &corev1.Pod{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().Pod}, pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return pod, true
}

func (h *agentHostHarness) markPodReady() {
	h.t.Helper()
	pod, ok := h.pod()
	if !ok {
		h.t.Fatal("no Pod to mark ready")
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.7"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

func agentHostReady(host *corev1alpha1.AgentHost) *metav1.Condition {
	return meta.FindStatusCondition(host.Status.Conditions, agentHostConditionReady)
}

func TestAgentHostRelayCreatesHardenedObjects(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()

	host := h.host()
	if host.Status.HostNamespace != testHostNamespace || host.Status.Phase != corev1alpha1.AgentHostPhaseProvisioning {
		t.Fatalf("status = %+v", host.Status)
	}
	if len(host.Status.Objects) != 5 {
		t.Fatalf("receipts = %+v, want token Secrets, PVC, NetworkPolicy and Pod", host.Status.Objects)
	}
	for _, receipt := range host.Status.Objects {
		if receipt.UID == "" || receipt.Nonce == "" {
			t.Fatalf("receipt without UID or nonce: %+v", receipt)
		}
	}

	pod, ok := h.pod()
	if !ok {
		t.Fatal("no host Pod")
	}
	assertHardenedHostPod(t, pod)

	names := h.names()
	copied := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.GitHubTokenSecret}, copied); err != nil {
		t.Fatal(err)
	}
	if string(copied.Data["token"]) != "ghs_user_token" {
		t.Fatal("GitHub token was not copied")
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.NetworkPolicy}, policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress) != 1 || policy.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "orka-system" {
		t.Fatalf("ingress = %+v", policy.Spec.Ingress)
	}
	connection := &corev1.Secret{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: names.Connection}, connection)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("relay hosts get no connection Secret, err = %v", err)
	}

	h.markPodReady()
	h.settle()
	host = h.host()
	if host.Status.Phase != corev1alpha1.AgentHostPhaseReady || agentHostReady(host).Status != metav1.ConditionTrue {
		t.Fatalf("status after ready = %+v", host.Status)
	}
}

func assertHardenedHostPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("pod policy = %s automount=%v", pod.Spec.RestartPolicy, *pod.Spec.AutomountServiceAccountToken)
	}
	sidecar := pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1]
	if sidecar.Name != "orka-host-agent" || sidecar.Image != testAgentImage ||
		sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("host agent sidecar = %+v", sidecar)
	}
	// Orka's own image stages the connection token, so host images need no tools.
	stage := pod.Spec.InitContainers[0]
	if stage.Image != testAgentImage || len(stage.Args) == 0 || stage.Args[0] != "stage-token" {
		t.Fatalf("token staging container = %+v", stage)
	}
	hostContainer := pod.Spec.Containers[0]
	if hostContainer.LivenessProbe == nil || hostContainer.LivenessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("host liveness = %+v", hostContainer.LivenessProbe)
	}
	if hostContainer.StartupProbe == nil || hostContainer.StartupProbe.HTTPGet.Path != "/startupz" ||
		hostContainer.ReadinessProbe == nil || hostContainer.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("host probes = %+v / %+v", hostContainer.StartupProbe, hostContainer.ReadinessProbe)
	}
	env := map[string]string{}
	for _, value := range hostContainer.Env {
		env[value.Name] = value.Value
	}
	if env[agenthost.HostIDEnv] != "host-uid" || env[agenthost.GitHubTokenFileEnv] == "" ||
		env[agenthost.ConnectionTokenFileEnv] == "" || env[agenthost.StatusDirEnv] == "" {
		t.Fatalf("host env = %v", env)
	}
	for _, mount := range sidecar.VolumeMounts {
		if mount.Name == "orka-github-token" {
			t.Fatal("the GitHub token must be mounted into the host container only")
		}
	}
	for _, mount := range hostContainer.VolumeMounts {
		if mount.Name == "orka-connection-token-secret" {
			t.Fatal("the host container must not mount the connection token Secret")
		}
	}
}

func TestAgentHostRequiresItsGitHubToken(t *testing.T) {
	host := testHost()
	host.Spec.GitHubTokenSecretRef = nil
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired), host)
	h.settle()
	got := h.host()
	if got.Status.Phase != corev1alpha1.AgentHostPhaseFailed || agentHostReady(got).Reason != agentHostReasonGitHubTokenRequired {
		t.Fatalf("status = %+v", got.Status)
	}
	if _, ok := h.pod(); ok {
		t.Fatal("a host without its token must not start")
	}
}

func TestAgentHostMissingTemplateFails(t *testing.T) {
	h := newAgentHostHarness(t, testHost())
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonTemplateNotFound {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestAgentHostNeverAdoptsForeignObjects(t *testing.T) {
	host := testHost()
	names := agenthost.ObjectNames(host, testHostNamespace)
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: names.PVC, Namespace: testHostNamespace, UID: "foreign",
			Labels: map[string]string{agenthost.LabelHostUID: string(host.UID)}},
	}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone),
		host, foreign)
	h.settle()
	got := h.host()
	if agentHostReady(got).Reason != agentHostReasonObjectCollision {
		t.Fatalf("status = %+v", got.Status)
	}
	if _, ok := h.pod(); ok {
		t.Fatal("no Pod may start next to a foreign volume")
	}

	// Deletion leaves the foreign object alone.
	if err := h.client.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.settle()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.PVC}, pvc); err != nil || pvc.UID != "foreign" {
		t.Fatalf("foreign PVC was touched: %v %v", err, pvc.UID)
	}
}

func TestAgentHostAdoptsObjectFromAnInterruptedCreate(t *testing.T) {
	host := testHost()
	host.Spec.GitHubTokenSecretRef = nil
	host.Status.HostNamespace = testHostNamespace
	names := agenthost.ObjectNames(host, testHostNamespace)
	// The receipt was recorded but the controller stopped before saving the UID.
	host.Status.Objects = []corev1alpha1.AgentHostObjectReceipt{{Kind: agentHostKindSecret, Name: names.ConnectionTokenSecret, Nonce: "n1"}}
	host.Finalizers = []string{agentHostFinalizer}
	created := agenthost.ConnectionTokenSecret(host, names, "n1", []byte("token-value"))
	created.UID = "created-uid"
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone), host, created)
	h.settle()
	receipt := findAgentHostReceipt(h.host(), agentHostKindSecret, "", names.ConnectionTokenSecret)
	if receipt == nil || receipt.UID != "created-uid" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestAgentHostStandaloneConnectionSecret(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone), testHost())
	h.settle()
	host := h.host()
	if host.Status.ConnectionSecretRef == nil {
		t.Fatal("standalone hosts report a connection Secret")
	}
	connection := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: host.Status.ConnectionSecretRef.Name}, connection); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(connection, host) || string(connection.Data["port"]) != "7777" ||
		string(connection.Data["namespace"]) != testHostNamespace || len(connection.Data["token"]) != 2*agentHostTokenBytes {
		t.Fatalf("connection Secret = %+v", connection)
	}
	if pod, ok := h.pod(); !ok || pod.Annotations[agenthost.AnnotationGitHubToken] != "false" {
		t.Fatal("standalone host without a token template must not mount a GitHub token")
	}
}

func TestAgentHostReplacesATerminatedPod(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	first, _ := h.pod()
	first.Status.Phase = corev1.PodFailed
	if err := h.client.Status().Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a failed Pod is replaced only after the restart backoff")
	}
	host := h.host()
	if host.Status.Restarts != 1 || agentHostReady(host).Reason != agentHostReasonRestarting {
		t.Fatalf("status = %+v", host.Status)
	}

	h.now = h.now.Add(11 * time.Second)
	h.settle()
	second, ok := h.pod()
	if !ok || second.UID == first.UID {
		t.Fatalf("terminated Pod was not replaced (ok=%v)", ok)
	}
	if second.Annotations[agenthost.AnnotationNonce] == first.Annotations[agenthost.AnnotationNonce] {
		t.Fatal("a replacement Pod gets a fresh creation nonce")
	}

	// A second failure doubles the wait, and readiness resets the count.
	second.Status.Phase = corev1.PodFailed
	if err := h.client.Status().Update(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	h.settle()
	h.now = h.now.Add(11 * time.Second)
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("the second replacement must wait longer than the first")
	}
	h.now = h.now.Add(10 * time.Second)
	h.settle()
	h.markPodReady()
	h.settle()
	if got := h.host().Status.Restarts; got != 0 {
		t.Fatalf("restarts after the host became ready = %d", got)
	}
}

func TestAgentHostHostContainerExitRetiresThePod(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	pod, _ := h.pod()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: corev1alpha1.AgentHostContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a Pod whose host exited must be retired even while its sidecar runs")
	}
}

func TestAgentHostSuspendsWithoutItsTemplateOrToken(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	for _, object := range []client.Object{
		&corev1alpha1.AgentHostTemplate{ObjectMeta: metav1.ObjectMeta{Name: "copilotd", Namespace: testUserNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alice-github", Namespace: testUserNamespace}},
	} {
		if err := h.client.Delete(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	host := h.host()
	host.Spec.Suspended = true
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("suspension must stop the Pod even without the template or token")
	}
	if got := h.host().Status.Phase; got != corev1alpha1.AgentHostPhaseSuspended {
		t.Fatalf("phase = %s", got)
	}
}

func TestAgentHostStopsWhenItsTokenIsRevoked(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	names := h.names()
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alice-github", Namespace: testUserNamespace}}
	if err := h.client.Delete(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a host whose token source is gone must stop")
	}
	copied := &corev1.Secret{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.GitHubTokenSecret}, copied)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the copied token must be deleted: %v", err)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.PVC}, pvc); err != nil {
		t.Fatalf("the state volume must survive a failure: %v", err)
	}
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonGitHubTokenUnavailable {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestAgentHostRefusesAForgedConnectionSecret(t *testing.T) {
	host := testHost()
	names := agenthost.ObjectNames(host, testHostNamespace)
	forged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: names.Connection, Namespace: testUserNamespace, UID: "forged",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "core.orka.ai/v1alpha1", Kind: "AgentHost", Name: host.Name, UID: host.UID, Controller: new(true),
		}},
	}}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone),
		host, forged)
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonObjectCollision {
		t.Fatalf("status = %+v", got.Status)
	}
	secret := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: names.Connection}, secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 0 {
		t.Fatal("the connection token was written into a Secret this host did not create")
	}
}

func TestAgentHostNeverRecoversUserNamespaceObjectsByNonce(t *testing.T) {
	host := testHost()
	host.Spec.GitHubTokenSecretRef = nil
	host.Finalizers = []string{agentHostFinalizer}
	host.Status.HostNamespace = testHostNamespace
	names := agenthost.ObjectNames(host, testHostNamespace)
	// The receipt is public in status; a user copies its nonce onto a Secret
	// before the controller records the real Secret's UID.
	host.Status.Objects = []corev1alpha1.AgentHostObjectReceipt{{
		Kind: agentHostKindConnectionSecret, Namespace: testUserNamespace, Name: names.Connection, Nonce: "public-nonce",
	}}
	planted := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: names.Connection, Namespace: testUserNamespace, UID: "planted",
		Labels:      map[string]string{agenthost.LabelHostUID: string(host.UID)},
		Annotations: map[string]string{agenthost.AnnotationNonce: "public-nonce"},
	}}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone),
		host, planted)
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonObjectCollision {
		t.Fatalf("status = %+v", got.Status)
	}
	secret := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: names.Connection}, secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 0 {
		t.Fatal("the connection token was written into a planted Secret")
	}
}

func TestAgentHostNamespaceChangeFailsWithoutTouchingObjects(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	pod, _ := h.pod()
	h.reconciler.HostNamespace = "orka-agent-hosts-2"
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonHostNamespaceChanged {
		t.Fatalf("status = %+v", got.Status)
	}
	if after, ok := h.pod(); !ok || after.UID != pod.UID {
		t.Fatal("a host in the old namespace must be left alone until it is deleted")
	}
}

func TestAgentHostSuspendKeepsStateAndResumes(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	host := h.host()
	host.Spec.Suspended = true
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("suspended host still has a Pod")
	}
	host = h.host()
	if host.Status.Phase != corev1alpha1.AgentHostPhaseSuspended ||
		meta.FindStatusCondition(host.Status.Conditions, agentHostConditionSuspended).Reason != agentHostReasonRequested {
		t.Fatalf("status = %+v", host.Status)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().PVC}, pvc); err != nil {
		t.Fatalf("state volume must survive suspension: %v", err)
	}

	host.Spec.Suspended = false
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("resumed host has no Pod")
	}
}

func TestAgentHostIdleSuspendAndWake(t *testing.T) {
	template := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	template.Spec.IdleSuspend = &corev1alpha1.AgentHostIdleSuspend{After: metav1.Duration{Duration: 30 * time.Minute}}
	h := newAgentHostHarness(t, template, testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()

	since := h.now.Add(-10 * time.Minute)
	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("a host idle for less than the window must keep running")
	}
	if got := h.host().Status.IdleSince; got == nil || !got.Time.Equal(since) {
		t.Fatalf("idleSince = %v", got)
	}

	// A stale report never suspends.
	since = h.now.Add(-2 * time.Hour)
	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since, Stale: true}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("a stale idle report must not suspend the host")
	}

	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a host idle past the window must be suspended")
	}
	host := h.host()
	if host.Status.IdleSuspendedAt == nil || host.Status.Phase != corev1alpha1.AgentHostPhaseSuspended {
		t.Fatalf("status = %+v", host.Status)
	}

	// An older wake request does nothing; a newer one starts the host.
	before := metav1.NewTime(host.Status.IdleSuspendedAt.Add(-time.Minute))
	host.Spec.WakeRequestedAt = &before
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a wake request older than the suspension must not start the host")
	}
	host = h.host()
	after := metav1.NewTime(host.Status.IdleSuspendedAt.Add(time.Minute))
	host.Spec.WakeRequestedAt = &after
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("a wake request must start the host")
	}
	if got := h.host().Status.IdleSuspendedAt; got != nil {
		t.Fatalf("idleSuspendedAt = %v after wake", got)
	}
}

func TestAgentHostIdleStatusErrorsNeverSuspend(t *testing.T) {
	template := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	template.Spec.IdleSuspend = &corev1alpha1.AgentHostIdleSuspend{After: metav1.Duration{Duration: time.Minute}}
	h := newAgentHostHarness(t, template, testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()
	h.idle.err = errors.New("unreachable")
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("an unreadable idle status must not suspend the host")
	}
}

func TestAgentHostRotatesTheGitHubTokenCopy(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_first"))
	h.settle()
	pod, _ := h.pod()
	source := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "alice-github"}, source); err != nil {
		t.Fatal(err)
	}
	source.Data["token"] = []byte("ghs_second")
	if err := h.client.Update(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	h.settle()
	copied := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().GitHubTokenSecret}, copied); err != nil {
		t.Fatal(err)
	}
	if string(copied.Data["token"]) != "ghs_second" {
		t.Fatal("the host's copy did not follow the rotated token")
	}
	if after, _ := h.pod(); after.UID != pod.UID {
		t.Fatal("rotating the token must not restart the host")
	}
}

func TestAgentHostLostStateVolumeFailsInsteadOfRecreating(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone), testHost())
	h.settle()
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: testHostNamespace, Name: h.names().PVC}
	if err := h.client.Get(context.Background(), key, pvc); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonStateVolumeLost {
		t.Fatalf("status = %+v", got.Status)
	}
	if err := h.client.Get(context.Background(), key, pvc); !apierrors.IsNotFound(err) {
		t.Fatalf("a lost state volume must not be silently recreated: %v", err)
	}
}

func TestAgentHostDeletionRemovesEverythingItCreated(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	host := h.host()
	receipts := append([]corev1alpha1.AgentHostObjectReceipt(nil), host.Status.Objects...)
	if err := h.client.Delete(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	for _, receipt := range receipts {
		object := newAgentHostObject(receipt.Kind)
		err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: receipt.Name}, object)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("%s %s survived deletion: %v", receipt.Kind, receipt.Name, err)
		}
	}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "alice"}, &corev1alpha1.AgentHost{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("AgentHost kept its finalizer: %v", err)
	}
}

func TestAgentHostObjectNamesAreStableAndBounded(t *testing.T) {
	host := testHost()
	host.Name = strings.Repeat("a", 250)
	names := agenthost.ObjectNames(host, testHostNamespace)
	if len(names.Pod) > 63 || len(names.ConnectionTokenSecret) > 253 || len(names.Connection) > 253 {
		t.Fatalf("names too long: %+v", names)
	}
	if again := agenthost.ObjectNames(host, testHostNamespace); again != names {
		t.Fatal("names must be stable")
	}
	other := testHost()
	other.Name = host.Name
	other.UID = "other-uid"
	if agenthost.ObjectNames(other, testHostNamespace).Pod == names.Pod {
		t.Fatal("hosts with the same name in different namespaces must not share objects")
	}
}
