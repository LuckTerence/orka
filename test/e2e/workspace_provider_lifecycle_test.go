//go:build e2e

package e2e

// Workspace provider lifecycle runs the same Codex Tasks, scripted by aimock,
// against a real out-of-tree provider: agent-sandbox or Agent Substrate.
// E2E_WORKSPACE_PROVIDER selects it; every other lane skips this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"

	"github.com/orka-agents/orka/test/utils"
)

const (
	workspaceProviderEnv = "E2E_WORKSPACE_PROVIDER"
	// E2E_ORKA_WORKSPACE_DIR builds providers from a local checkout, as the
	// orka-workspace CI does, instead of the revision Core pins.
	workspaceSourceEnv = "E2E_ORKA_WORKSPACE_DIR"

	wspData        = "ORKA_WSP_DURABLE_DATA"
	wspProviderNS  = "orka-workspace-system"
	wspSandboxNS   = defaultRuntimeNamespace
	wspSandboxUID  = "sandbox.workspace.orka.ai/workspace-uid"
	wspAtespace    = "orka-system"
	wspNativePool  = "ate-demo"
	wspNativeOwner = "substrate.workspace.orka.ai/workspace-uid"
)

// workspaceBackend holds what differs between providers. Everything else is
// asserted through Orka's provider-neutral Task and workspace objects.
type workspaceBackend struct {
	name, deployment, controllerName string
	profileGroup, profileKind        string
	profileSpec                      func(suspend bool) map[string]any
	prepare                          func()
	patchProvider                    func(deployment map[string]any)
	loseInstance                     func(workspaceUID string)
	leftovers                        func(g Gomega, workspaceUID string) []string
	// lostWorkspaceFails reports whether losing the instance fails the
	// workspace (native process) rather than replacing its Pod.
	lostWorkspaceFails bool
}

