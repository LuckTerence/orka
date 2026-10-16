package controller

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type generationCommitFailureStore struct {
	store.DurableControlStore
	failures int
}

func (s *generationCommitFailureStore) CommitSessionRuntimeGeneration(ctx context.Context, request store.CommitSessionRuntimeGenerationRequest) (*store.SessionControl, error) {
	if s.failures > 0 {
		s.failures--
		return nil, errors.New("injected generation commit failure")
	}
	return s.DurableControlStore.CommitSessionRuntimeGeneration(ctx, request)
}

// A failure between RuntimeSession creation and prompt submission requeues
// the Task with the provider process preserved. The retry reuses a process
// that never received a prompt, so the toolbox note is still due; it stops
// being due only once a prompt is accepted.
func TestACPDispatcherToolboxNoteSurvivesPreSubmissionRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	creates, prompts := 0, 0
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "toolbox-note-retry", "88888888-8888-8888-8888-888888888888",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				onPrompt: func(harnessv2.StartPromptRequest) { prompts++ },
			}, func(harnessv2.CreateRuntimeSessionRequest) { creates++ })
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "toolbox-note-retry", Create: true, Append: true}
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	failure := &generationCommitFailureStore{DurableControlStore: persistence, failures: 1}
	fixture.dispatcher.EventStore = persistence
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: failure, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
	})
	require.NoError(t, err)
	fixture.dispatcher.Sessions = continuity

	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))
	require.Equal(t, 1, creates)
	require.Zero(t, prompts, "the injected commit failure happens before prompt submission")
	current := fixture.currentTask(t, ctx)
	binding := fixture.dispatcher.currentRuntimeSessionBinding(current.Status.Execution.RuntimeSessionUID)
	require.NotNil(t, binding)
	require.False(t, binding.PromptDelivered, "a created but never-prompted process has not received the note")

	reserved, target, err = fixture.dispatcher.reserveTask(ctx, current)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	retry := &acpTaskSession{Binding: *binding, Reused: true}
	require.True(t, acpToolboxNoteDue(retry), "a reused never-prompted process still gets the note")
	require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))
	require.Equal(t, 1, creates, "the retry must reuse the preserved process")
	require.Equal(t, 1, prompts)
	current = fixture.currentTask(t, ctx)
	binding = fixture.dispatcher.currentRuntimeSessionBinding(current.Status.Execution.RuntimeSessionUID)
	require.NotNil(t, binding)
	require.True(t, binding.PromptDelivered, "an accepted prompt delivers the note")
	require.False(t, acpToolboxNoteDue(&acpTaskSession{Binding: *binding, Reused: true}))
}

// Continuation turns reuse a live binding with its delivery record, so the
// note is not repeated; any replacement process starts without it.
func TestACPToolboxNoteFollowsRuntimeSessionPlanning(t *testing.T) {
	profileDigest := harnessv2.ProfileDigest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	rotatedDigest := harnessv2.ProfileDigest("sha256:2222222222222222222222222222222222222222222222222222222222222222")
	mcpDigest := "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	control := store.SessionControl{SessionUID: "session-uid", Availability: store.SessionAvailable}
	delivered := &ACPRuntimeSessionBinding{
		SessionUID: "session-uid", Generation: 3, ProfileDigest: profileDigest, MCPDigest: mcpDigest,
		RuntimeInstanceID: "instance", SupervisorBootID: "boot", PromptDelivered: true,
	}
	require.True(t, acpToolboxNoteDue(nil), "standalone Tasks always start a provider conversation")

	plan, err := PlanACPRuntimeSession(control, delivered, profileDigest, mcpDigest, "instance", "boot")
	require.NoError(t, err)
	require.False(t, plan.Recreate)
	require.False(t, acpToolboxNoteDue(&acpTaskSession{Binding: plan.Binding, Reused: true}), "a continuation turn must not repeat the note")

	for name, replan := range map[string]func() (ACPRuntimeSessionPlan, error){
		"profile rotated": func() (ACPRuntimeSessionPlan, error) {
			return PlanACPRuntimeSession(control, delivered, rotatedDigest, mcpDigest, "instance", "boot")
		},
		"runtime lost": func() (ACPRuntimeSessionPlan, error) {
			return PlanACPRuntimeSession(control, delivered, profileDigest, mcpDigest, "instance", "boot-2")
		},
		"initial": func() (ACPRuntimeSessionPlan, error) {
			return PlanACPRuntimeSession(control, nil, profileDigest, mcpDigest, "instance", "boot")
		},
	} {
		plan, err := replan()
		require.NoError(t, err, name)
		require.True(t, plan.Recreate, name)
		require.True(t, acpToolboxNoteDue(&acpTaskSession{Binding: plan.Binding}), "%s: a new process gets the note", name)
	}
}
