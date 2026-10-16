package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestKagentProfileHasOnlyFrozenBrokeredCapabilities(t *testing.T) {
	t.Setenv(EnvKagentAdapterDigest, testDigest("kagent-adapter"))
	t.Setenv(EnvAgentConfigurationDigest, testDigest("kagent-config"))
	profile, err := providerProfile(providerKindKagent, "kagent/kagent/probe", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Command != "/orka-runtime-kagent" || strings.Join(profile.Args, " ") != "--protocol acp --config /agent/kagent.json" ||
		profile.AdapterName != kagentAdapterName || profile.AdapterDigest != testDigest("kagent-adapter") {
		t.Fatalf("kagent child identity is not fixed: %#v", profile)
	}
	capabilities := providerCapabilities(providerKindKagent, "kagent/kagent/probe")
	if capabilities.SupportsPermissions || capabilities.SupportsBrokeredToolApprovals || capabilities.SupportsImages ||
		capabilities.SupportsAudio || capabilities.SupportsEmbeddedResources || !capabilities.SupportsCancel || !capabilities.SupportsTools {
		t.Fatal("kagent advertised capabilities its ACP child does not provide")
	}
	request := agentKitBrokeredProjectionRequest(t)
	request.Profile.ProviderKind = providerKindKagent
	request.Profile.Model = "kagent/kagent/probe"
	if _, err := profile.ProjectSession(request, acp.SessionPaths{}, ProviderProxyBinding{}); err != nil {
		t.Fatal(err)
	}
	environment, err := profile.EnvironmentForSession(request, acp.SessionPaths{}, ProviderProxyBinding{BaseURL: "http://127.0.0.1:1/_orka/provider/route/v1", Credential: "session-credential"})
	if err != nil {
		t.Fatal(err)
	}
	if environment[kagentProviderBaseURLEnv] != "http://127.0.0.1:1/_orka/provider/route/v1" || environment[kagentProviderTokenEnv] != "session-credential" ||
		environment[kagentModelEnv] != "kagent/kagent/probe" || environment[kagentConfigDigestEnv] != testDigest("kagent-config") || len(environment) != 4 {
		t.Fatalf("kagent child environment = %#v", environment)
	}
	request.AgentConfiguration = &harnessv2.AgentSessionConfiguration{}
	if _, err := profile.ProjectSession(request, acp.SessionPaths{}, ProviderProxyBinding{}); err == nil {
		t.Fatal("kagent accepted a per-Task configuration override")
	}
}

func TestKagentProxyRequiresLifecycleBrokerEndpoint(t *testing.T) {
	_, _, err := (ProviderProxyConfig{
		UpstreamBaseURL: "http://127.0.0.1:8091", UpstreamBearerToken: testUpstreamToken,
		ProviderKind: providerKindKagent, Model: "test-model",
	}).normalized()
	if err == nil || !strings.Contains(err.Error(), "external kagent provider proxy must target the lifecycle broker /v1 endpoint") {
		t.Fatalf("kagent proxy without broker /v1 endpoint error = %v", err)
	}
}

func TestKagentProxyBindsControllerContextAndRestrictsRoutes(t *testing.T) {
	observed := make(chan foundryBrokerContext, 4)
	session, binding, prompt := newLifecycleBrokerTestSession(t, providerKindKagent, kagentAdapterName, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, proof := foundryTestProof(t, r)
		if r.URL.Path == "/v1/responses" {
			observed <- value
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"opaque-response","status":"completed","output":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	if session.foundry == nil || session.foundry.label != "kagent" {
		t.Fatal("kagent session has no lifecycle broker binding")
	}
	body := []byte(`{"model":"test-model","stream":true,"store":true,"input":"hello"}`)
	response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, body, http.Header{
		foundryContextHeader: []string{"child-selected-owner"},
	})
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("kagent inference status = %d", response.StatusCode)
	}
	select {
	case actual := <-observed:
		if actual.Owner != session.foundry.owner || actual.PromptID != prompt.Metadata.PromptID || actual.InvocationSequence != 1 {
			t.Fatalf("broker context was not derived from the controller request: %#v", actual)
		}
	case <-time.After(time.Second):
		t.Fatal("no broker inference observed")
	}
	for _, path := range []string{"/chat/completions", "/v1/responses", "/models"} {
		response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+path, binding.Credential, body, nil)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("kagent proxy allowed %s: status %d", path, response.StatusCode)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := session.foundry.settle(ctx, string(prompt.Metadata.PromptID)); err != nil {
		t.Fatalf("kagent settlement was not proven: %v", err)
	}
}

func newLifecycleBrokerTestSession(t *testing.T, providerKind, adapterName string, handler http.Handler) (*providerProxySession, ProviderProxyBinding, harnessv2.StartPromptRequest) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	proxy := newTestProviderProxy(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamBearerToken: testUpstreamToken,
		ProviderKind: providerKind, Model: "test-model",
	})
	cfg, profile := newTestConfigWithUpstream(t, "immediate", upstream.URL+"/v1", testUpstreamToken)
	profile.ProviderKind = providerKind
	profile.AgentConfigurationDigest = testDigest(providerKind + "-config")
	profile.AdapterDigests = map[string]string{adapterName: testDigest(providerKind + "-adapter")}
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fence.RuntimeProfileDigest = profileDigest
	create := testCreateSessionRequest(t, cfg, profile)
	create.AgentConfiguration = nil
	create.Metadata.RequestDigest = ""
	sealRequest(t, &create.Metadata.RequestDigest, create)
	session, binding, err := proxy.newSessionForRequest(create)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = session.wait(ctx)
	})
	request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	if err := session.activatePrompt(request, 5, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return session, binding, request
}
