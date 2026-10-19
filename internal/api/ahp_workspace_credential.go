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
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	// ahpGitTokenFile holds the person's github.com token in their workspace,
	// readable by them only, on the Pod's private /tmp.
	ahpGitTokenDir  = "/tmp/orka-git"
	ahpGitTokenFile = ahpGitTokenDir + "/github-token"
	// ahpGitTokenEvery is how often a workspace's token is replaced; a token
	// stays valid for at least AHPWorkspaceTokenHorizon.
	ahpGitTokenEvery = 20 * time.Minute
	// ahpGitCredentialHelper answers git's "get" with the token file, and
	// nothing when there is none, so git falls back to anonymous access.
	ahpGitCredentialHelper = `!f() { test "$1" = get || exit 0; t=$(cat ` + ahpGitTokenFile +
		` 2>/dev/null) || exit 0; test -n "$t" || exit 0; echo username=x-access-token; echo "password=$t"; }; f`
)

// AHPWorkspaceTokenHorizon is how long a token provided to a workspace stays
// valid at least: past the next replacement, with room for a long fetch.
const AHPWorkspaceTokenHorizon = 45 * time.Minute

// provideCredential puts the person's own github.com token in their
// workspace, from their linked account, at most every ahpGitTokenEvery. A
// person without a usable link gets none, and a token they no longer hold
// is removed.
func (w *ahpWorkspaces) provideCredential(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod) {
	fetch := w.b.cfg.RepositoryCredential
	if fetch == nil || !w.enabled() || w.b.mayUseWorkspace(principal) != nil {
		return
	}
	now := w.b.now()
	w.mu.Lock()
	if w.provided == nil {
		w.provided = map[string]ahpProvidedCredential{}
	}
	last := w.provided[principal.UserKey]
	if last.pod == string(pod.UID) && now.Sub(last.at) < ahpGitTokenEvery {
		w.mu.Unlock()
		return
	}
	w.provided[principal.UserKey] = ahpProvidedCredential{pod: string(pod.UID), at: now}
	w.mu.Unlock()
	token, err := fetch(ctx, principal.Namespace, principal.Issuer, principal.Subject)
	if err != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\n\r") {
		_ = w.run(ctx, pod, "rm", "-f", ahpGitTokenFile)
		if err != nil {
			// Ask again in half the usual time: the person may link their
			// account at any moment.
			w.mu.Lock()
			w.provided[principal.UserKey] = ahpProvidedCredential{pod: string(pod.UID), at: now.Add(-ahpGitTokenEvery / 2)}
			w.mu.Unlock()
		}
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, ahpWorkspaceExecTimeout)
	defer cancel()
	// The token travels on standard input, never in a command line.
	_ = w.exec(writeCtx, pod, []string{ahpShell, "-c",
		`umask 077; mkdir -p "$1"; cat > "$2.next" && mv "$2.next" "$2"`, "sh", ahpGitTokenDir, ahpGitTokenFile},
		remotecommand.StreamOptions{Stdin: strings.NewReader(token), Stdout: &ahpLimitedBuffer{limit: 4 << 10}, Stderr: &ahpLimitedBuffer{limit: 4 << 10}})
}

// ahpProvidedCredential records when a workspace last got its token.
type ahpProvidedCredential struct {
	pod string
	at  time.Time
}
