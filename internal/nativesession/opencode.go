package nativesession

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	_ "embed"

	// Registers the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"
)

const (
	openCodeProviderKind = "opencode"
	// openCodeVersion is the OpenCode release whose database this format
	// carries. It must equal acp.OpenCodeVersion.
	openCodeVersion      = "1.19.0"
	openCodeBundleFormat = "orka/opencode-session-v1"

	openCodeManifestEntry   = "manifest.json"
	openCodeDatabaseEntry   = "opencode.db"
	openCodeToolOutputEntry = "tool-output/"
	openCodeToolOutputDir   = "tool-output"
	maxOpenCodeToolOutputs  = 256

	openCodeMessageTable = "message"
	openCodePartTable    = "part"
)

var (
	openCodeSessionIDPattern  = regexp.MustCompile(`^ses_[A-Za-z0-9]{1,64}$`)
	openCodeToolOutputPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

// openCodeSchemaSQL is the schema of a database OpenCode 1.19.0 created. A
// captured database is agent-written, so it must match this schema exactly
// before anything that evaluates its expressions runs, in the supervisor or
// in the controller.
//
//go:embed opencode_schema.sql
var openCodeSchemaSQL string

type openCodeSchema struct {
	entries []string
	tables  []string
}

// expectedOpenCodeSchema returns the pinned schema's sqlite_schema entries.
var expectedOpenCodeSchema = sync.OnceValues(func() (openCodeSchema, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return openCodeSchema{}, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(openCodeSchemaSQL); err != nil {
		return openCodeSchema{}, fmt.Errorf("load the pinned OpenCode schema: %w", err)
	}
	return readOpenCodeSchema(context.Background(), db)
})

// readOpenCodeSchema reads every sqlite_schema entry, SQLite's own included,
// since a crafted file can give any object a reserved name. Reading it
// evaluates nothing the schema defines.
func readOpenCodeSchema(ctx context.Context, db *sql.DB) (openCodeSchema, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		return openCodeSchema{}, err
	}
	defer func() { _ = rows.Close() }()
	var schema openCodeSchema
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			return openCodeSchema{}, err
		}
		schema.entries = append(schema.entries, kind+"\x00"+name+"\x00"+table+"\x00"+statement)
		if kind == "table" {
			schema.tables = append(schema.tables, name)
		}
	}
	return schema, rows.Err()
}

// requireOpenCodeSchema returns the tables of db, whose schema must equal
// the pinned OpenCode schema.
func requireOpenCodeSchema(ctx context.Context, db *sql.DB) ([]string, error) {
	expected, err := expectedOpenCodeSchema()
	if err != nil {
		return nil, err
	}
	actual, err := readOpenCodeSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(actual.entries, expected.entries) {
		return nil, fmt.Errorf("%w: OpenCode database schema differs from OpenCode %s", ErrUnsupported, openCodeVersion)
	}
	return expected.tables, nil
}

// openCodeCarriedTables hold the conversation. Capture keeps only the
// session's rows; the migration tables tell OpenCode its schema is current.
var openCodeCarriedTables = []string{"project", "session", openCodeMessageTable, openCodePartTable, "migration", "data_migration"}

// openCodeDroppedTables are not needed to resume, or must not cross turns:
// the duplicate update log, the directory index, persisted approvals,
// credentials, and sharing state. Rows in any other table make a capture
// unsupported, so state a newer OpenCode adds is never silently dropped.
var openCodeDroppedTables = []string{
	"event", "event_sequence", "project_directory", "permission",
	"account", "account_state", "control_account", "credential", "session_share",
}

// openCodeManifest describes a captured OpenCode conversation.
type openCodeManifest struct {
	Format    string `json:"format"`
	Version   string `json:"version"`
	SessionID string `json:"sessionID"`
	// Workspace is the captured RuntimeSession's working directory, which
	// OpenCode recorded as the session's directory.
	Workspace string `json:"workspace"`
	// ToolOutputDir is where the captured session spilled truncated tool
	// output. Parts reference those files by absolute path.
	ToolOutputDir string `json:"toolOutputDir"`
}

type openCodeBundle struct {
	manifest   openCodeManifest
	database   []byte
	toolOutput map[string][]byte
}

// openCodeFormat carries OpenCode's SQLite conversation: a consistent copy of
// opencode.db holding only the session's rows, plus spilled tool output.
type openCodeFormat struct{}