func sandboxWorkspaceBackend() workspaceBackend {
	return workspaceBackend{
		name: "sandbox", deployment: "orka-workspace-sandbox", controllerName: "sandbox.workspace.orka.ai",
		profileGroup: "sandbox.workspace.orka.ai", profileKind: "SandboxWorkspaceProfile",
		profileSpec: func(suspend bool) map[string]any {
			if !suspend {
				return map[string]any{}
			}
			return map[string]any{"suspend": map[string]any{"mode": "DataOnly", "volume": map[string]any{"capacity": "1Gi"}}}
		},
		prepare: func() {
			version, err := utils.Run(exec.Command("go", "list", "-m", "-f", "{{.Version}}", "sigs.k8s.io/agent-sandbox"))
			Expect(err).NotTo(HaveOccurred())
			base := "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/" + strings.TrimSpace(version)
			for _, args := range [][]string{
				{"apply", "-f", base + "/sandbox.yaml"},
				{"apply", "-f", base + "/extensions.yaml"},
				{"wait", "--for=condition=Established", "--timeout=90s", "crd/sandboxes.agents.x-k8s.io",
					"crd/sandboxclaims.extensions.agents.x-k8s.io", "crd/sandboxtemplates.extensions.agents.x-k8s.io",
					"crd/sandboxwarmpools.extensions.agents.x-k8s.io"},
				{"-n", "agent-sandbox-system", "rollout", "status", "deployment/agent-sandbox-controller", "--timeout=5m"},
			} {
				_, err := utils.Run(exec.Command("kubectl", args...))
				Expect(err).NotTo(HaveOccurred())
			}
		},
		loseInstance: func(workspaceUID string) {
			_, err := utils.Run(exec.Command("kubectl", "-n", wspSandboxNS, "delete", "sandboxes.agents.x-k8s.io",
				"-l", wspSandboxUID+"="+workspaceUID, "--wait=false"))
			Expect(err).NotTo(HaveOccurred())
		},
		leftovers: func(g Gomega, workspaceUID string) []string {
			output, err := utils.Run(exec.Command("kubectl", "-n", wspSandboxNS, "get",
				"sandboxclaims,sandboxwarmpools,sandboxtemplates,sandboxes.agents.x-k8s.io,configmaps",
				"-l", wspSandboxUID+"="+workspaceUID, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			return utils.GetNonEmptyLines(output)
		},
	}
}

// The Substrate cluster, WorkerPool, ActorTemplate, and Actor DNS come from
// scripts/workspace-substrate-cluster.sh, which also builds kubectl-ate.
func substrateWorkspaceBackend() workspaceBackend {
	kubectlATE := func(args ...string) (string, error) {
		path := os.Getenv("E2E_SUBSTRATE_KUBECTL_ATE")
		Expect(path).NotTo(BeEmpty(), "E2E_SUBSTRATE_KUBECTL_ATE must name the kubectl-ate binary")
		return utils.Run(exec.Command(path, args...))
	}
	nativeNames := func(g Gomega, kind string) []string {
		output, err := kubectlATE("get", kind, "--atespace", wspAtespace, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred())
		var listed map[string][]struct {
			Metadata struct{ Name string } `json:"metadata"`
		}
		g.Expect(json.Unmarshal([]byte(output), &listed)).To(Succeed())
		var names []string
		for _, item := range listed[kind] {
			names = append(names, kind+"/"+item.Metadata.Name)
		}
		return names
	}
	return workspaceBackend{
		name: "substrate", deployment: "orka-workspace-substrate", controllerName: "v1.substrate.workspace.orka.ai",
		profileGroup: "substrate.workspace.orka.ai", profileKind: "SubstrateWorkspaceProfile",
		profileSpec: func(suspend bool) map[string]any {
			spec := map[string]any{"templateRef": map[string]any{"namespace": wspAtespace, "name": "orka-acp-infra"}}
			if suspend {
				spec["suspend"] = map[string]any{"mode": "DataOnly"}
			}
			return spec
		},
		prepare: func() {},
		// The provider authenticates to Substrate with a PodCertificate and
		// dials Actors directly.
		patchProvider: func(deployment map[string]any) {
			pod := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			security, _ := pod["securityContext"].(map[string]any)
			if security == nil {
				security = map[string]any{}
			}
			security["runAsUser"], security["runAsGroup"], security["fsGroup"] = 65532, 65532, 65532
			pod["securityContext"] = security
			pod["volumes"] = []any{
				map[string]any{"name": "native-client", "projected": map[string]any{"sources": []any{map[string]any{"podCertificate": map[string]any{
					"signerName": "podidentity.podcert.ate.dev/identity", "keyType": "ECDSAP256", "credentialBundlePath": "credential-bundle.pem"}}}}},
				map[string]any{"name": "native-server", "projected": map[string]any{"sources": []any{map[string]any{"clusterTrustBundle": map[string]any{
					"signerName": "servicedns.podcert.ate.dev/identity", "path": "trust-bundle.pem",
					"labelSelector": map[string]any{"matchLabels": map[string]any{"podcert.ate.dev/canarying": "live"}}}}}}},
			}
			container := pod["containers"].([]any)[0].(map[string]any)
			container["volumeMounts"] = []any{
				map[string]any{"name": "native-client", "mountPath": "/native/client", "readOnly": true},
				map[string]any{"name": "native-server", "mountPath": "/native/server", "readOnly": true},
			}
			var args []any
			for _, arg := range container["args"].([]any) {
				value := arg.(string)
				if !strings.HasPrefix(value, "--native-ca-file=") && !strings.HasPrefix(value, "--native-token-file=") &&
					!strings.HasPrefix(value, "--native-direct-egress=") {
					args = append(args, value)
				}
			}
			container["args"] = append(args, "--native-ca-file=/native/server/trust-bundle.pem",
				"--native-cert-file=/native/client/credential-bundle.pem",
				"--native-key-file=/native/client/credential-bundle.pem", "--native-direct-egress=true")
		},
		loseInstance: func(string) {
			var actors []string
			Eventually(func(g Gomega) {
				actors = nativeNames(g, "actors")
				g.Expect(actors).To(HaveLen(1), "exactly the held Task's Actor must run")
			}, time.Minute, 2*time.Second).Should(Succeed())
			// Upstream can report a filesystem cleanup error after it has
			// terminated the Actor; the Task and workspace prove the loss.
			_, _ = kubectlATE("delete", "actor", strings.TrimPrefix(actors[0], "actors/"),
				"--atespace", wspAtespace, "--any-state")
		},
		leftovers: func(g Gomega, workspaceUID string) []string {
			left := append(nativeNames(g, "actors"), nativeNames(g, "tags")...)
			output, err := utils.Run(exec.Command("kubectl", "-n", wspNativePool, "get", "workerpools,pods,networkpolicies",
				"-l", wspNativeOwner+"="+workspaceUID, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			return append(left, utils.GetNonEmptyLines(output)...)
		},
		lostWorkspaceFails: true,
	}
}

var _ = Describe("Workspace Provider Lifecycle", Ordered, Label("workspace-provider"), func() {
	var (
		backend    workspaceBackend
		apiBaseURL string
		token      string
		cancelPF   context.CancelFunc
		pfCmd      *exec.Cmd
		runID      string
		agent      string
		suspend    string // Session-scoped class that suspends on detach
		dataWS     workspaceIdentity
		checkpoint workspacev1alpha1.ExecutionWorkspaceCheckpoint
		// Every workspace UID the specs bound, for the final leftover check.
		workspaceUIDs []string
	)

	name := func(prefix string) string { return prefix + "-" + runID }
	session := func(prefix string) string { return name(prefix + "-s") }
	marker := func(check string) string { return "[e2e:wsp-" + check + "]" }

	BeforeAll(func() {
		switch os.Getenv(workspaceProviderEnv) {
		case "sandbox":
			backend = sandboxWorkspaceBackend()
		case "substrate":
			backend = substrateWorkspaceBackend()
		default:
			Skip(workspaceProviderEnv + " selects no real workspace provider")
		}
		runID = fmt.Sprintf("%x", time.Now().Unix()%0xfffff)
		var err error
		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(18096)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancelPF, pfCmd) })
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		By("deploying aimock as the Vekil stand-in behind the provider proxy")
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(providerProxyMockLLM.delete)
		providerProxyMockLLM.deploy(projectDir)

		By("installing the " + backend.name + " backend and provider")
		backend.prepare()
		installWorkspaceProvider(backend)

		agent = name("wsp-agent")
		Expect(applyManifestJSON(runtimeAgentManifest(agent, "codex", runtimeMatrixMockCodexModel, 2, nil))).To(Succeed())
		suspend = name("wsp-suspend")
		applyWorkspaceClass(backend, suspend, true)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpWorkspaceProviderDiagnostics(backend)
		}
	})

	It("runs a Task through the workspace class and removes the workspace on detach", func() {
		class := name("wsp-delete")
		applyWorkspaceClass(backend, class, false)
		task := name("wsp-read")
		manifest := externalCodexReadTaskManifest(task, agent, class)
		Expect(applyManifestJSON(manifest)).To(Succeed())
		ws := waitTaskWorkspace(task, class, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)

		Expect(waitForTaskCompletion(task, 10*time.Minute)).To(Equal("Succeeded"))
		Expect(strings.TrimSpace(fetchTaskResultSummaryViaAPI(apiBaseURL, token, task))).To(HaveSuffix(liveRuntimeRepoSentinel))
		deleteE2ETasks(apiBaseURL, token, task)
		waitWorkspaceGone(backend, ws)
	})

	It("keeps workspace data across suspend and cold resume", func() {
		write := name("wsp-write")
		Expect(applyManifestJSON(wspTask(write, agent, suspend, session("data"), true,
			"Write the durable workspace file. "+marker("write"), "15m", nil))).To(Succeed())
		dataWS = waitTaskWorkspace(write, suspend, backend)
		workspaceUIDs = append(workspaceUIDs, dataWS.uid)
		first := waitTaskExecution(write, corev1alpha1.TaskExecutionStateSucceeded)
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
		waitRuntimePoolStopped(dataWS.pool)

		read := name("wsp-resume")
		Expect(applyManifestJSON(wspTask(read, agent, suspend, session("data"), false,
			"Read the durable workspace file. "+marker("resume"), "15m", nil))).To(Succeed())
		second := waitTaskExecution(read, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, read)).To(ContainSubstring("ORKA_WSP_READ_OK"))
		Expect(waitTaskWorkspace(read, suspend, backend).uid).To(Equal(dataWS.uid), "the Session must resume its workspace")
		Expect(second.Status.Execution.RuntimeInstanceID).NotTo(Equal(first.Status.Execution.RuntimeInstanceID),
			"cold resume must start a new runtime instance")
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
	})

	It("exports a checkpoint and restores its data into a new workspace", func() {
		checkpoint = exportCheckpoint(name("wsp-save"), dataWS, false)
		restore := name("wsp-restore")
		Expect(applyManifestJSON(wspTask(restore, agent, suspend, "", false,
			"Read the durable workspace file. "+marker("restore"), "15m", &checkpoint))).To(Succeed())
		ws := waitTaskWorkspace(restore, suspend, backend)
		Expect(ws.uid).NotTo(Equal(dataWS.uid))
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitTaskExecution(restore, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, restore)).To(ContainSubstring("ORKA_WSP_READ_OK"))
		waitWorkspaceGone(backend, ws)
	})

	It("cancels a held prompt at its Task timeout without replaying it", func() {
		task := name("wsp-timeout")
		Expect(applyManifestJSON(wspTask(task, agent, suspend, session("timeout"), true,
			"Wait for the model. "+marker("timeout"), "45s", nil))).To(Succeed())
		ws := waitTaskWorkspace(task, suspend, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		final := waitTaskExecution(task, corev1alpha1.TaskExecutionStateCancelled)
		Expect(final.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("TaskTimeout")))
		expectServedOnce(marker("timeout"))
		waitWorkspaceState(ws.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
	})

	It("settles a held prompt once across a controller restart and continues the Session", func() {
		task := name("wsp-restart")
		Expect(applyManifestJSON(wspTask(task, agent, suspend, session("data"), false,
			"Wait for the model. "+marker("restart"), "15m", nil))).To(Succeed())
		waitHeldPrompt(task, marker("restart"))

		By("restarting the controller while the prompt is held")
		stopPortForward(cancelPF, pfCmd)
		deployment, err := controllerManagerDeploymentName()
		Expect(err).NotTo(HaveOccurred())
		for _, args := range [][]string{{"rollout", "restart"}, {"rollout", "status", "--timeout=10m"}} {
			_, err := utils.Run(exec.Command("kubectl", append([]string{"-n", namespace}, append(args, "deployment/"+deployment)...)...))
			Expect(err).NotTo(HaveOccurred())
		}
		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(18096)
		Expect(err).NotTo(HaveOccurred())

		waitTaskExecution(task, terminalExecutionStates...)
		expectServedOnce(marker("restart"))
		next := name("wsp-after-restart")
		Expect(applyManifestJSON(wspTask(next, agent, suspend, session("data"), false,
			"Read the durable workspace file. "+marker("after-restart"), "15m", nil))).To(Succeed())
		waitTaskExecution(next, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, next)).To(ContainSubstring("ORKA_WSP_READ_OK"))
	})

	It("settles a prompt whose runtime instance is lost and recovers the workspace data", func() {
		task := name("wsp-lost")
		Expect(applyManifestJSON(wspTask(task, agent, suspend, session("data"), false,
			"Wait for the model. "+marker("lost"), "15m", nil))).To(Succeed())
		waitHeldPrompt(task, marker("lost"))
		backend.loseInstance(dataWS.uid)
		waitTaskExecution(task, corev1alpha1.TaskExecutionStateOutcomeUnknown)
		expectServedOnce(marker("lost"))

		if !backend.lostWorkspaceFails {
			By("continuing the Session on a replacement instance")
			next := name("wsp-after-loss")
			Expect(applyManifestJSON(wspTask(next, agent, suspend, session("data"), false,
				"Read the durable workspace file. "+marker("after-loss"), "15m", nil))).To(Succeed())
			waitTaskExecution(next, corev1alpha1.TaskExecutionStateSucceeded)
			Expect(fetchTaskResultViaAPI(apiBaseURL, token, next)).To(ContainSubstring("ORKA_WSP_READ_OK"))
			return
		}

		By("failing the workspace and exporting its last checkpoint only on explicit recovery")
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateFailed)
		recovered := exportCheckpoint(name("wsp-recover"), dataWS, true)
		restore := name("wsp-after-loss")
		Expect(applyManifestJSON(wspTask(restore, agent, suspend, "", false,
			"Read the durable workspace file. "+marker("after-loss"), "15m", &recovered))).To(Succeed())
		ws := waitTaskWorkspace(restore, suspend, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitTaskExecution(restore, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, restore)).To(ContainSubstring("ORKA_WSP_READ_OK"))
		waitWorkspaceGone(backend, ws)
	})

	It("deletes every workspace and leaves no backend resources", func() {
		for _, s := range []string{session("data"), session("timeout")} {
			deleteSessionViaAPI(apiBaseURL, token, s)
		}
		output, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", "tasks", "-o", "name"))
		Expect(err).NotTo(HaveOccurred())
		var tasks []string
		for _, task := range utils.GetNonEmptyLines(output) {
			if task = strings.TrimPrefix(task, "task.core.orka.ai/"); strings.HasSuffix(task, "-"+runID) {
				tasks = append(tasks, task)
			}
		}
		deleteE2ETasks(apiBaseURL, token, tasks...)
		for _, args := range [][]string{
			{"delete", "executionworkspacecheckpoints", "--all"},
			{"delete", "executionworkspaces", "--all"},
		} {
			_, err := utils.Run(exec.Command("kubectl", append(append([]string{"-n", namespace}, args...), "--timeout=10m")...))
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func(g Gomega) {
			output, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", "runtimepools", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(ContainSubstring("acp-ws-"))
			for _, uid := range workspaceUIDs {
				g.Expect(backend.leftovers(g, uid)).To(BeEmpty(), "workspace %s left backend resources", uid)
			}
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
	})
})

