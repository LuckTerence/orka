/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
)

type ahpRecordingEmitter struct {
	mu   sync.Mutex
	data []string
}

func (e *ahpRecordingEmitter) Emit(_ context.Context, _ string, actions ...ahptypes.StateAction) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, action := range actions {
		if data, ok := action.Value.(*ahptypes.TerminalDataAction); ok {
			e.data = append(e.data, data.Data)
		}
	}
	return nil
}

func TestAHPTerminalOutputNeverSplitsACharacter(t *testing.T) {
	emit := &ahpRecordingEmitter{}
	out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t")
	text := strings.Repeat("héllo wörld ✓ ", 5000) // larger than one chunk
	raw := []byte(text)
	// Write in awkward pieces that cut characters in half.
	for i := 0; i < len(raw); i += 7 {
		_, err := out.Write(raw[i:min(i+7, len(raw))])
		require.NoError(t, err)
	}
	out.close()
	emit.mu.Lock()
	defer emit.mu.Unlock()
	var joined strings.Builder
	for _, chunk := range emit.data {
		require.True(t, utf8.ValidString(chunk), "every chunk is valid UTF-8")
		require.LessOrEqual(t, len(chunk), ahpTerminalChunkMax)
		joined.WriteString(chunk)
	}
	require.Equal(t, text, joined.String())
}

func TestAHPTerminalOutputFlushesQuietOutput(t *testing.T) {
	emit := &ahpRecordingEmitter{}
	out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t")
	_, _ = out.Write([]byte("$ "))
	require.Eventually(t, func() bool {
		emit.mu.Lock()
		defer emit.mu.Unlock()
		return len(emit.data) == 1 && emit.data[0] == "$ "
	}, time.Second, 5*time.Millisecond, "a prompt without a newline still reaches the client")
}

func TestAHPValidBranch(t *testing.T) {
	for _, good := range []string{"main", "sozercan-expert-succotash", "orka/session-1a2b", "feature/x.y_z"} {
		require.True(t, ahpValidBranch(good), good)
	}
	for _, bad := range []string{"", "-x", "a..b", "a/../b", ".hidden", "a/.b", "x.lock", "a b", "a;b", "a$(b)", "/abs", "a//b"} {
		require.False(t, ahpValidBranch(bad), bad)
	}
}

func TestAHPTerminalsAreTheirOwnersOnly(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	bob := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "bob")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	term := &ahpTerminal{ctx: ctx, channel: "ahp-terminal:/a", input: make(chan []byte, ahpTerminalInputQueue), cancel: cancel}
	env.backend.mu.Lock()
	env.backend.shells[ahpTerminalKey(alice.UserKey, term.channel)] = term
	env.backend.mu.Unlock()
	input := ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "ls\n"}}
	require.NoError(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
	require.Error(t, env.backend.AuthorizeClientAction(context.Background(), bob, term.channel, input), "another person's terminal")
	env.backend.HandleClientAction(context.Background(), alice, term.channel, input, nil)
	require.Equal(t, "ls\n", string(<-term.input))
	listed, err := env.backend.Terminals(context.Background(), bob)
	require.NoError(t, err)
	require.Empty(t, listed)

	// Without a workspace image there are no terminals.
	_, err = env.backend.CreateTerminal(context.Background(), alice, ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/b"}, nil)
	require.Error(t, err)
}

func TestAHPTerminalOutputPassesMalformedBytesOn(t *testing.T) {
	emit := &ahpRecordingEmitter{}
	out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t")
	garbage := make([]byte, 2*ahpTerminalChunkMax+5)
	for i := range garbage {
		garbage[i] = 0x80 // continuation bytes with no start byte
	}
	_, err := out.Write(garbage)
	require.NoError(t, err)
	out.mu.Lock()
	pending := len(out.pending)
	out.mu.Unlock()
	require.Less(t, pending, ahpTerminalChunkMax, "malformed output does not pile up")
	out.close()
	emit.mu.Lock()
	defer emit.mu.Unlock()
	for _, chunk := range emit.data {
		require.True(t, utf8.ValidString(chunk))
	}
}

func TestAHPTerminalSizeQueueEndsWithTheTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	term := &ahpTerminal{ctx: ctx, sizes: make(chan remotecommand.TerminalSize)}
	done := make(chan *remotecommand.TerminalSize)
	go func() { done <- term.Next() }()
	cancel()
	select {
	case size := <-done:
		require.Nil(t, size)
	case <-time.After(time.Second):
		t.Fatal("the resize worker stayed blocked after the terminal ended")
	}
}

func TestAHPTerminalRefusesInputItCannotQueue(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	term := &ahpTerminal{ctx: ctx, channel: "ahp-terminal:/a", input: make(chan []byte, ahpTerminalInputQueue), cancel: cancel}
	env.backend.mu.Lock()
	env.backend.shells[ahpTerminalKey(alice.UserKey, term.channel)] = term
	env.backend.mu.Unlock()
	input := ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "x"}}
	for range ahpTerminalInputQueue {
		require.NoError(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
		env.backend.HandleClientAction(context.Background(), alice, term.channel, input, nil)
	}
	// The shell reads nothing, so the next input is refused, not dropped.
	require.Error(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
	require.Len(t, term.input, ahpTerminalInputQueue)
}

func TestAHPTerminalURIsAreNotReused(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	term := &ahpTerminal{ctx: ctx, channel: "ahp-terminal:/a", input: make(chan []byte, ahpTerminalInputQueue), cancel: cancel}
	env.backend.mu.Lock()
	env.backend.shells[ahpTerminalKey(alice.UserKey, term.channel)] = term
	env.backend.mu.Unlock()
	input := ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "rm -rf old\n"}}
	require.NoError(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
	require.NoError(t, env.backend.DisposeTerminal(context.Background(), alice, term.channel))
	// Queued input for the ended terminal finds nothing to reach.
	env.backend.HandleClientAction(context.Background(), alice, term.channel, input, nil)
	require.Empty(t, term.input)
	env.backend.mu.Lock()
	_, retired := env.backend.retired[ahpTerminalKey(alice.UserKey, term.channel)]
	env.backend.mu.Unlock()
	require.True(t, retired, "the URI cannot name a new terminal")
}