func openCodeRoot(dirs Dirs) string { return filepath.Join(dirs.Data, "opencode") }

func (openCodeFormat) Capture(ctx context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return nil, err
	}
	if !openCodeSessionIDPattern.MatchString(providerSessionID) {
		return nil, errors.New("OpenCode session ID is invalid")
	}
	root := openCodeRoot(dirs)
	source := filepath.Join(root, openCodeDatabaseEntry)
	if err := requireRealPath(source, false); err != nil {
		return nil, err
	}
	// The live database also holds the duplicate event log, so it may be
	// larger than what Capture keeps, but not unboundedly so.
	stored := int64(0)
	for _, name := range []string{source, source + "-wal", source + "-shm", source + "-journal"} {
		if err := requireRealPathIfPresent(name, false); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(name); err == nil {
			stored += info.Size()
		}
	}
	if stored > 2*int64(expandedLimit(limit)) {
		return nil, fmt.Errorf("%w: OpenCode database exceeds the capture budget", ErrUnsupported)
	}
	toolOutput, err := readOpenCodeToolOutput(filepath.Join(root, openCodeToolOutputDir), limit)
	if err != nil {
		return nil, err
	}
	temp, err := privateTempDir()
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	copied := filepath.Join(temp, openCodeDatabaseEntry)
	if err := copyOpenCodeDatabase(ctx, source, copied); err != nil {
		return nil, err
	}
	if err := pruneOpenCodeDatabase(ctx, copied, providerSessionID, dirs.Workspace); err != nil {
		return nil, err
	}
	toolOutputBytes := 0
	for _, content := range toolOutput {
		toolOutputBytes += len(content)
	}
	database, err := readBoundedFile(copied, max(0, expandedLimit(limit)-toolOutputBytes))
	if err != nil {
		return nil, err
	}
	data, err := encodeOpenCodeBundle(limit, openCodeBundle{
		manifest: openCodeManifest{
			Format: openCodeBundleFormat, Version: openCodeVersion, SessionID: providerSessionID,
			Workspace: dirs.Workspace, ToolOutputDir: filepath.Join(root, openCodeToolOutputDir),
		},
		database: database, toolOutput: toolOutput,
	})
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: OpenCode conversation exceeds Orka's %d-byte transport limit", ErrUnsupported, limit)
	}
	return data, nil
}

func (openCodeFormat) Inspect(ctx context.Context, data []byte, maxBytes int) (Summary, error) {
	bundle, err := decodeOpenCodeBundle(data, maxBytes)
	if err != nil {
		return Summary{}, err
	}
	temp, err := privateTempDir()
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	database := filepath.Join(temp, openCodeDatabaseEntry)
	if err := writeNewFile(database, bundle.database); err != nil {
		return Summary{}, err
	}
	if err := verifyOpenCodeDatabase(ctx, database, bundle.manifest); err != nil {
		return Summary{}, err
	}
	return Summary{ProviderSessionID: bundle.manifest.SessionID, ProviderVersion: bundle.manifest.Version, DataDigest: dataDigest(data)}, nil
}

// Install writes the conversation into a fresh session's XDG data directory
// and points it at the new working directory, because OpenCode resumes in the
// session's recorded directory rather than the cwd it is given. Every file is
// created new, so a failed install leaves nothing a retry could mistake for
// its own; journalDir is unused.
func (openCodeFormat) Install(ctx context.Context, data []byte, dirs Dirs, _ string, maxBytes int) error {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return err
	}
	bundle, err := decodeOpenCodeBundle(data, limit)
	if err != nil {
		return err
	}
	for _, dir := range []string{dirs.Data, dirs.Workspace} {
		if err := requireRealPath(dir, true); err != nil {
			return err
		}
	}
	root := openCodeRoot(dirs)
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := requireRealPath(root, true); err != nil {
		return err
	}
	destination := filepath.Join(root, openCodeDatabaseEntry)
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("OpenCode session home already holds a database")
	}
	toolOutputDir := filepath.Join(root, openCodeToolOutputDir)
	if len(bundle.toolOutput) > 0 {
		if err := os.Mkdir(toolOutputDir, 0o700); err != nil {
			return err
		}
		for name, content := range bundle.toolOutput {
			if err := writeNewFile(filepath.Join(toolOutputDir, name), content); err != nil {
				return err
			}
		}
	}
	staged := filepath.Join(root, ".opencode.db.restore")
	if err := writeNewFile(staged, bundle.database); err != nil {
		return err
	}
	if err := verifyOpenCodeDatabase(ctx, staged, bundle.manifest); err != nil {
		return err
	}
	// Relocation may grow the conversation only into the budget the carried
	// state leaves.
	carried := len(bundle.database)
	for _, content := range bundle.toolOutput {
		carried += len(content)
	}
	if err := relocateOpenCodeSession(ctx, staged, bundle.manifest, dirs.Workspace, toolOutputDir, expandedLimit(limit)-carried); err != nil {
		return err
	}
	if err := os.Rename(staged, destination); err != nil {
		return err
	}
	return syncDir(root)
}

