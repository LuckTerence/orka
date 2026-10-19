/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/ahphost"
)

// The helper is shell that git runs; run it here against a token file.
func TestAHPGitCredentialHelper(t *testing.T) {
	file := filepath.Join(t.TempDir(), "github-token")
	helper := strings.Replace(strings.TrimPrefix(ahpGitCredentialHelper, "!"), ahpGitTokenFile, file, 1)
	run := func(action string) string {
		// Git appends the action to a shell helper's command.
		out, err := exec.Command("/bin/sh", "-c", helper+" "+action).Output()
		require.NoError(t, err)
		return string(out)
	}
	require.Empty(t, run("get"), "no token means anonymous access")
	require.NoError(t, os.WriteFile(file, []byte("not-a-real-token"), 0o600))
	require.Equal(t, "username=x-access-token\npassword=not-a-real-token\n", run("get"))
	require.Empty(t, run("store"), "git's store and erase are ignored")
}

func TestAHPWorkspaceAsksGitHubOnlyThroughTheHelper(t *testing.T) {
	w := &ahpWorkspaces{namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x")}
	pod := w.podSpec(ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")})
	env := map[string]string{}
	for _, v := range pod.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	require.Equal(t, "1", env["GIT_CONFIG_COUNT"])
	require.Equal(t, "credential.https://github.com.helper", env["GIT_CONFIG_KEY_0"])
	require.Equal(t, ahpGitCredentialHelper, env["GIT_CONFIG_VALUE_0"])
	require.Equal(t, "0", env["GIT_TERMINAL_PROMPT"])
}
