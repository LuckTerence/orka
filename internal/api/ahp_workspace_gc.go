/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	// ahpWorkspaceActiveAnnotation records a workspace's last use, so an idle
	// workspace is found after a restart too.
	ahpWorkspaceActiveAnnotation = "orka.ai/ahp-workspace-active-at"
	ahpWorkspaceActiveEvery      = 10 * time.Minute
	ahpWorkspaceCollectEvery     = 5 * time.Minute
)

// touch notes that a user used their workspace.
func (w *ahpWorkspaces) touch(principal ahphost.Principal, pod *corev1.Pod) {
	if !w.enabled() || pod == nil {
		return
	}
	now := w.b.now().UTC()
	w.mu.Lock()
	if w.active == nil {
		w.active = map[string]time.Time{}
	}
	label := principal.UserKey[:40]
	last := w.active[label]
	w.active[label] = now
	w.mu.Unlock()
	recorded, err := time.Parse(time.RFC3339, pod.Annotations[ahpWorkspaceActiveAnnotation])
	if now.Sub(last) < ahpWorkspaceActiveEvery && err == nil && now.Sub(recorded) < ahpWorkspaceActiveEvery {
		return
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[ahpWorkspaceActiveAnnotation] = now.Format(time.RFC3339)
	_ = w.b.h.client.Patch(context.Background(), pod, patch)
}

// collectIdle removes idle workspaces until ctx ends.
func (w *ahpWorkspaces) collectIdle(ctx context.Context) {
	if !w.enabled() || w.b.cfg.WorkspaceIdleTimeout <= 0 {
		return
	}
	ticker := time.NewTicker(ahpWorkspaceCollectEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.collectOnce(ctx)
		}
	}
}

func (w *ahpWorkspaces) collectOnce(ctx context.Context) {
	var pods corev1.PodList
	if err := w.reader().List(ctx, &pods, client.InNamespace(w.namespace),
		client.MatchingLabels{"app.kubernetes.io/component": ahpWorkspaceComponent}); err != nil {
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		label := pod.Labels[ahpWorkspaceUserLabel]
		if label == "" || !pod.DeletionTimestamp.IsZero() || w.inUse(label) {
			continue
		}
		w.mu.Lock()
		last := w.active[label]
		w.mu.Unlock()
		if !ahpWorkspaceIdle(w.b.now(), last, pod, w.b.cfg.WorkspaceIdleTimeout) {
			continue
		}
		if pod.Status.Phase == corev1.PodRunning && w.holdsWork(ctx, pod) {
			continue
		}
		_ = w.b.h.client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
	}
}

// inUse reports whether a user has a shell or a turn running.
func (w *ahpWorkspaces) inUse(label string) bool {
	b := w.b
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.shells {
		if strings.HasPrefix(key, label) {
			return true
		}
	}
	for key := range b.turns {
		if strings.HasPrefix(key, label) {
			return true
		}
	}
	for key := range b.reserved {
		if strings.HasPrefix(key, label) {
			return true
		}
	}
	return false
}

// ahpWorkspaceIdle reports whether a workspace's last use, as this process
// saw it or as its annotation or creation records it, is older than idle.
func ahpWorkspaceIdle(now, last time.Time, pod *corev1.Pod, idle time.Duration) bool {
	if recorded, err := time.Parse(time.RFC3339, pod.Annotations[ahpWorkspaceActiveAnnotation]); err == nil && recorded.After(last) {
		last = recorded
	}
	if created := pod.CreationTimestamp.Time; created.After(last) {
		last = created
	}
	return now.Sub(last) >= idle
}

// holdsWork reports whether any checkout in a workspace has changes that
// exist nowhere else: uncommitted files or commits its upstream lacks. When
// it cannot tell, it keeps the workspace.
func (w *ahpWorkspaces) holdsWork(ctx context.Context, pod *corev1.Pod) bool {
	out, err := w.output(ctx, pod, 64<<10, ahpShell, "-c", `for p in "$1"/*/*; do
  [ -d "$p/.git" ] || continue
  git -C "$p" worktree list --porcelain | sed -n 's/^worktree //p' | while IFS= read -r wt; do
    if [ -n "$(git -C "$wt" status --porcelain 2>/dev/null | head -n 1)" ]; then echo "changed $wt"; continue; fi
    if git -C "$wt" rev-parse --abbrev-ref '@{u}' >/dev/null 2>&1; then
      [ "$(git -C "$wt" rev-list --count '@{u}..HEAD')" = 0 ] || echo "ahead $wt"
    fi
  done
done
echo done`, "sh", ahpWorkspaceRoot+"/src")
	if err != nil {
		return true
	}
	text := strings.TrimSpace(string(out))
	return text != "done"
}