// copyOpenCodeDatabase takes a consistent copy of a stopped OpenCode
// database. Committed rows still in the write-ahead log are included; a plain
// file copy would lose them.
func copyOpenCodeDatabase(ctx context.Context, source, target string) error {
	db, err := openSQLite(source, "mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	// A source Orka cannot read or verify will not become readable on retry,
	// so it falls back to the transcript. A structurally sound database holds
	// no page twice, so its copy is no larger than the bounded source; the
	// check reads each page once.
	if _, err := requireOpenCodeSchema(ctx, db); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return err
		}
		return fmt.Errorf("%w: OpenCode database is unreadable or corrupt: %w", ErrUnsupported, err)
	}
	if err := requireIntactSQLite(ctx, db); err != nil {
		return fmt.Errorf("%w: OpenCode database is unreadable or corrupt: %w", ErrUnsupported, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		return fmt.Errorf("copy OpenCode database: %w", err)
	}
	return nil
}

// pruneOpenCodeDatabase leaves only providerSessionID's conversation in a
// private copy of the database.
func pruneOpenCodeDatabase(ctx context.Context, path, providerSessionID, workspace string) error {
	db, err := openSQLite(path, "")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tables, err := requireOpenCodeSchema(ctx, db)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		return err
	}
	for _, table := range tables {
		switch {
		case slices.Contains(openCodeCarriedTables, table):
		case slices.Contains(openCodeDroppedTables, table):
			if _, err := db.ExecContext(ctx, `DELETE FROM "`+table+`"`); err != nil {
				return err
			}
		default:
			populated, err := sqliteExists(ctx, db, `SELECT EXISTS(SELECT 1 FROM "`+table+`")`)
			if err != nil {
				return err
			}
			if populated {
				return fmt.Errorf("%w: OpenCode table %s holds state Orka does not carry", ErrUnsupported, table)
			}
		}
	}
	session, err := readOpenCodeSession(ctx, db, providerSessionID)
	if err != nil {
		return err
	}
	if session.directory != workspace {
		return fmt.Errorf("%w: OpenCode session runs outside its RuntimeSession workspace", ErrUnsupported)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM project WHERE id != ?`, session.projectID); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	return nil
}

// verifyOpenCodeDatabase checks that path is an intact OpenCode database
// holding exactly the manifest's conversation and nothing else.
func verifyOpenCodeDatabase(ctx context.Context, path string, manifest openCodeManifest) error {
	db, err := openSQLite(path, "mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tables, err := requireOpenCodeSchema(ctx, db)
	if errors.Is(err, ErrUnsupported) {
		// Capture checked the same schema, so this bundle is invalid.
		return errors.New("OpenCode database schema differs from the captured version")
	}
	if err != nil {
		return err
	}
	if err := requireIntactSQLite(ctx, db); err != nil {
		return err
	}
	for _, table := range tables {
		if slices.Contains(openCodeCarriedTables, table) {
			continue
		}
		populated, err := sqliteExists(ctx, db, `SELECT EXISTS(SELECT 1 FROM "`+table+`")`)
		if err != nil {
			return err
		}
		if populated {
			return fmt.Errorf("OpenCode database carries rows in %s", table)
		}
	}
	session, err := readOpenCodeSession(ctx, db, manifest.SessionID)
	if errors.Is(err, ErrUnsupported) {
		// Capture never produces this, so the bundle is invalid, not merely unsupported.
		return errors.New("OpenCode database carries state outside the captured session")
	}
	if err != nil {
		return err
	}
	if session.directory != manifest.Workspace {
		return errors.New("OpenCode session directory differs from the captured workspace")
	}
	foreign, err := sqliteExists(ctx, db, `SELECT EXISTS(SELECT 1 FROM project WHERE id != ?)`, session.projectID)
	if err != nil || foreign {
		return errors.New("OpenCode database carries another project")
	}
	return nil
}

// openCodePermissionOnlyDenies reports whether a session's permission rules
// can only deny. OpenCode evaluates the last rule that matches across the
// agent's rules and then the session's, so a deny rule only narrows what the
// runtime's configuration allows; `opencode run` writes such rules for its
// interactive tools. An allow or ask rule could widen it, so it is not carried.
// Keys are matched exactly, as OpenCode reads them.
func openCodePermissionOnlyDenies(permission sql.NullString) bool {
	if !permission.Valid {
		return true
	}
	var rules []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(permission.String), &rules); err != nil {
		return false
	}
	for _, rule := range rules {
		if len(rule) != 3 {
			return false
		}
		for _, key := range []string{"permission", "pattern", "action"} {
			var value *string
			if json.Unmarshal(rule[key], &value) != nil || value == nil {
				return false
			}
			if key == "action" && *value != "deny" {
				return false
			}
		}
	}
	return true
}

type openCodeSession struct {
	projectID, directory, path string
}

// readOpenCodeSession requires the database to hold exactly one top-level
// session, providerSessionID, whose messages and parts are its own.
func readOpenCodeSession(ctx context.Context, db *sql.DB, providerSessionID string) (openCodeSession, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, project_id, directory, COALESCE(path, ''), parent_id IS NULL AND workspace_id IS NULL,
 permission FROM session`)
	if err != nil {
		return openCodeSession{}, err
	}
	defer func() { _ = rows.Close() }()
	var session openCodeSession
	found, count, restricted := false, 0, false
	for rows.Next() {
		var current openCodeSession
		var id string
		var topLevel bool
		var permission sql.NullString
		if err := rows.Scan(&id, &current.projectID, &current.directory, &current.path, &topLevel, &permission); err != nil {
			return openCodeSession{}, err
		}
		count++
		if id == providerSessionID {
			found, session, restricted = true, current, !topLevel || !openCodePermissionOnlyDenies(permission)
		}
	}
	if err := rows.Err(); err != nil {
		return openCodeSession{}, err
	}
	switch {
	case !found:
		return openCodeSession{}, errors.New("OpenCode database does not hold the session")
	case count != 1:
		return openCodeSession{}, fmt.Errorf("%w: OpenCode database holds more than the captured session", ErrUnsupported)
	case restricted:
		return openCodeSession{}, fmt.Errorf("%w: OpenCode session carries child, workspace, or permission state", ErrUnsupported)
	}
	for _, query := range []string{
		`SELECT EXISTS(SELECT 1 FROM message WHERE session_id != ?)`,
		`SELECT EXISTS(SELECT 1 FROM part WHERE session_id != ? OR message_id NOT IN (SELECT id FROM message))`,
	} {
		foreign, err := sqliteExists(ctx, db, query, providerSessionID)
		if err != nil {
			return openCodeSession{}, err
		}
		if foreign {
			return openCodeSession{}, errors.New("OpenCode database holds messages of another session")
		}
	}
	return session, nil
}

