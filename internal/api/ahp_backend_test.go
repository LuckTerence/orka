/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/executionmode"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	ahpTestNamespace = "team"
	ahpTestRepo      = "https://github.com/octo/widgets"
	ahpTestDir       = "file:///workspace/src/octo/widgets"
	ahpTestSession   = "ahp-session:/one"
)

type ahpTestEnv struct {
	t       *testing.T
	kube    client.Client
	live    *liveevents.Broker
	backend *ahpBackend
	host    *ahphost.Host
	server  *httptest.Server
	tokens  *ahphost.TokenService

	mu        sync.Mutex
	decisions []string
}

func ahpTestAgent(name string, runtime corev1alpha1.AgentRuntimeType, model string) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ahpTestNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: runtime},
			Model:   &corev1alpha1.ModelConfig{Name: model},
		},
	}
}

func newAHPTestEnv(t *testing.T, objects ...client.Object) *ahpTestEnv {
	t.Helper()
	objects = append(objects,
		ahpTestAgent("copilot-haiku", corev1alpha1.AgentRuntimeCopilot, "claude-haiku-5.5"),
		ahpTestAgent("copilot-gpt", corev1alpha1.AgentRuntimeCopilot, "gpt-5.4-nano"),
		ahpTestAgent("claude-sonnet", corev1alpha1.AgentRuntimeClaude, "claude-sonnet-5"),
		// An Agent without a model and one on a custom runtime are not offered.
		&corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "no-model", Namespace: ahpTestNamespace},
			Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}}},
	)
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objects...).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &ahphost.TokenService{Store: s}
	registry, err := ahphost.NewRegistry()
	require.NoError(t, err)
	host, err := ahphost.New(ahphost.Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	live := liveevents.NewBroker(0)
	handlers := NewHandlers(HandlersConfig{Client: kube, APIReader: kube, ExecutionMode: executionmode.HarnessV2, ResultStore: s})
	env := &ahpTestEnv{t: t, kube: kube, live: live, host: host, tokens: tokens}
	backend := newAHPBackend(handlers, s, live, AHPBackendConfig{GitCredentialSecret: "git-token"})
	backend.emitterFor = host.Emitter
	backend.decideApproval = func(_ context.Context, ui *UserInfo, namespace, task, approvalID string, approve bool) error {
		env.mu.Lock()
		defer env.mu.Unlock()
		env.decisions = append(env.decisions, strings.Join([]string{ui.Subject, namespace, task, approvalID, map[bool]string{true: "approve", false: "decline"}[approve]}, " "))
		return nil
	}
	require.NoError(t, host.RegisterBackend(backend))
	env.backend = backend
	env.server = httptest.NewServer(host)
	t.Cleanup(env.server.Close)
	return env
}

func (e *ahpTestEnv) connect(subject string) *ahp.Client {
	e.t.Helper()
	minted, err := e.tokens.Mint(context.Background(), ahphost.MintRequest{
		Issuer: "https://issuer.example", Subject: subject, Username: subject, Namespace: ahpTestNamespace,
	})
	require.NoError(e.t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport, err := ahpws.Connect(ctx, "ws"+strings.TrimPrefix(e.server.URL, "http")+"/?tkn="+minted.Token)
	require.NoError(e.t, err)
	c, err := ahp.Connect(ctx, transport, ahp.DefaultConfig())
	require.NoError(e.t, err)
	e.t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	_, err = c.Initialize(ctx, "client-"+subject, []string{"0.9.0"}, nil)
	require.NoError(e.t, err)
	return c
}

func ahpCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// setUpSession adds the project and creates a session in it.
func (e *ahpTestEnv) setUpSession(c *ahp.Client, mode string) string {
	e.t.Helper()
	ctx := ahpCtx(e.t)
	var cloned struct {
		Project map[string]any `json:"project"`
	}
	require.NoError(e.t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo + ".git"}, &cloned))
	require.Equal(e.t, "/workspace/src/octo/widgets", cloned.Project["path"])
	require.Equal(e.t, "ready", cloned.Project["status"])
	provider := "copilot"
	modeJSON, _ := json.Marshal(mode)
	require.NoError(e.t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: ahpTestSession, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
		Config: map[string]json.RawMessage{"approvalMode": modeJSON, "baseBranch": json.RawMessage(`"main"`)},
	}, nil))
	result, _, err := c.Subscribe(ctx, ahpTestSession)
	require.NoError(e.t, err)
	state := result.Snapshot.State.Session
	require.Len(e.t, state.Chats, 1)
	return state.Chats[0].Resource
}

func (e *ahpTestEnv) waitForTask(name string) *corev1alpha1.Task {
	e.t.Helper()
	task := &corev1alpha1.Task{}
	require.Eventually(e.t, func() bool {
		return e.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: name}, task) == nil
	}, 5*time.Second, 20*time.Millisecond, "the turn's Task was never created")
	return task
}

