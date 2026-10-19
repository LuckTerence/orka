/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/orka-agents/orka/internal/ahphost"
)

func TestAHPWorkspaceIdle(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Hour))}}
	require.True(t, ahpWorkspaceIdle(now, time.Time{}, pod, 4*time.Hour))
	require.False(t, ahpWorkspaceIdle(now, now.Add(-time.Hour), pod, 4*time.Hour), "this process saw a recent use")
	pod.Annotations = map[string]string{ahpWorkspaceActiveAnnotation: now.Add(-30 * time.Minute).Format(time.RFC3339)}
	require.False(t, ahpWorkspaceIdle(now, time.Time{}, pod, 4*time.Hour), "the annotation survives restarts")
	pod.Annotations = nil
	pod.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
	require.False(t, ahpWorkspaceIdle(now, time.Time{}, pod, 4*time.Hour), "a new workspace is not idle")
}

func TestAHPWorkspaceCollectorKeepsWorkspacesInUse(t *testing.T) {
	env := newAHPTestEnv(t)
	backend := env.backend
	backend.cfg.WorkspaceIdleTimeout = time.Hour
	old := time.Now().Add(-48 * time.Hour)
	backend.now = time.Now
	w := &ahpWorkspaces{b: backend, clientset: kubefake.NewClientset(), rest: &rest.Config{}, namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x")}
	backend.workspaces = w
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	bob := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "bob")}
	for _, p := range []ahphost.Principal{alice, bob} {
		pod := w.podSpec(p)
		pod.CreationTimestamp = metav1.NewTime(old)
		require.NoError(t, env.kube.Create(context.Background(), pod))
	}
	// Bob has a shell open; Alice has nothing running.
	backend.mu.Lock()
	backend.shells[ahpTerminalKey(bob.UserKey, "ahp-terminal:/b")] = &ahpTerminal{}
	backend.mu.Unlock()

	w.collectOnce(context.Background())
	err := env.kube.Get(context.Background(), types.NamespacedName{Namespace: w.namespace, Name: ahpWorkspacePodName(alice.UserKey)}, &corev1.Pod{})
	require.True(t, apierrors.IsNotFound(err), "an idle workspace with nothing running is removed")
	require.NoError(t, env.kube.Get(context.Background(), types.NamespacedName{Namespace: w.namespace, Name: ahpWorkspacePodName(bob.UserKey)}, &corev1.Pod{}),
		"a workspace with a shell stays")
}