type workspaceIdentity struct{ name, uid, pool string }

func wspTask(name, agent, class, session string, create bool, prompt, timeout string,
	restore *workspacev1alpha1.ExecutionWorkspaceCheckpoint) map[string]any {
	var createRef *bool
	if session != "" {
		createRef = &create
	}
	manifest := runtimeAgentTaskManifest(name, agent, prompt, 2, nil, nil, session, createRef, nil)
	workspace := map[string]any{"classRef": map[string]any{"name": class}, "reusePolicy": "session"}
	if session == "" {
		workspace["reusePolicy"], workspace["onDetach"] = "none", "Delete"
	}
	if restore != nil {
		workspace["restoreFrom"] = map[string]any{"name": restore.Name, "uid": string(restore.UID), "digest": restore.Status.Digest}
	}
	spec := manifest["spec"].(map[string]any)
	spec["timeout"], spec["execution"] = timeout, map[string]any{"workspace": workspace}
	return manifest
}

// installWorkspaceProvider builds the provider from orka-workspace source and
// applies its bundled registration, RBAC, and Deployment.
func installWorkspaceProvider(backend workspaceBackend) {
	source := os.Getenv(workspaceSourceEnv)
	if source == "" {
		dir, err := os.MkdirTemp("", "orka-e2e-workspace-source-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		Expect(fetchPinnedOrkaWorkspaceSource(dir)).To(Succeed())
		source = dir
	}
	image := "orka-workspace-" + backend.name + ":e2e"
	_, err := utils.Run(exec.Command("docker", "build", "-t", image,
		"-f", filepath.Join(source, "providers", backend.name, "Dockerfile"), source))
	Expect(err).NotTo(HaveOccurred())
	ref, err := pushImageToLocalRegistry(e2eRegistryAddress, image, "orka/workspace-provider-"+backend.name+":e2e")
	Expect(err).NotTo(HaveOccurred())

	_, err = utils.Run(exec.Command("kubectl", "apply", "--server-side", "-k", filepath.Join(source, "config", "policy")))
	Expect(err).NotTo(HaveOccurred())
	rendered, err := utils.Run(exec.Command("kubectl", "kustomize", filepath.Join(source, "providers", backend.name, "config")))
	Expect(err).NotTo(HaveOccurred())
	cmd := exec.Command("kubectl", "create", "--dry-run=client", "-o", "json", "-f", "-")
	cmd.Stdin = strings.NewReader(rendered)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	var list struct {
		Items []map[string]any `json:"items"`
	}
	Expect(json.Unmarshal([]byte(output), &list)).To(Succeed())
	var items []any
	for _, item := range list.Items {
		switch item["kind"] {
		case backend.profileKind:
			continue // the bundle's sample profile targets the default namespace
		case "Deployment":
			container := item["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			container["image"], container["imagePullPolicy"] = ref, "IfNotPresent"
			if backend.patchProvider != nil {
				backend.patchProvider(item)
			}
		}
		items = append(items, item)
	}
	Expect(applyManifestJSON(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})).To(Succeed())
	_, err = utils.Run(exec.Command("kubectl", "-n", wspProviderNS, "rollout", "status",
		"deployment/"+backend.deployment, "--timeout=5m"))
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		var provider workspacev1alpha1.ExecutionWorkspaceProvider
		kubectlJSON(g, &provider, "get", "executionworkspaceprovider", backend.name)
		g.Expect(provider.Status.SupportedContracts).To(ContainElement("orka.workspace.lifecycle.v1"))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

func applyWorkspaceClass(backend workspaceBackend, name string, suspend bool) {
	lifecycle := map[string]any{
		"defaultOnDetach": "Delete", "allowedOnDetach": []string{"Delete"},
		"detachTimeout": "5m", "maxLifetime": "2h",
		"deletionPolicy": map[string]any{"providerResources": "Delete", "persistentVolumes": "Delete", "checkpoints": "Delete"},
	}
	reuse := []string{"None"}
	if suspend {
		lifecycle["defaultOnDetach"], lifecycle["allowedOnDetach"] = "Suspend", []string{"Suspend", "Delete"}
		reuse = []string{"None", "Session"}
	}
	Expect(applyManifestJSON(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
		map[string]any{
			"apiVersion": backend.profileGroup + "/v1alpha1", "kind": backend.profileKind,
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"spec":     backend.profileSpec(suspend),
		},
		map[string]any{
			"apiVersion": "workspace.orka.ai/v1alpha1", "kind": "ExecutionWorkspaceClass",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"spec": map[string]any{
				"providerRef":        map[string]any{"name": backend.name},
				"parametersRef":      map[string]any{"group": backend.profileGroup, "kind": backend.profileKind, "name": name},
				"mode":               "Interactive",
				"requiredFeatures":   []string{"acp.runtime.v2"},
				"allowedReuseScopes": reuse,
				"lifecycle":          lifecycle,
			},
		},
	}})).To(Succeed())
	Eventually(func(g Gomega) {
		var class workspacev1alpha1.ExecutionWorkspaceClass
		kubectlJSON(g, &class, "get", "executionworkspaceclass", name, "-n", namespace)
		ready := false
		for _, condition := range class.Status.Conditions {
			ready = ready || condition.Type == "Ready" && condition.Status == "True"
		}
		g.Expect(ready).To(BeTrue(), "class %s is not Ready: %+v", name, class.Status.Conditions)
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

// waitTaskWorkspace returns the workspace a Task bound, after checking that
// the provider-neutral Task projection names the class and nothing native.
func waitTaskWorkspace(task, class string, backend workspaceBackend) workspaceIdentity {
	var ws workspaceIdentity
	Eventually(func(g Gomega) {
		current := fetchTaskForACPAssertion(g, task)
		projection := current.Status.ExecutionWorkspace
		g.Expect(projection).NotTo(BeNil())
		g.Expect(projection.ClassRef).NotTo(BeNil())
		g.Expect(projection.ClassRef.Name).To(Equal(class))
		g.Expect(projection.WorkspaceRef).NotTo(BeNil())
		g.Expect(projection.Placement).To(BeNil(), "Task workspace status must stay provider-neutral")
		g.Expect(current.Status.Execution).NotTo(BeNil())
		g.Expect(current.Status.Execution.RuntimePoolName).To(HavePrefix("acp-ws-"))
		var workspace workspacev1alpha1.ExecutionWorkspace
		kubectlJSON(g, &workspace, "get", "executionworkspace", projection.WorkspaceRef.Name, "-n", namespace)
		g.Expect(string(workspace.UID)).To(Equal(projection.WorkspaceRef.UID))
		g.Expect(workspace.Spec.ProviderBinding.Name).To(Equal(backend.name))
		ws = workspaceIdentity{name: workspace.Name, uid: string(workspace.UID), pool: current.Status.Execution.RuntimePoolName}
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	return ws
}

func waitTaskExecution(task string, states ...corev1alpha1.TaskExecutionState) corev1alpha1.Task {
	var current corev1alpha1.Task
	EventuallyWithOffset(1, func(g Gomega) {
		current = fetchTaskForACPAssertion(g, task)
		g.Expect(current.Status.Execution).NotTo(BeNil())
		g.Expect(current.Status.Execution.State).To(BeElementOf(states), "Task %s execution", task)
	}, 15*time.Minute, 2*time.Second).Should(Succeed())
	return current
}

// waitHeldPrompt waits until aimock holds the Task's prompt in flight.
func waitHeldPrompt(task, promptMarker string) {
	waitTaskExecution(task, corev1alpha1.TaskExecutionStateRunning)
	EventuallyWithOffset(1, func(g Gomega) {
		requests, err := providerProxyMockLLM.requestsFor(promptMarker)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requests).NotTo(BeEmpty(), "aimock has not received %s", promptMarker)
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
}

// expectServedOnce checks that a held prompt reached the model exactly once.
func expectServedOnce(promptMarker string) {
	requests, err := providerProxyMockLLM.requestsFor(promptMarker)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, requests).To(HaveLen(1), "%s must not be replayed", promptMarker)
}

func waitWorkspaceState(name string, state workspacev1alpha1.ExecutionWorkspaceState) {
	EventuallyWithOffset(1, func(g Gomega) {
		var workspace workspacev1alpha1.ExecutionWorkspace
		kubectlJSON(g, &workspace, "get", "executionworkspace", name, "-n", namespace)
		g.Expect(workspace.Status.State).To(Equal(state))
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
}

func waitWorkspaceGone(backend workspaceBackend, ws workspaceIdentity) {
	EventuallyWithOffset(1, func(g Gomega) {
		for _, kind := range []string{"executionworkspace/" + ws.name, "runtimepool/" + ws.pool} {
			output, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", kind, "--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "%s must be deleted", kind)
		}
		g.Expect(backend.leftovers(g, ws.uid)).To(BeEmpty())
	}, 10*time.Minute, 5*time.Second).Should(Succeed())
}

func exportCheckpoint(name string, ws workspaceIdentity, recover bool) workspacev1alpha1.ExecutionWorkspaceCheckpoint {
	spec := map[string]any{"workspaceRef": map[string]any{"name": ws.name, "uid": ws.uid}}
	if recover {
		spec["recoverLastCheckpoint"] = true
	}
	ExpectWithOffset(1, applyManifestJSON(map[string]any{
		"apiVersion": "workspace.orka.ai/v1alpha1", "kind": "ExecutionWorkspaceCheckpoint",
		"metadata": map[string]any{"name": name, "namespace": namespace}, "spec": spec,
	})).To(Succeed())
	var checkpoint workspacev1alpha1.ExecutionWorkspaceCheckpoint
	EventuallyWithOffset(1, func(g Gomega) {
		kubectlJSON(g, &checkpoint, "get", "executionworkspacecheckpoint", name, "-n", namespace)
		g.Expect(checkpoint.Status.Phase).To(Equal("Ready"))
		g.Expect(checkpoint.Status.Digest).To(MatchRegexp(`^sha256:[a-f0-9]{64}$`))
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
	return checkpoint
}

// deleteE2ETasks finalizes Tasks through the suite's exact cleanup path.
func deleteE2ETasks(apiBaseURL, token string, tasks ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cleanup, err := newE2ECleanup(apiBaseURL, token)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, cleanup.tasks(ctx, tasks, false)).To(Succeed())
}

func deleteSessionViaAPI(apiBaseURL, token, session string) {
	url := fmt.Sprintf("%s/api/v1/sessions/%s?namespace=%s", strings.TrimRight(apiBaseURL, "/"), session, namespace)
	EventuallyWithOffset(1, func(g Gomega) {
		body, status, err := doAuthorizedJSONRequest(http.MethodDelete, url, token, "", "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(status).To(BeElementOf(http.StatusNoContent, http.StatusNotFound), body)
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

func dumpWorkspaceProviderDiagnostics(backend workspaceBackend) {
	for _, args := range [][]string{
		{"-n", namespace, "get", "tasks,runtimepools,executionworkspaces,executionworkspacecheckpoints", "-o", "yaml"},
		{"-n", wspProviderNS, "logs", "deployment/" + backend.deployment, "--tail=200"},
	} {
		output, _ := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n", strings.Join(args, " "), output)
	}
}
