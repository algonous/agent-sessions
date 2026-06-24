package data

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSessions(t *testing.T) {
	dir := t.TempDir()
	content := `{"sessionId":"aaa","timestamp":1000,"project":"/Users/kfu/code/foo","display":"hello world"}
{"sessionId":"aaa","timestamp":2000,"project":"/Users/kfu/code/foo","display":"second msg"}
{"sessionId":"bbb","timestamp":3000,"project":"/Users/kfu/code/bar","display":"other session"}
`
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	sessions, err := LoadSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	// Most recent first (bbb has timestamp 3000).
	if sessions[0].SessionID != "claude:bbb" {
		t.Errorf("expected bbb first, got %s", sessions[0].SessionID)
	}
	if sessions[1].SessionID != "claude:aaa" {
		t.Errorf("expected aaa second, got %s", sessions[1].SessionID)
	}

	// Check aaa details.
	aaa := sessions[1]
	if aaa.MessageCount != 2 {
		t.Errorf("expected 2 messages, got %d", aaa.MessageCount)
	}
	if aaa.FirstTS != 1000 {
		t.Errorf("expected FirstTS=1000, got %d", aaa.FirstTS)
	}
	if aaa.LastTS != 2000 {
		t.Errorf("expected LastTS=2000, got %d", aaa.LastTS)
	}
	if aaa.FirstMessage != "hello world" {
		t.Errorf("expected first message 'hello world', got %q", aaa.FirstMessage)
	}
	if aaa.ProjectName != "foo" {
		t.Errorf("expected project name 'foo', got %q", aaa.ProjectName)
	}
}

func TestLoadSessionsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	sessions, err := LoadSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestLoadSessionsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	content := `not json
{"sessionId":"aaa","timestamp":1000,"project":"/foo","display":"ok"}
{"bad json
`
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	sessions, err := LoadSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
}

func TestProjectName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Users/kfu/code/foo", "foo"},
		{"/Users/kfu/code/foo/", "foo"},
		{"", ""},
		{"/", "."},
	}
	for _, tt := range tests {
		got := projectName(tt.input)
		if got != tt.want {
			t.Errorf("projectName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEncodeProjectDir(t *testing.T) {
	got := EncodeProjectDir("/Users/kfu/code/foo")
	want := "-Users-kfu-code-foo"
	if got != want {
		t.Errorf("EncodeProjectDir = %q, want %q", got, want)
	}
}

func TestFindTranscriptPath(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "projects", "-Users-kfu-code-foo")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(projDir, "abc-123.jsonl")
	if err := os.WriteFile(sessionFile, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := FindTranscriptPath(dir, "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if got != sessionFile {
		t.Errorf("got %q, want %q", got, sessionFile)
	}

	_, err = FindTranscriptPath(dir, "nonexistent")
	if !os.IsNotExist(err) {
		t.Errorf("expected not-exist error, got %v", err)
	}
}

func TestFindTranscriptPathAmbiguous(t *testing.T) {
	dir := t.TempDir()
	rawID := "abc-123"
	for _, proj := range []string{"-Users-kfu-code-foo", "-tmp-xyz"} {
		projDir := filepath.Join(dir, "projects", proj)
		if err := os.MkdirAll(projDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projDir, rawID+".jsonl"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := FindTranscriptPath(dir, rawID)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestResolveTranscriptPathPrefersProject(t *testing.T) {
	dir := t.TempDir()
	rawID := "abc-123"
	ownProject := "/Users/kfu/code/foo"
	ownDir := filepath.Join(dir, "projects", ClaudeProjectDirName(ownProject))
	otherDir := filepath.Join(dir, "projects", "-tmp-xyz")
	for _, d := range []string{ownDir, otherDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, rawID+".jsonl"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// FilePath deliberately points at the wrong project's fragment, as a
	// stale resolution from indexClaudeTranscriptText would.
	s := SessionSummary{
		Source:       SourceClaude,
		RawSessionID: rawID,
		DataDir:      dir,
		Project:      ownProject,
		FilePath:     filepath.Join(otherDir, rawID+".jsonl"),
	}
	got, err := ResolveTranscriptPath(s)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ownDir, rawID+".jsonl")
	if got != want {
		t.Fatalf("ResolveTranscriptPath = %q, want project fragment %q", got, want)
	}
}
