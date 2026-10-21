/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

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

// A second workspace request that arrives while the person's token is being
// installed waits for that installation, and the token is fetched once.
func TestAHPWorkspaceCredentialInstallsOnceWhileRequestsOverlap(t *testing.T) {
	var fetches atomic.Int32
	b := &ahpBackend{now: time.Now, cfg: AHPBackendConfig{
		RepositoryCredential: func(context.Context, string, string, string) (string, error) {
			fetches.Add(1)
			return "not-a-real-token", nil
		},
	}}
	started, release := make(chan struct{}), make(chan struct{})
	w := &ahpWorkspaces{
		b: b, namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x"),
		rest: &rest.Config{}, clientset: k8sfake.NewClientset(),
		writeToken: func(context.Context, *corev1.Pod, string) error {
			close(started)
			<-release
			return nil
		},
	}
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}}

	first := make(chan struct{})
	go func() { w.provideCredential(context.Background(), principal, pod); close(first) }()
	<-started
	second := make(chan struct{})
	go func() { w.provideCredential(context.Background(), principal, pod); close(second) }()
	select {
	case <-second:
		t.Fatal("a request returned while the token was still being installed")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-first
	<-second
	require.Equal(t, int32(1), fetches.Load(), "the waiting request used the finished installation")

	// A request whose own context ends stops waiting.
	w.writeToken = func(ctx context.Context, _ *corev1.Pod, _ string) error { <-ctx.Done(); return ctx.Err() }
	w.provided = nil
	busy, cancelBusy := context.WithCancel(context.Background())
	defer cancelBusy()
	go w.provideCredential(busy, principal, pod)
	require.Eventually(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.providing[principal.UserKey] != nil },
		time.Second, 10*time.Millisecond)
	waiter, cancelWaiter := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWaiter()
	w.provideCredential(waiter, principal, pod)
	require.ErrorIs(t, waiter.Err(), context.DeadlineExceeded)
}