func (e *ahpTestEnv) setPhase(name string, phase corev1alpha1.TaskPhase) {
	e.t.Helper()
	task := &corev1alpha1.Task{}
	require.NoError(e.t, e.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: name}, task))
	task.Status.Phase = phase
	require.NoError(e.t, e.kube.Status().Update(context.Background(), task))
}

func (e *ahpTestEnv) publish(task string, update *harnessv2.UpdateEvent) {
	e.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: task, Runtime: &harnessv2.Event{Type: harnessv2.EventUpdate, Update: update}})
}

func waitForActionType(t *testing.T, sub *ahp.Subscription, want string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event := <-sub.Events():
			if action, ok := event.(ahp.SubscriptionEventAction); ok {
				raw, _ := json.Marshal(action.Envelope.Action)
				var head struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(raw, &head)
				if head.Type == want {
					return
				}
			}
		case <-timeout:
			t.Fatalf("no %s action arrived", want)
		}
	}
}

func TestAHPBackendOffersBuiltInRuntimesAndProjects(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	result, _, err := c.Subscribe(ctx, ahptypes.RootResourceURI)
	require.NoError(t, err)
	root := result.Snapshot.State.Root
	require.JSONEq(t, `{"available":true}`, string(root.Meta[ahpCopilotProjectManagementMeta]))
	providers := map[string][]string{}
	for _, agent := range root.Agents {
		for _, model := range agent.Models {
			providers[agent.Provider] = append(providers[agent.Provider], model.Id)
		}
		require.Empty(t, agent.ProtectedResources, "Orka never asks for the person's own credentials")
	}
	require.Equal(t, map[string][]string{"copilot": {"gpt-5.4-nano", "claude-haiku-5.5"}, "claude": {"claude-sonnet-5"}}, providers, "Agents are offered in name order")

	for _, bad := range []string{"http://github.com/a/b", "https://user:pw@github.com/a/b", "https://github.com/a", "https://github.com/a/b?x=1"} {
		err := c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": bad}, nil)
		require.Error(t, err, bad)
	}
	provider := "copilot"
	err = c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: "ahp-session:/elsewhere", Provider: &provider, WorkingDirectories: []string{"file:///etc"},
	}, nil)
	require.Error(t, err, "a session outside a project is refused")
}

