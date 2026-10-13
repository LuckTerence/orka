// Package nativesession saves and restores an agent's own conversation state
// between RuntimeSessions. Each supported provider kind has one Format; the
// controller, its store, and the runtime supervisor all use the same Format
// so a provider is added in one place.
package nativesession

import (
	"context"
	"errors"
)

// ErrUnsupported identifies a stopped conversation outside the provider's
// supported format or transport bounds. Callers fall back to the canonical
// transcript. I/O failures and uncertain outcomes are deliberately separate.
var ErrUnsupported = errors.New("native session format or transport is unsupported")

// ErrInstallOutcomeUnknown identifies an installation that may have been
// partly published. Callers retain the destination for reconciliation.
var ErrInstallOutcomeUnknown = errors.New("native session installation outcome is unknown")

// Dirs are the private directories of one agent session.
type Dirs struct {
	// Home is the session's HOME directory.
	Home string
	// Workspace is the agent's working directory.
	Workspace string
}

// Summary describes a verified saved conversation.
type Summary struct {
	ProviderSessionID string
	ProviderVersion   string
	DataDigest        string
}

// Format reads, verifies, and installs one provider's saved conversation.
// Capture and Install require that no agent process can write the
// directories: Capture runs after the writer is proven stopped, and Install
// before the agent starts.
type Format interface {
	// Capture reads the conversation providerSessionID from dirs.
	Capture(ctx context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error)
	// Inspect verifies data without starting an agent.
	Inspect(ctx context.Context, data []byte, maxBytes int) (Summary, error)
	// Install writes data into dirs. journalDir is private, outside dirs, and
	// lets a retry reconcile an earlier attempt for the same destination.
	Install(ctx context.Context, data []byte, dirs Dirs, journalDir string, maxBytes int) error
}

var formats = map[string]Format{
	codexProviderKind: codexFormat{},
}

// For returns the Format for providerKind.
func For(providerKind string) (Format, bool) {
	format, ok := formats[providerKind]
	return format, ok
}

// Supported reports whether providerKind can save and restore conversations.
func Supported(providerKind string) bool {
	_, ok := formats[providerKind]
	return ok
}
