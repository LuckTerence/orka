/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store/sqlite"
)

// terminalBackend is a fake backend that runs terminals and records input.
type terminalBackend struct {
	*fakeBackend
	mu       sync.Mutex
	inputs   []string
	running  map[string]bool
	disposed []string
}

func (b *terminalBackend) CreateTerminal(_ context.Context, _ Principal, params ahptypes.CreateTerminalParams, _ Emitter) (ahptypes.TerminalState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running[params.Channel] = true
	return ahptypes.TerminalState{Title: "shell", Claim: params.Claim,
		Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}}}, nil
}

func (b *terminalBackend) DisposeTerminal(_ context.Context, _ Principal, channel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.running, channel)
	b.disposed = append(b.disposed, channel)
	return nil
}

func (b *terminalBackend) Terminals(context.Context, Principal) ([]ahptypes.TerminalInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ahptypes.TerminalInfo, 0, len(b.running))
	for channel := range b.running {
		out = append(out, ahptypes.TerminalInfo{Resource: channel, Title: "shell",
			Claim:     ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}},
			Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}}})
	}
	return out, nil
}

func (b *terminalBackend) AuthorizeClientAction(context.Context, Principal, string, ahptypes.StateAction) error {
	return nil
}

func (b *terminalBackend) HandleClientAction(_ context.Context, _ Principal, _ string, action ahptypes.StateAction, _ Emitter) {
	if input, ok := action.Value.(*ahptypes.TerminalInputAction); ok {
		time.Sleep(time.Millisecond) // a slow handler must not reorder input
		b.mu.Lock()
		b.inputs = append(b.inputs, input.Data)
		b.mu.Unlock()
	}
}

func TestTerminalsRouteToTheirBackendInOrder(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{}}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)

	claim := ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}}
	require.NoError(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil))
	err = client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil)
	require.Equal(t, ahptypes.ErrorCodeAlreadyExists, rpcCode(t, err))

	result, _, err := client.Subscribe(ctx, rootChannel)
	require.NoError(t, err)
	require.Len(t, result.Snapshot.State.Root.Terminals, 1)

	for i := range 50 {
		_, err := client.Dispatch(ctx, "ahp-terminal:/one", ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{
			Type: ahptypes.ActionTypeTerminalInput, Data: fmt.Sprintf("%d;", i)}})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return len(backend.inputs) == 50
	}, 5*time.Second, 10*time.Millisecond)
	backend.mu.Lock()
	var want strings.Builder
	for i := range 50 {
		fmt.Fprintf(&want, "%d;", i)
	}
	require.Equal(t, want.String(), strings.Join(backend.inputs, ""), "input reaches the backend in order")
	backend.mu.Unlock()

	// Another user cannot use the terminal.
	other := env.connect(env.mint("bob").Token)
	_, err = other.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	_, _, err = other.Subscribe(ctx, "ahp-terminal:/one")
	require.Error(t, err)

	require.NoError(t, client.Request(ctx, "disposeTerminal", ahptypes.DisposeTerminalParams{Channel: "ahp-terminal:/one"}, nil))
	require.Equal(t, []string{"ahp-terminal:/one"}, backend.disposed)
	_, _, err = client.Subscribe(ctx, "ahp-terminal:/one")
	require.Error(t, err)
}

func TestTrimTerminalContentKeepsTheEnd(t *testing.T) {
	state := &ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{
		{Value: &ahptypes.TerminalUnclassifiedPart{Type: "unclassified", Value: strings.Repeat("a", maxStoredTerminalContent)}},
		{Value: &ahptypes.TerminalUnclassifiedPart{Type: "unclassified", Value: strings.Repeat("é", 100)}},
	}}
	trimTerminalContent(state)
	total := 0
	for _, part := range state.Content {
		total += terminalPartSize(part)
	}
	require.LessOrEqual(t, total, maxStoredTerminalContent)
	last := state.Content[len(state.Content)-1].Value.(*ahptypes.TerminalUnclassifiedPart)
	require.Equal(t, strings.Repeat("é", 100), last.Value, "the newest output is kept")
}