//nolint:gocyclo // One end-to-end turn checks every mapped action against the reduced state.
func TestAHPBackendRunsATurnAsATask(t *testing.T) {
	gitSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: ahpTestNamespace}, Data: map[string][]byte{"token": []byte("not-a-real-token")}}
	env := newAHPTestEnv(t, gitSecret)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "manual")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "Add a comment and run the tests", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)

	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.Equal(t, "copilot-haiku", task.Spec.AgentRef.Name)
	require.Equal(t, "Add a comment and run the tests", task.Spec.Prompt)
	require.True(t, task.Spec.SessionRef.Create)
	require.True(t, task.Spec.SessionRef.Append)
	require.Equal(t, ahpTestRepo, task.Spec.Workspace.GitRepo)
	require.Equal(t, "main", task.Spec.Workspace.Branch)
	require.Equal(t, corev1alpha1.WorkspaceIntentWrite, task.Spec.Workspace.Intent, "a configured git credential makes turns writable")
	require.Equal(t, "git-token", task.Spec.Workspace.PublicationCredentialRef.Name)
	require.Equal(t, "alice", task.Spec.RequestedBy.Subject, "the Task is the person's")
	require.Equal(t, labels.RequestedBySourceAPI, task.Annotations[labels.AnnotationRequestedBySource])

	// A second message while the turn runs is refused.
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-x", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "again", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)

	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantThoughtChunk, AssistantThought: &harnessv2.AssistantMessageChunk{Text: "Thinking."}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "Running "}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "them."}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCall, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Name: "bash", Title: "Run tests", Status: harnessv2.ToolCallStatusPending,
		RawInput: json.RawMessage(`{"command":"go test ./..."}`),
	}})
	env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, Approval: &liveevents.Approval{
		ID: "approval-1", State: liveevents.ApprovalPending, ToolCallID: "tool-1", ToolName: "bash", Title: "Run tests",
	}})
	waitForActionType(t, chatSub, "chat/toolCallReady")
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
		Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: "turn-1", ToolCallId: "tool-1", Approved: true,
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return len(env.decisions) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "alice team "+taskName+" approval-1 approve", env.decisions[0])

	old := "package widgets\n"
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Status: harnessv2.ToolCallStatusCompleted,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "ok"}},
		Diffs:   []harnessv2.ToolCallDiff{{Path: "widgets.go", OldText: &old, NewText: "// Widgets.\npackage widgets\n"}},
	}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "Done."}})
	waitForActionType(t, chatSub, "chat/delta")
	env.setPhase(taskName, corev1alpha1.TaskPhaseSucceeded)
	waitForActionType(t, chatSub, "chat/turnComplete")

	snapshot, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	turns := snapshot.Snapshot.State.Chat.Turns
	require.Len(t, turns, 1, "the refused second message never became a turn")
	require.Equal(t, ahptypes.TurnStateComplete, turns[0].State)
	var kinds []string
	var text strings.Builder
	for _, part := range turns[0].ResponseParts {
		switch p := part.Value.(type) {
		case *ahptypes.MarkdownResponsePart:
			kinds = append(kinds, "markdown")
			text.WriteString(p.Content)
		case *ahptypes.ReasoningResponsePart:
			kinds = append(kinds, "reasoning")
		case *ahptypes.ToolCallResponsePart:
			kinds = append(kinds, "toolCall")
			raw, _ := json.Marshal(p.ToolCall)
			require.Contains(t, string(raw), `"status":"completed"`)
			require.Contains(t, string(raw), `"toolName":"bash"`)
		}
	}
	require.Equal(t, []string{"reasoning", "markdown", "toolCall", "markdown"}, kinds)
	require.Equal(t, "Running them.Done.", text.String())

	changes, _, err := c.Subscribe(ctx, record.Changeset)
	require.NoError(t, err)
	files := changes.Snapshot.State.Changeset.Files
	require.Len(t, files, 1)
	require.Equal(t, ahpTestDir+"/widgets.go", files[0].Edit.After.Uri)
	require.Equal(t, int64(1), *files[0].Edit.Diff.Added)
	require.Equal(t, int64(0), *files[0].Edit.Diff.Removed)
	var after, before ahptypes.ResourceReadResult
	require.NoError(t, c.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: record.Changeset, Uri: ahpTestDir + "/widgets.go"}, &after))
	require.Equal(t, "// Widgets.\npackage widgets\n", after.Data)
	require.NoError(t, c.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: record.Changeset, Uri: files[0].Edit.Before.Content.Uri}, &before))
	require.Equal(t, old, before.Data)

	var listed ahptypes.ListSessionsResult
	require.NoError(t, c.Request(ctx, "listSessions", ahptypes.ListSessionsParams{}, &listed))
	require.Equal(t, "Add a comment and run the tests", listed.Items[0].Title)
	require.Equal(t, ahptypes.SessionStatusIdle, listed.Items[0].Status)
	require.Equal(t, int64(1), *listed.Items[0].Changes.Files)

	// The next turn continues the Session; another model is refused.
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-2", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "switch", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "gpt-5.4-nano"}},
	}})
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-2", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "and again", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)
	second := env.waitForTask(record.OrkaSession + "-t2")
	require.False(t, second.Spec.SessionRef.Create)
	require.Equal(t, "and again", second.Spec.Prompt)
}

func TestAHPBackendCancelsATurn(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "sleep", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.Equal(t, corev1alpha1.WorkspaceIntentRead, task.Spec.Workspace.Intent, "without a git credential turns are read-only")
	require.Eventually(t, func() bool {
		return env.backend.activeTurn(ahphost.UserKey("https://issuer.example", "alice"), ahpTestSession) != nil
	}, 5*time.Second, 20*time.Millisecond)

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{
		Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: "turn-1", Duration: 10,
	}})
	require.NoError(t, err)
	waitForActionType(t, chatSub, "chat/turnCancelled")
	require.Eventually(t, func() bool {
		current := &corev1alpha1.Task{}
		_ = env.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, current)
		_, ok := current.Annotations[labels.AnnotationCancelRequested]
		return ok
	}, 5*time.Second, 20*time.Millisecond, "cancelling the turn cancels its Task")
	env.setPhase(taskName, corev1alpha1.TaskPhaseCancelled)
	require.Eventually(t, func() bool {
		return env.backend.activeTurn(ahphost.UserKey("https://issuer.example", "alice"), ahpTestSession) == nil
	}, 5*time.Second, 20*time.Millisecond)
	snapshot, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	turns := snapshot.Snapshot.State.Chat.Turns
	require.Len(t, turns, 1)
	require.Equal(t, ahptypes.TurnStateCancelled, turns[0].State)
}

func TestAHPBackendKeepsUsersApart(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := env.connect("alice")
	env.setUpSession(alice, "manual")
	bob := env.connect("bob")
	ctx := ahpCtx(t)
	var projects struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, bob.Request(ctx, ahpExtensionListProjects, map[string]any{}, &projects))
	require.Empty(t, projects.Projects, "projects are per person")
	var listed ahptypes.ListSessionsResult
	require.NoError(t, bob.Request(ctx, "listSessions", ahptypes.ListSessionsParams{}, &listed))
	require.Empty(t, listed.Items)
}
