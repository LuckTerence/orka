/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	corev1 "k8s.io/api/core/v1"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	// ahpFileReadMax bounds a file read from a workspace.
	ahpFileReadMax = 1 << 20
	// ahpListMax bounds a directory listing, and ahpWatchFilesMax the files a
	// watch follows.
	ahpListMax       = 5000
	ahpWatchFilesMax = 5000
	ahpWatchInterval = 5 * time.Second
	ahpEntryFile     = "file"
	ahpEntryDir      = "directory"
)

// errAHPOutputTooLarge reports output past a read's bound.
var errAHPOutputTooLarge = errors.New("the output is too large")

// ahpWorkspacePath turns a file URI into a clean path inside the workspace.
func ahpWorkspacePath(uri string) (string, bool) {
	raw, ok := strings.CutPrefix(uri, "file://")
	if !ok || raw == "" || strings.ContainsAny(raw, "\x00\n") {
		return "", false
	}
	clean := path.Clean(raw)
	if clean != ahpWorkspaceRoot && !strings.HasPrefix(clean, ahpWorkspaceRoot+"/") {
		return "", false
	}
	return clean, true
}

// output runs a program in the workspace and returns its standard output, up
// to limit bytes.
func (w *ahpWorkspaces) output(ctx context.Context, pod *corev1.Pod, limit int, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ahpWorkspaceExecTimeout)
	defer cancel()
	out := &ahpLimitedBuffer{limit: limit}
	if err := w.streamOutput(ctx, pod, argv, out); err != nil {
		if out.over {
			return nil, errAHPOutputTooLarge
		}
		return nil, err
	}
	if out.over {
		return nil, errAHPOutputTooLarge
	}
	return out.buf.Bytes(), nil
}

// readFile reads a regular file from the workspace.
func (w *ahpWorkspaces) readFile(ctx context.Context, pod *corev1.Pod, file string) ([]byte, error) {
	return w.output(ctx, pod, ahpFileReadMax, "/bin/sh", "-c", `[ -f "$1" ] || exit 3; exec cat -- "$1"`, "sh", file)
}

// list lists a directory in the workspace, directories marked with a slash.
func (w *ahpWorkspaces) list(ctx context.Context, pod *corev1.Pod, dir string) ([]ahptypes.DirectoryEntry, error) {
	raw, err := w.output(ctx, pod, 1<<20, "/bin/sh", "-c", `[ -d "$1" ] || exit 3; cd "$1" && ls -1Ap`, "sh", dir)
	if err != nil {
		return nil, err
	}
	var entries []ahptypes.DirectoryEntry
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" || len(entries) >= ahpListMax {
			continue
		}
		if name, ok := strings.CutSuffix(line, "/"); ok {
			entries = append(entries, ahptypes.DirectoryEntry{Name: name, Type: ahpEntryDir})
		} else {
			entries = append(entries, ahptypes.DirectoryEntry{Name: line, Type: ahpEntryFile})
		}
	}
	return entries, nil
}

// stamps reports each file's modification time and size under root, below
// .git directories excluded, at most ahpWatchFilesMax of them.
func (w *ahpWorkspaces) stamps(ctx context.Context, pod *corev1.Pod, root string, recursive bool) (map[string]string, error) {
	depth := "1"
	if recursive {
		depth = "64"
	}
	raw, err := w.output(ctx, pod, 4<<20, "/bin/sh", "-c",
		`[ -e "$1" ] || exit 3; find "$1" -maxdepth "$2" -name .git -prune -o -type f -print | head -n "$3" | while IFS= read -r f; do stat -c '%Y %s %n' -- "$f"; done`,
		"sh", root, depth, strconv.Itoa(ahpWatchFilesMax))
	if err != nil {
		return nil, err
	}
	stamps := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 64<<10)
	for scanner.Scan() {
		mtime, rest, ok := strings.Cut(scanner.Text(), " ")
		if !ok {
			continue
		}
		size, name, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		stamps[name] = mtime + " " + size
	}
	return stamps, nil
}

// ahpLimitedBuffer keeps at most limit bytes and notes when more arrived.
type ahpLimitedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (b *ahpLimitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		b.over = true
		return 0, errAHPOutputTooLarge
	}
	b.buf.Write(p)
	return len(p), nil
}

// workspaceFor returns the user's running workspace when they may use it.
// Reads never start a workspace.
func (b *ahpBackend) workspaceFor(ctx context.Context, principal ahphost.Principal) (*corev1.Pod, bool) {
	if !b.workspaces.enabled() || b.mayUseWorkspace(principal) != nil {
		return nil, false
	}
	return b.workspaces.running(ctx, principal)
}

