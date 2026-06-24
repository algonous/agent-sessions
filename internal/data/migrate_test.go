package data

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeProjectDirName(t *testing.T) {
	got := ClaudeProjectDirName("/Users/kfu/repo/mdp-stateful-compute/_wt/JSDPB-1303/.dot")
	want := "-Users-kfu-repo-mdp-stateful-compute--wt-JSDPB-1303--dot"
	if got != want {
		t.Fatalf("ClaudeProjectDirName = %q, want %q", got, want)
	}
}

func TestMigrateClaudeSession(t *testing.T) {
	root := t.TempDir()
	oldProject := filepath.Join(root, "repo", "my_repo")
	targetProject := filepath.Join(root, "repo", "target.project")
	rawID := "11111111-2222-3333-4444-555555555555"
	oldProjectDir := filepath.Join(root, "projects", ClaudeProjectDirName(oldProject))
	sessionDir := filepath.Join(oldProjectDir, rawID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(oldProjectDir, "memory"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetProject, 0755); err != nil {
		t.Fatal(err)
	}
	history := `{"sessionId":"11111111-2222-3333-4444-555555555555","timestamp":1000,"project":"` + oldProject + `","display":"hello"}
{"sessionId":"other","timestamp":2000,"project":"/other","display":"other"}
`
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte(history), 0644); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","sessionId":"` + rawID + `","cwd":"` + oldProject + `","message":{"role":"user","content":"hello"}}
{"type":"assistant","sessionId":"` + rawID + `","cwd":"` + oldProject + `","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"` + oldProject + `/main.go"}}]}}
`
	if err := os.WriteFile(filepath.Join(oldProjectDir, rawID+".jsonl"), []byte(transcript), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "tool.json"), []byte(`{"ok":true}`), 0644); err != nil {
		t.Fatal(err)
	}

	plan, err := MigrateSession(MigrationOptions{
		Source:    SourceClaude,
		DataDir:   root,
		SessionID: rawID,
		TargetDir: targetProject,
		BackupDir: filepath.Join(root, "backup"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != SourceClaude || plan.OldProject != oldProject || plan.TargetDir != targetProject {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "memory") {
		t.Fatalf("expected memory warning, got %+v", plan.Warnings)
	}

	newProjectDir := filepath.Join(root, "projects", ClaudeProjectDirName(targetProject))
	if _, err := os.Stat(filepath.Join(newProjectDir, rawID+".jsonl")); err != nil {
		t.Fatalf("new transcript missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newProjectDir, rawID, "tool.json")); err != nil {
		t.Fatalf("session dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldProjectDir, "memory")); err != nil {
		t.Fatalf("project memory should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldProjectDir, rawID+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("old transcript should be gone, err=%v", err)
	}

	historyOut, err := os.ReadFile(filepath.Join(root, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(historyOut), `"project":"`+targetProject+`"`) {
		t.Fatalf("history not updated: %s", historyOut)
	}
	transcriptOut, err := os.ReadFile(filepath.Join(newProjectDir, rawID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcriptOut), `"cwd":"`+targetProject+`"`) {
		t.Fatalf("transcript cwd not updated: %s", transcriptOut)
	}
	if !strings.Contains(string(transcriptOut), `"file_path":"`+oldProject+`/main.go"`) {
		t.Fatalf("nested tool input should not be rewritten: %s", transcriptOut)
	}
	if _, err := os.Stat(filepath.Join(root, "backup", "history.jsonl")); err != nil {
		t.Fatalf("history backup missing: %v", err)
	}
}

func TestMigrateClaudeSessionRejectsActiveTargetSession(t *testing.T) {
	root := t.TempDir()
	rawID := "11111111-2222-3333-4444-555555555555"
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0755); err != nil {
		t.Fatal(err)
	}
	state := `{"sessionId":"` + rawID + `","status":"busy","cwd":"/tmp/old"}`
	if err := os.WriteFile(filepath.Join(root, "sessions", "123.json"), []byte(state), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte(`{"sessionId":"`+rawID+`","timestamp":1,"project":"/tmp/old","display":"hi"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, "projects", ClaudeProjectDirName("/tmp/old"))
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, rawID+".jsonl"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	_, err := PlanSessionMigration(MigrationOptions{Source: SourceClaude, DataDir: root, SessionID: rawID, TargetDir: target})
	if err == nil || !strings.Contains(err.Error(), "appears active") {
		t.Fatalf("expected active session error, got %v", err)
	}
	if strings.Contains(err.Error(), "close all") {
		t.Fatalf("error should not say 'close all', got %v", err)
	}
}

func TestMigrateClaudeSessionAllowsUnrelatedActiveSession(t *testing.T) {
	root := t.TempDir()
	oldProject := filepath.Join(root, "repo", "my_repo")
	targetProject := filepath.Join(root, "repo", "target_project")
	rawID := "11111111-2222-3333-4444-555555555555"
	oldProjectDir := filepath.Join(root, "projects", ClaudeProjectDirName(oldProject))
	sessionDir := filepath.Join(oldProjectDir, rawID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetProject, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0755); err != nil {
		t.Fatal(err)
	}
	// An unrelated active session must not block the migration.
	state := `{"sessionId":"99999999-2222-3333-4444-555555555555","status":"busy","cwd":"/tmp/other"}`
	if err := os.WriteFile(filepath.Join(root, "sessions", "123.json"), []byte(state), 0644); err != nil {
		t.Fatal(err)
	}
	history := `{"sessionId":"` + rawID + `","timestamp":1000,"project":"` + oldProject + `","display":"hello"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte(history), 0644); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","sessionId":"` + rawID + `","cwd":"` + oldProject + `","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(filepath.Join(oldProjectDir, rawID+".jsonl"), []byte(transcript), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "tool.json"), []byte(`{"ok":true}`), 0644); err != nil {
		t.Fatal(err)
	}

	plan, err := MigrateSession(MigrationOptions{
		Source:    SourceClaude,
		DataDir:   root,
		SessionID: rawID,
		TargetDir: targetProject,
		BackupDir: filepath.Join(root, "backup"),
	})
	if err != nil {
		t.Fatal(err)
	}

	newProjectDir := filepath.Join(root, "projects", ClaudeProjectDirName(targetProject))
	if _, err := os.Stat(filepath.Join(newProjectDir, rawID+".jsonl")); err != nil {
		t.Fatalf("new transcript missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newProjectDir, rawID, "tool.json")); err != nil {
		t.Fatalf("session dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldProjectDir, rawID+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("old transcript should be gone, err=%v", err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("old session dir should be gone, err=%v", err)
	}
	historyOut, err := os.ReadFile(filepath.Join(root, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(historyOut), `"project":"`+targetProject+`"`) {
		t.Fatalf("history not updated: %s", historyOut)
	}
	foundWarning := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "history.jsonl") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("expected history.jsonl warning, got %+v", plan.Warnings)
	}
}

func TestMigrateCodexSession(t *testing.T) {
	root := t.TempDir()
	oldProject := filepath.Join(root, "old")
	targetProject := filepath.Join(root, "target")
	rawID := "019ef72e-ebe0-7833-a478-81555158a83c"
	if err := os.MkdirAll(oldProject, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetProject, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte(`{"session_id":"`+rawID+`","ts":1000,"text":"hi"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rolloutDir := filepath.Join(root, "sessions", "2026", "06", "24")
	if err := os.MkdirAll(rolloutDir, 0755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(rolloutDir, "rollout-2026-06-24T10-11-47-"+rawID+".jsonl")
	content := `{"type":"session_meta","payload":{"id":"` + rawID + `","cwd":"` + oldProject + `"}}
{"type":"turn_context","payload":{"cwd":"` + oldProject + `","workspace_roots":["` + oldProject + `","/extra"]}}
{"type":"event_msg","payload":{"type":"user_message","message":"hi"}}
`
	if err := os.WriteFile(rollout, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "state_5.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, cwd TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO threads (id, rollout_path, cwd) VALUES (?, ?, ?)", rawID, rollout, oldProject); err != nil {
		t.Fatal(err)
	}
	db.Close()

	plan, err := MigrateSession(MigrationOptions{
		Source:    SourceCodex,
		DataDir:   root,
		SessionID: rawID,
		TargetDir: targetProject,
		BackupDir: filepath.Join(root, "backup"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != SourceCodex {
		t.Fatalf("source = %s", plan.Source)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cwd string
	if err := db.QueryRow("SELECT cwd FROM threads WHERE id = ?", rawID).Scan(&cwd); err != nil {
		t.Fatal(err)
	}
	if cwd != targetProject {
		t.Fatalf("cwd = %q, want %q", cwd, targetProject)
	}
	rolloutOut, err := os.ReadFile(rollout)
	if err != nil {
		t.Fatal(err)
	}
	var sawWorkspaceRoot bool
	for _, line := range strings.Split(strings.TrimSpace(string(rolloutOut)), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatal(err)
		}
		payload, _ := obj["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		if cwd, _ := payload["cwd"].(string); cwd != "" && cwd != targetProject {
			t.Fatalf("payload cwd = %q, want %q", cwd, targetProject)
		}
		if roots, ok := payload["workspace_roots"].([]any); ok {
			if roots[0] != targetProject {
				t.Fatalf("workspace root = %q, want %q", roots[0], targetProject)
			}
			sawWorkspaceRoot = true
		}
	}
	if !sawWorkspaceRoot {
		t.Fatal("did not see workspace_roots")
	}
	if _, err := os.Stat(filepath.Join(root, "backup", "state_5.sqlite")); err != nil {
		t.Fatalf("sqlite backup missing: %v", err)
	}
}
