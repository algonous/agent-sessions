package data

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type MigrationOptions struct {
	Source    string
	DataDir   string
	SessionID string
	TargetDir string
	DryRun    bool
	BackupDir string
}

type MigrationPlan struct {
	Source       string
	SessionID    string
	RawSessionID string
	DataDir      string
	OldProject   string
	TargetDir    string
	BackupDir    string
	Operations   []MigrationOperation
	Warnings     []string

	session        SessionSummary
	claudeOldDir   string
	claudeNewDir   string
	claudeHistory  string
	claudeSessionD string
	codexStateDB   string
	codexRollout   string
}

type MigrationOperation struct {
	Action string
	Path   string
	Detail string
}

type codexThread struct {
	id          string
	rolloutPath string
	cwd         string
}

func MigrateSession(opts MigrationOptions) (*MigrationPlan, error) {
	plan, err := PlanSessionMigration(opts)
	if err != nil {
		return nil, err
	}
	if opts.DryRun {
		return plan, nil
	}
	if err := ExecuteSessionMigration(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func PlanSessionMigration(opts MigrationOptions) (*MigrationPlan, error) {
	if strings.TrimSpace(opts.SessionID) == "" {
		return nil, errors.New("session id is required")
	}
	targetDir, err := cleanAbsPath(opts.TargetDir)
	if err != nil {
		return nil, fmt.Errorf("target dir: %w", err)
	}
	if info, err := os.Stat(targetDir); err != nil {
		return nil, fmt.Errorf("target dir: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("target dir is not a directory: %s", targetDir)
	}

	session, err := resolveMigrationSession(opts)
	if err != nil {
		return nil, err
	}
	if session.Project == "" && session.Source == SourceCodex {
		thread, err := findCodexThread(session.DataDir, session.RawSessionID)
		if err == nil {
			session.Project = thread.cwd
			session.ProjectName = projectName(thread.cwd)
			session.FilePath = thread.rolloutPath
		}
	}
	if session.Project == "" {
		return nil, fmt.Errorf("could not determine current work dir for %s", session.SessionID)
	}
	oldProject, err := cleanAbsPath(session.Project)
	if err == nil {
		session.Project = oldProject
	}
	if session.Project == targetDir {
		return nil, fmt.Errorf("session is already in target dir: %s", targetDir)
	}

	backupDir := opts.BackupDir
	if backupDir != "" {
		backupDir, err = cleanAbsPath(backupDir)
		if err != nil {
			return nil, fmt.Errorf("backup dir: %w", err)
		}
	} else {
		backupDir = defaultMigrationBackupDir(session.DataDir, session.Source, session.RawSessionID)
	}

	plan := &MigrationPlan{
		Source:       session.Source,
		SessionID:    session.SessionID,
		RawSessionID: session.RawSessionID,
		DataDir:      session.DataDir,
		OldProject:   session.Project,
		TargetDir:    targetDir,
		BackupDir:    backupDir,
		session:      session,
	}

	switch session.Source {
	case SourceClaude:
		if err := planClaudeMigration(plan); err != nil {
			return nil, err
		}
	case SourceCodex:
		if err := planCodexMigration(plan); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported source: %s", session.Source)
	}
	return plan, nil
}

func ExecuteSessionMigration(plan *MigrationPlan) error {
	if plan == nil {
		return errors.New("nil migration plan")
	}
	switch plan.Source {
	case SourceClaude:
		return executeClaudeMigration(plan)
	case SourceCodex:
		return executeCodexMigration(plan)
	default:
		return fmt.Errorf("unsupported source: %s", plan.Source)
	}
}

func resolveMigrationSession(opts MigrationOptions) (SessionSummary, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return SessionSummary{}, err
	}

	source := strings.TrimSpace(opts.Source)
	if source == "" || source == "auto" {
		source = ""
	}
	keySource, rawID := SplitSessionKey(opts.SessionID)
	if keySource == SourceClaude || keySource == SourceCodex {
		if source != "" && source != keySource {
			return SessionSummary{}, fmt.Errorf("session id source %q does not match --source %q", keySource, source)
		}
		source = keySource
	}
	if source != "" && source != SourceClaude && source != SourceCodex {
		return SessionSummary{}, fmt.Errorf("unknown source: %s", source)
	}
	if rawID == "" {
		return SessionSummary{}, errors.New("raw session id is empty")
	}

	var roots []SourceRoot
	if opts.DataDir != "" {
		dir, err := cleanAbsPath(opts.DataDir)
		if err != nil {
			return SessionSummary{}, err
		}
		detected := DetectSourceFromDir(dir)
		if source == "" {
			source = detected
		}
		if source == "" {
			return SessionSummary{}, fmt.Errorf("cannot infer source from data dir: %s", dir)
		}
		roots = []SourceRoot{{Source: source, Dir: dir}}
	} else if source != "" {
		adapter, ok := AdapterBySource(source)
		if !ok {
			return SessionSummary{}, fmt.Errorf("unknown source: %s", source)
		}
		roots = []SourceRoot{{Source: source, Dir: filepath.Join(home, adapter.DirName)}}
	} else {
		roots = DiscoverDefaultRoots(home)
	}

	sessions, err := LoadSessionsMulti(roots)
	if err != nil {
		return SessionSummary{}, err
	}
	var matches []SessionSummary
	for _, session := range sessions {
		if source != "" && session.Source != source {
			continue
		}
		if session.RawSessionID == rawID || session.SessionID == opts.SessionID {
			matches = append(matches, session)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		var ids []string
		for _, match := range matches {
			ids = append(ids, match.SessionID)
		}
		return SessionSummary{}, fmt.Errorf("ambiguous session id %q; use a source-prefixed id or --source (matches: %s)", opts.SessionID, strings.Join(ids, ", "))
	}

	if source == SourceCodex && len(roots) == 1 {
		thread, err := findCodexThread(roots[0].Dir, rawID)
		if err == nil {
			return SessionSummary{
				SessionID:    MakeSessionKey(SourceCodex, rawID),
				RawSessionID: rawID,
				Source:       SourceCodex,
				DataDir:      roots[0].Dir,
				Project:      thread.cwd,
				ProjectName:  projectName(thread.cwd),
				FilePath:     thread.rolloutPath,
			}, nil
		}
	}

	return SessionSummary{}, fmt.Errorf("session not found: %s", opts.SessionID)
}

func planClaudeMigration(plan *MigrationPlan) error {
	otherActive, err := checkClaudeActiveSessions(plan.DataDir, plan.RawSessionID)
	if err != nil {
		return err
	}
	transcriptPath, err := ResolveTranscriptPath(plan.session)
	if err != nil {
		return fmt.Errorf("resolve Claude transcript: %w", err)
	}
	oldProjectDir := filepath.Dir(transcriptPath)
	newProjectDir := filepath.Join(plan.DataDir, "projects", ClaudeProjectDirName(plan.TargetDir))
	newTranscriptPath := filepath.Join(newProjectDir, plan.RawSessionID+".jsonl")
	sessionDir := filepath.Join(oldProjectDir, plan.RawSessionID)
	newSessionDir := filepath.Join(newProjectDir, plan.RawSessionID)
	if _, err := os.Stat(newTranscriptPath); err == nil {
		return fmt.Errorf("target transcript already exists: %s", newTranscriptPath)
	}
	if _, err := os.Stat(newSessionDir); err == nil {
		return fmt.Errorf("target session directory already exists: %s", newSessionDir)
	}

	plan.claudeHistory = filepath.Join(plan.DataDir, "history.jsonl")
	plan.claudeOldDir = oldProjectDir
	plan.claudeNewDir = newProjectDir
	plan.claudeSessionD = sessionDir
	plan.session.FilePath = transcriptPath
	plan.Operations = append(plan.Operations,
		MigrationOperation{Action: "write", Path: newTranscriptPath, Detail: "copy transcript and update top-level cwd fields"},
	)
	if info, err := os.Stat(sessionDir); err == nil && info.IsDir() {
		plan.Operations = append(plan.Operations, MigrationOperation{Action: "copy", Path: sessionDir, Detail: newSessionDir})
	}
	plan.Operations = append(plan.Operations,
		MigrationOperation{Action: "rewrite", Path: plan.claudeHistory, Detail: "update matching history project fields"},
		MigrationOperation{Action: "remove", Path: transcriptPath, Detail: "after history update succeeds"},
	)
	if info, err := os.Stat(sessionDir); err == nil && info.IsDir() {
		plan.Operations = append(plan.Operations, MigrationOperation{Action: "remove", Path: sessionDir, Detail: "after history update succeeds"})
	}
	if _, err := os.Stat(filepath.Join(oldProjectDir, "memory")); err == nil {
		plan.Warnings = append(plan.Warnings, "old Claude project-level memory exists and will not be moved")
	}
	if otherActive {
		plan.Warnings = append(plan.Warnings, "other active Claude sessions may append to history.jsonl during migration; concurrent history entries may not be preserved (best-effort)")
	}
	return nil
}

func executeClaudeMigration(plan *MigrationPlan) error {
	transcriptPath := plan.session.FilePath
	newTranscriptPath := filepath.Join(plan.claudeNewDir, plan.RawSessionID+".jsonl")
	newSessionDir := filepath.Join(plan.claudeNewDir, plan.RawSessionID)

	if err := os.MkdirAll(plan.BackupDir, 0755); err != nil {
		return err
	}
	if err := backupPath(plan.claudeHistory, plan.DataDir, plan.BackupDir); err != nil {
		return err
	}
	if err := backupPath(transcriptPath, plan.DataDir, plan.BackupDir); err != nil {
		return err
	}
	if info, err := os.Stat(plan.claudeSessionD); err == nil && info.IsDir() {
		if err := backupPath(plan.claudeSessionD, plan.DataDir, plan.BackupDir); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(plan.claudeNewDir, 0755); err != nil {
		return err
	}
	if err := writeTopLevelCWDJSONLToPath(transcriptPath, newTranscriptPath, plan.OldProject, plan.TargetDir); err != nil {
		return err
	}
	if info, err := os.Stat(plan.claudeSessionD); err == nil && info.IsDir() {
		if err := copyDir(plan.claudeSessionD, newSessionDir); err != nil {
			return err
		}
	}
	if err := rewriteClaudeHistory(plan.claudeHistory, plan.RawSessionID, plan.TargetDir); err != nil {
		return err
	}
	if err := os.Remove(transcriptPath); err != nil {
		return err
	}
	if info, err := os.Stat(plan.claudeSessionD); err == nil && info.IsDir() {
		if err := os.RemoveAll(plan.claudeSessionD); err != nil {
			return err
		}
	}
	return nil
}

func planCodexMigration(plan *MigrationPlan) error {
	thread, err := findCodexThread(plan.DataDir, plan.RawSessionID)
	if err != nil {
		return err
	}
	if thread.cwd == "" {
		return fmt.Errorf("Codex thread has empty cwd: %s", plan.RawSessionID)
	}
	if thread.cwd != plan.OldProject {
		plan.OldProject = thread.cwd
	}
	if _, err := os.Stat(thread.rolloutPath); err != nil {
		return fmt.Errorf("Codex rollout path: %w", err)
	}
	plan.codexStateDB = filepath.Join(plan.DataDir, "state_5.sqlite")
	plan.codexRollout = thread.rolloutPath
	plan.session.FilePath = thread.rolloutPath
	plan.Warnings = append(plan.Warnings, "close Codex before migrating; active writers are rejected on a best-effort SQLite lock check")
	plan.Operations = append(plan.Operations,
		MigrationOperation{Action: "update", Path: plan.codexStateDB, Detail: "set threads.cwd"},
		MigrationOperation{Action: "rewrite", Path: thread.rolloutPath, Detail: "update session_meta/turn_context cwd and workspace_roots"},
	)
	return nil
}

func executeCodexMigration(plan *MigrationPlan) error {
	if err := checkCodexWritable(plan.codexStateDB); err != nil {
		return err
	}
	if err := os.MkdirAll(plan.BackupDir, 0755); err != nil {
		return err
	}
	for _, path := range []string{plan.codexStateDB, plan.codexStateDB + "-wal", plan.codexStateDB + "-shm", plan.codexRollout} {
		if _, err := os.Stat(path); err == nil {
			if err := backupPath(path, plan.DataDir, plan.BackupDir); err != nil {
				return err
			}
		}
	}
	if err := updateCodexThreadCWD(plan.codexStateDB, plan.RawSessionID, plan.TargetDir); err != nil {
		return err
	}
	return rewriteCodexRolloutCWD(plan.codexRollout, plan.OldProject, plan.TargetDir)
}

func rewriteClaudeHistory(path, rawSessionID, targetDir string) error {
	return rewriteJSONL(path, func(obj map[string]any) bool {
		if sid, _ := obj["sessionId"].(string); sid != rawSessionID {
			return false
		}
		if obj["project"] == targetDir {
			return false
		}
		obj["project"] = targetDir
		return true
	})
}

func rewriteTopLevelCWDJSONL(path, oldCWD, targetDir string) error {
	return rewriteJSONL(path, func(obj map[string]any) bool {
		cwd, _ := obj["cwd"].(string)
		if cwd != oldCWD {
			return false
		}
		obj["cwd"] = targetDir
		return true
	})
}

func writeTopLevelCWDJSONLToPath(srcPath, dstPath, oldCWD, targetDir string) error {
	return writeJSONLToPath(srcPath, dstPath, func(obj map[string]any) bool {
		cwd, _ := obj["cwd"].(string)
		if cwd != oldCWD {
			return false
		}
		obj["cwd"] = targetDir
		return true
	})
}

func rewriteCodexRolloutCWD(path, oldCWD, targetDir string) error {
	return rewriteJSONL(path, func(obj map[string]any) bool {
		entryType, _ := obj["type"].(string)
		if entryType != "session_meta" && entryType != "turn_context" {
			return false
		}
		payload, ok := obj["payload"].(map[string]any)
		if !ok {
			return false
		}
		changed := false
		if cwd, _ := payload["cwd"].(string); cwd == oldCWD {
			payload["cwd"] = targetDir
			changed = true
		}
		if roots, ok := payload["workspace_roots"].([]any); ok {
			for i, root := range roots {
				if s, ok := root.(string); ok && s == oldCWD {
					roots[i] = targetDir
					changed = true
				}
			}
		}
		return changed
	})
}

func rewriteJSONL(path string, mutate func(map[string]any) bool) error {
	tmpPath, err := writeJSONLTemp(path, filepath.Dir(path), mutate)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func writeJSONLToPath(srcPath, dstPath string, mutate func(map[string]any) bool) error {
	tmpPath, err := writeJSONLTemp(srcPath, filepath.Dir(dstPath), mutate)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func writeJSONLTemp(path, tmpDir string, mutate func(map[string]any) bool) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()

	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(tmpDir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	writer := bufio.NewWriter(tmp)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err == nil && mutate(obj) {
			nextLine, err := json.Marshal(obj)
			if err != nil {
				return "", err
			}
			line = nextLine
		}
		if _, err := writer.Write(line); err != nil {
			return "", err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return "", err
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if err := writer.Flush(); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err == nil {
		if chmodErr := os.Chmod(tmpPath, info.Mode()); chmodErr != nil {
			return "", chmodErr
		}
	}
	committed = true
	return tmpPath, nil
}

func findCodexThread(codexDir, rawSessionID string) (codexThread, error) {
	dbPath := filepath.Join(codexDir, "state_5.sqlite")
	db, err := openSQLite(dbPath)
	if err != nil {
		return codexThread{}, err
	}
	defer db.Close()
	var thread codexThread
	err = db.QueryRow("SELECT id, rollout_path, cwd FROM threads WHERE id = ?", rawSessionID).Scan(&thread.id, &thread.rolloutPath, &thread.cwd)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return codexThread{}, fmt.Errorf("Codex thread not found in state_5.sqlite: %s", rawSessionID)
		}
		return codexThread{}, err
	}
	return thread, nil
}

func updateCodexThreadCWD(dbPath, rawSessionID, targetDir string) error {
	db, err := openSQLite(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE threads SET cwd = ? WHERE id = ?", targetDir, rawSessionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("updated %d Codex thread rows, want 1", n)
	}
	return tx.Commit()
}

func checkCodexWritable(dbPath string) error {
	db, err := openSQLite(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("Codex state database is busy; close Codex and retry: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE threads SET cwd = cwd WHERE id = '__agent_sessions_write_check__'"); err != nil {
		return fmt.Errorf("Codex state database is busy; close Codex and retry: %w", err)
	}
	return nil
}

func openSQLite(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 2000"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// checkClaudeActiveSessions scans ~/.claude/sessions/*.json. It returns an error if the
// session being migrated (rawSessionID) is itself active, since migration deletes its
// transcript and session dir. Unrelated active sessions do not block migration but are
// reported via otherActive so the caller can warn about the shared history.jsonl rewrite.
func checkClaudeActiveSessions(claudeDir, rawSessionID string) (otherActive bool, err error) {
	entries, err := os.ReadDir(filepath.Join(claudeDir, "sessions"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(claudeDir, "sessions", entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var state struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
			CWD       string `json:"cwd"`
		}
		if json.Unmarshal(b, &state) != nil || state.SessionID == "" {
			continue
		}
		if claudeSessionStatusInactive(state.Status) {
			continue
		}
		if state.SessionID == rawSessionID {
			return false, fmt.Errorf("Claude session appears active in %s (session=%s status=%q cwd=%q); close that Claude session before migrating", path, state.SessionID, state.Status, state.CWD)
		}
		otherActive = true
	}
	return otherActive, nil
}

func claudeSessionStatusInactive(status string) bool {
	switch strings.ToLower(status) {
	case "done", "complete", "completed", "exited", "stopped", "failed", "error":
		return true
	default:
		return false
	}
}

func backupPath(path, baseDir, backupRoot string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(path)
	}
	dst := filepath.Join(backupRoot, rel)
	if info.IsDir() {
		return copyDir(path, dst)
	}
	return copyFile(path, dst, info.Mode())
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func defaultMigrationBackupDir(dataDir, source, rawSessionID string) string {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	shortID := rawSessionID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return filepath.Join(dataDir, "backups", "agent-sessions", stamp+"-"+source+"-"+shortID)
}

func cleanAbsPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	expanded, err := expandUserPath(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func expandUserPath(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