// relocateOpenCodeSession points the restored session at workspace. OpenCode
// runs tools in session.directory and ignores the cwd passed to
// session/resume, and the previous turn's directories no longer exist.
// Every string it replaces is a path Orka chose, and the growth the
// replacements add stays within budget.
func relocateOpenCodeSession(ctx context.Context, path string, manifest openCodeManifest, workspace, toolOutputDir string, budget int) error {
	db, err := openSQLite(path, "")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var projectID, worktree string
	var sessionPath sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT s.project_id, s.path, p.worktree
 FROM session s JOIN project p ON p.id = s.project_id WHERE s.id = ?`, manifest.SessionID).
		Scan(&projectID, &sessionPath, &worktree); err != nil {
		return fmt.Errorf("read OpenCode session location: %w", err)
	}
	// verifyOpenCodeDatabase proved session.directory is the captured workspace.
	directory := manifest.Workspace
	// The global project's worktree is "/", and OpenCode titles tools with
	// the worktree-relative path, which is then the workspace without its
	// leading slash. Other worktree-relative forms are left alone.
	previousPath := ""
	if worktree == "/" && sessionPath.Valid && sessionPath.String == strings.TrimPrefix(directory, "/") {
		previousPath = sessionPath.String
	}
	// A git project's worktree is the workspace itself; the global project's
	// is the filesystem root and stays.
	newWorktree := worktree
	if worktree == directory {
		newWorktree = workspace
	}
	if sessionPath.Valid {
		relative, err := filepath.Rel(newWorktree, workspace)
		if err != nil {
			return fmt.Errorf("relocate OpenCode session path: %w", err)
		}
		// OpenCode records path.relative(worktree, directory), which is empty
		// rather than "." for the worktree itself.
		if relative == "." {
			relative = ""
		}
		sessionPath.String = filepath.ToSlash(relative)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE project SET worktree = ? WHERE id = ?`, []any{newWorktree, projectID}},
		{`UPDATE session SET directory = ?, path = ? WHERE id = ?`, []any{workspace, sessionPath, manifest.SessionID}},
	}
	// Absolute paths move first, so the worktree-relative form OpenCode uses
	// in tool titles only matches where no absolute path remains.
	growth := int64(0)
	for _, move := range [][2]string{{directory, workspace}, {manifest.ToolOutputDir, toolOutputDir}, {previousPath, sessionPath.String}} {
		if move[0] == "" || move[0] == move[1] {
			continue
		}
		for _, table := range []string{openCodeMessageTable, openCodePartTable} {
			if len(move[1]) > len(move[0]) {
				var occurrences int64
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum((length(CAST(data AS BLOB)) - length(CAST(replace(data, ?, '') AS BLOB))) / ?), 0)
 FROM `+table+` WHERE instr(data, ?) > 0`, move[0], len(move[0]), move[0]).Scan(&occurrences); err != nil {
					return err
				}
				growth += occurrences * int64(len(move[1])-len(move[0]))
			}
			statements = append(statements, struct {
				query string
				args  []any
			}{`UPDATE ` + table + ` SET data = replace(data, ?, ?) WHERE instr(data, ?) > 0`, []any{move[0], move[1], move[0]}})
		}
	}
	if growth > int64(budget) {
		return errors.New("relocating the OpenCode session would exceed its expanded budget")
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("relocate OpenCode session: %w", err)
		}
	}
	return tx.Commit()
}

func requireIntactSQLite(ctx context.Context, db *sql.DB) error {
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&check); err != nil || check != "ok" {
		return errors.New("OpenCode database failed its integrity check")
	}
	return nil
}

func sqliteExists(ctx context.Context, db *sql.DB, query string, args ...any) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, query, args...).Scan(&exists)
	return exists, err
}

// openSQLite opens path without trusting functions its schema names.
func openSQLite(path, query string) (*sql.DB, error) {
	if strings.ContainsAny(path, "?#%") {
		return nil, errors.New("SQLite path contains URI metacharacters")
	}
	parameters := "_pragma=trusted_schema(0)&_pragma=busy_timeout(250)"
	if query != "" {
		parameters = query + "&" + parameters
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+parameters)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func readOpenCodeToolOutput(dir string, limit int) (map[string][]byte, error) {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := requireRealPath(dir, true); err != nil {
		return nil, err
	}
	handle, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	entries, err := handle.ReadDir(maxOpenCodeToolOutputs + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxOpenCodeToolOutputs {
		return nil, fmt.Errorf("%w: OpenCode spilled more than %d tool outputs", ErrUnsupported, maxOpenCodeToolOutputs)
	}
	files := make(map[string][]byte, len(entries))
	total := 0
	for _, entry := range entries {
		name := entry.Name()
		if !openCodeToolOutputPattern.MatchString(name) {
			return nil, fmt.Errorf("%w: OpenCode tool output has an unexpected name", ErrUnsupported)
		}
		path := filepath.Join(dir, name)
		if err := requireRealPath(path, false); err != nil {
			return nil, err
		}
		content, err := readBoundedFile(path, expandedLimit(limit)-total)
		if err != nil {
			return nil, err
		}
		total += len(content)
		files[name] = content
	}
	return files, nil
}

// encodeOpenCodeBundle fails with ErrUnsupported when the archive exceeds
// the expanded budget decodeOpenCodeBundle enforces.
func encodeOpenCodeBundle(limit int, bundle openCodeBundle) ([]byte, error) {
	manifest, err := json.Marshal(bundle.manifest)
	if err != nil {
		return nil, err
	}
	var tarred bytes.Buffer
	archive := tar.NewWriter(&tarred)
	write := func(name string, content []byte) error {
		header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg, Format: tar.FormatPAX, ModTime: time.Unix(0, 0)}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		_, err := archive.Write(content)
		return err
	}
	if err := write(openCodeManifestEntry, manifest); err != nil {
		return nil, err
	}
	if err := write(openCodeDatabaseEntry, bundle.database); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(bundle.toolOutput))
	for name := range bundle.toolOutput {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := write(openCodeToolOutputEntry+name, bundle.toolOutput[name]); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if tarred.Len() > expandedLimit(limit) {
		return nil, fmt.Errorf("%w: OpenCode conversation exceeds the expanded budget", ErrUnsupported)
	}
	var out bytes.Buffer
	compressed := gzip.NewWriter(&out)
	if _, err := compressed.Write(tarred.Bytes()); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// decodeOpenCodeBundle reads a bundle strictly: one gzip member holding a tar
// of regular files with known names, each at most once, within the limits.
func decodeOpenCodeBundle(data []byte, maxBytes int) (openCodeBundle, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return openCodeBundle{}, err
	}
	if len(data) == 0 || len(data) > limit {
		return openCodeBundle{}, errors.New("OpenCode bundle exceeds its size bounds")
	}
	source := bytes.NewReader(data)
	compressed, err := gzip.NewReader(source)
	if err != nil {
		return openCodeBundle{}, errors.New("OpenCode bundle is not gzip data")
	}
	compressed.Multistream(false)
	manifest, bundle, err := readOpenCodeArchive(compressed, int64(expandedLimit(limit)))
	if err != nil {
		return openCodeBundle{}, err
	}
	if trailing, err := io.Copy(io.Discard, io.LimitReader(compressed, 1)); err != nil || trailing != 0 || source.Len() != 0 {
		return openCodeBundle{}, errors.New("OpenCode bundle has trailing data")
	}
	if manifest == nil || len(bundle.database) == 0 {
		return openCodeBundle{}, errors.New("OpenCode bundle is incomplete")
	}
	if bundle.manifest, err = parseOpenCodeManifest(manifest); err != nil {
		return openCodeBundle{}, err
	}
	return bundle, nil
}

// readOpenCodeArchive reads tar entries until the archive ends, within budget
// bytes of content.
func readOpenCodeArchive(compressed io.Reader, budget int64) ([]byte, openCodeBundle, error) {
	archive := tar.NewReader(io.LimitReader(compressed, budget+1))
	bundle := openCodeBundle{toolOutput: map[string][]byte{}}
	var manifest []byte
	seen := map[string]bool{}
	var read int64
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return manifest, bundle, nil
		}
		if err != nil {
			return nil, openCodeBundle{}, errors.New("OpenCode bundle archive is invalid")
		}
		if header.Typeflag != tar.TypeReg || seen[header.Name] || header.Size < 0 || read+header.Size > budget {
			return nil, openCodeBundle{}, errors.New("OpenCode bundle entry is invalid")
		}
		seen[header.Name] = true
		read += header.Size
		content, err := io.ReadAll(io.LimitReader(archive, header.Size))
		if err != nil || int64(len(content)) != header.Size {
			return nil, openCodeBundle{}, errors.New("OpenCode bundle entry is truncated")
		}
		toolOutput, isToolOutput := strings.CutPrefix(header.Name, openCodeToolOutputEntry)
		switch {
		case header.Name == openCodeManifestEntry:
			manifest = content
		case header.Name == openCodeDatabaseEntry:
			bundle.database = content
		case isToolOutput && openCodeToolOutputPattern.MatchString(toolOutput) && len(bundle.toolOutput) < maxOpenCodeToolOutputs:
			bundle.toolOutput[toolOutput] = content
		default:
			return nil, openCodeBundle{}, errors.New("OpenCode bundle has an unknown or excess entry")
		}
	}
}

func parseOpenCodeManifest(content []byte) (openCodeManifest, error) {
	var manifest openCodeManifest
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || decoder.More() {
		return openCodeManifest{}, errors.New("OpenCode bundle manifest is invalid")
	}
	if manifest.Format != openCodeBundleFormat || !openCodeSessionIDPattern.MatchString(manifest.SessionID) ||
		!absoluteCleanPath(manifest.Workspace) || !absoluteCleanPath(manifest.ToolOutputDir) {
		return openCodeManifest{}, errors.New("OpenCode bundle manifest is invalid")
	}
	if manifest.Version != openCodeVersion {
		return openCodeManifest{}, fmt.Errorf("%w: OpenCode %s conversations are not supported", ErrUnsupported, manifest.Version)
	}
	return manifest, nil
}