// readWorkspaceFile serves a file from the user's workspace.
func (b *ahpBackend) readWorkspaceFile(ctx context.Context, principal ahphost.Principal, uri string) (ahptypes.ResourceReadResult, error) {
	file, ok := ahpWorkspacePath(uri)
	if !ok {
		return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
	}
	pod, ok := b.workspaceFor(ctx, principal)
	if !ok {
		return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
	}
	data, err := b.workspaces.readFile(ctx, pod, file)
	if errors.Is(err, errAHPOutputTooLarge) {
		return ahptypes.ResourceReadResult{}, &ahphost.Rejection{Reason: fmt.Sprintf("the file is larger than %d bytes", ahpFileReadMax)}
	}
	if err != nil {
		return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
	}
	if utf8.Valid(data) {
		return ahptypes.ResourceReadResult{Data: string(data), Encoding: ahptypes.ContentEncodingUtf8}, nil
	}
	return ahptypes.ResourceReadResult{Data: base64.StdEncoding.EncodeToString(data), Encoding: ahptypes.ContentEncodingBase64}, nil
}

// ListResource lists a directory in the user's workspace.
func (b *ahpBackend) ListResource(ctx context.Context, principal ahphost.Principal, uri string) ([]ahptypes.DirectoryEntry, error) {
	dir, ok := ahpWorkspacePath(uri)
	if !ok {
		return nil, ahphost.ErrResourceNotFound
	}
	pod, ok := b.workspaceFor(ctx, principal)
	if !ok {
		return nil, ahphost.ErrResourceNotFound
	}
	entries, err := b.workspaces.list(ctx, pod, dir)
	if err != nil {
		return nil, ahphost.ErrResourceNotFound
	}
	return entries, nil
}

// WatchResources polls the user's workspace for changes under a file or
// directory, filtered by the watch's include and exclude patterns.
func (b *ahpBackend) WatchResources(ctx context.Context, principal ahphost.Principal, params ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange)) error {
	root, ok := ahpWorkspacePath(params.Uri)
	if !ok || !b.workspaces.enabled() || b.mayUseWorkspace(principal) != nil {
		return ahphost.ErrResourceNotFound
	}
	includes, excludes := ahpWatchPatterns(params.Includes), ahpWatchPatterns(params.Excludes)
	recursive := params.Recursive != nil && *params.Recursive
	matches := func(file string) bool {
		rel := strings.TrimPrefix(strings.TrimPrefix(file, root), "/")
		if len(includes) > 0 && !ahpMatchAny(includes, rel) {
			return false
		}
		return !ahpMatchAny(excludes, rel)
	}
	poll := func() map[string]string {
		pod, ok := b.workspaces.running(ctx, principal)
		if !ok {
			return nil
		}
		stamps, err := b.workspaces.stamps(ctx, pod, root, recursive)
		if err != nil {
			return nil
		}
		for file := range stamps {
			if !matches(file) {
				delete(stamps, file)
			}
		}
		return stamps
	}
	last := poll()
	changed(nil)
	ticker := time.NewTicker(ahpWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		current := poll()
		if current == nil {
			// The workspace is not running; nothing can change.
			continue
		}
		var changes []ahptypes.ResourceChange
		for file, stamp := range current {
			previous, existed := last[file]
			switch {
			case !existed:
				changes = append(changes, ahptypes.ResourceChange{Uri: ahpDirectoryURI(file), Type: ahptypes.ResourceChangeTypeAdded})
			case previous != stamp:
				changes = append(changes, ahptypes.ResourceChange{Uri: ahpDirectoryURI(file), Type: ahptypes.ResourceChangeTypeUpdated})
			}
		}
		for file := range last {
			if _, still := current[file]; !still {
				changes = append(changes, ahptypes.ResourceChange{Uri: ahpDirectoryURI(file), Type: ahptypes.ResourceChangeTypeDeleted})
			}
		}
		last = current
		changed(changes)
	}
}

// ahpWatchPatterns reads a watch's { items } pattern list.
func ahpWatchPatterns(raw *json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var wrapped struct {
		Items []string `json:"items"`
	}
	if json.Unmarshal(*raw, &wrapped) != nil {
		return nil
	}
	return wrapped.Items
}

// ahpMatchAny matches a relative path against glob patterns. A pattern
// starting with **/ also matches at any depth, and a bare path matches the
// path and anything under it.
func ahpMatchAny(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimPrefix(pattern, "./")
		if pattern == rel || strings.HasPrefix(rel, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
		if ok, _ := path.Match(pattern, rel); ok {
			return true
		}
		if tail, ok := strings.CutPrefix(pattern, "**/"); ok {
			for suffix := rel; suffix != ""; {
				if ok, _ := path.Match(tail, suffix); ok {
					return true
				}
				_, next, found := strings.Cut(suffix, "/")
				if !found {
					break
				}
				suffix = next
			}
		}
	}
	return false
}
