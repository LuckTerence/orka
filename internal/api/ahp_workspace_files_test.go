/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"testing"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/ahphost"
)

func TestAHPWorkspacePath(t *testing.T) {
	for uri, want := range map[string]string{
		"file:///workspace/src/a/b/README.md": "/workspace/src/a/b/README.md",
		"file:///workspace":                   "/workspace",
		"file:///workspace/src/../src/x":      "/workspace/src/x",
	} {
		got, ok := ahpWorkspacePath(uri)
		require.True(t, ok, uri)
		require.Equal(t, want, got)
	}
	for _, uri := range []string{"file:///etc/passwd", "file:///workspace/../etc/passwd", "file:///workspacex/a", "https://example.com/a", "file://", "file:///workspace/a\nb"} {
		_, ok := ahpWorkspacePath(uri)
		require.False(t, ok, uri)
	}
}

func TestAHPMatchAny(t *testing.T) {
	require.True(t, ahpMatchAny([]string{".github/app.yml"}, ".github/app.yml"))
	require.True(t, ahpMatchAny([]string{"pkg"}, "pkg/a.go"))
	require.True(t, ahpMatchAny([]string{"*.go"}, "a.go"))
	require.False(t, ahpMatchAny([]string{"*.go"}, "pkg/a.go"))
	require.True(t, ahpMatchAny([]string{"**/*.go"}, "pkg/sub/a.go"))
	require.False(t, ahpMatchAny([]string{"**/*.md"}, "pkg/sub/a.go"))
	require.False(t, ahpMatchAny(nil, "a"))
}

func TestAHPResolveSessionConfig(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	env.setUpSession(c, "manual")
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Namespace: ahpTestNamespace}
	dir := ahptypes.URI(ahpTestDir)
	result, err := env.backend.ResolveSessionConfig(ahpCtx(t), alice, ahptypes.ResolveSessionConfigParams{
		WorkingDirectory: &dir, Config: map[string]json.RawMessage{"approvalMode": json.RawMessage(`"assisted"`), "branch": json.RawMessage(`"feature/x"`)},
	})
	require.NoError(t, err)
	require.JSONEq(t, `"manual"`, string(result.Values["approvalMode"]), "assisted becomes manual: Orka asks the person")
	require.JSONEq(t, `"main"`, string(result.Values["baseBranch"]), "the project's default branch")
	require.JSONEq(t, `"feature/x"`, string(result.Values["branch"]))
	require.Contains(t, result.Schema.Properties, "approvalMode")
	require.Contains(t, result.Schema.Properties, "baseBranch")

	_, err = env.backend.ResolveSessionConfig(ahpCtx(t), alice, ahptypes.ResolveSessionConfigParams{Config: map[string]json.RawMessage{"approvalMode": json.RawMessage(`"yolo"`)}})
	require.Error(t, err)
}
