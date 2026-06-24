package data

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testTranscript() (*Transcript, SessionSummary) {
	session := SessionSummary{
		SessionID:   "test-session",
		Project:     "/Users/kfu/code/foo",
		ProjectName: "foo",
	}
	transcript := &Transcript{
		SessionID: "test-session",
		Rounds: []Round{
			{
				Index:         0,
				UserTimestamp: "2026-02-26T11:00:00Z",
				Blocks: []Block{
					{Role: "you", Text: "hello"},
					{Role: "thinking", Text: "let me think about this"},
					{Role: "tool", ToolCall: &ToolCall{Name: "Read", InputSummary: "/foo/bar.go"}},
					{Role: "claude", Text: "Hi there!"},
					{Role: "claude", Text: "How can I help?"},
				},
				Usage: Usage{InputTokens: 100, OutputTokens: 50, CacheRead: 200, CacheCreation: 10},
			},
			{
				Index:         1,
				UserTimestamp: "2026-02-26T11:01:00Z",
				Blocks: []Block{
					{Role: "you", Text: "fix bug"},
					{Role: "claude", Text: "Done!"},
				},
				Usage: Usage{InputTokens: 200, OutputTokens: 60},
			},
		},
	}
	return transcript, session
}

func TestGenerateJSONLAll(t *testing.T) {
	transcript, session := testTranscript()
	out := GenerateJSONL(session, transcript, nil)

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// 5 blocks in round 0 + 2 blocks in round 1 = 7 lines.
	if len(lines) != 7 {
		t.Fatalf("expected 7 lines, got %d", len(lines))
	}

	var first ExportBlock
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.SessionID != "test-session" {
		t.Errorf("session_id = %q", first.SessionID)
	}
	if first.RoundIndex != 0 || first.BlockIndex != 0 {
		t.Errorf("first block: round=%d block=%d", first.RoundIndex, first.BlockIndex)
	}
	if first.Role != "you" || first.Text != "hello" {
		t.Errorf("first block: role=%q text=%q", first.Role, first.Text)
	}

	// Check tool block (index 2 in round 0).
	var tool ExportBlock
	if err := json.Unmarshal([]byte(lines[2]), &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Role != "tool" || tool.Name != "Read" || tool.InputSummary != "/foo/bar.go" {
		t.Errorf("tool block: role=%q name=%q input=%q", tool.Role, tool.Name, tool.InputSummary)
	}
	if tool.Text != "" {
		t.Errorf("tool block should have no text, got %q", tool.Text)
	}

	// Last line: round 1, block 1 (claude, "Done!").
	var last ExportBlock
	if err := json.Unmarshal([]byte(lines[6]), &last); err != nil {
		t.Fatal(err)
	}
	if last.RoundIndex != 1 || last.BlockIndex != 1 || last.Role != "claude" || last.Text != "Done!" {
		t.Errorf("last block: round=%d block=%d role=%q text=%q", last.RoundIndex, last.BlockIndex, last.Role, last.Text)
	}
}

func TestGenerateJSONLSelectedBlocks(t *testing.T) {
	transcript, session := testTranscript()

	// Select: round 0 block 0 (you), round 0 block 3 (claude), round 1 block 1 (claude).
	blocks := [][2]int{{0, 0}, {0, 3}, {1, 1}}
	out := GenerateJSONL(session, transcript, blocks)

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	var b0 ExportBlock
	json.Unmarshal([]byte(lines[0]), &b0)
	if b0.Role != "you" || b0.Text != "hello" {
		t.Errorf("block 0: role=%q text=%q", b0.Role, b0.Text)
	}

	var b1 ExportBlock
	json.Unmarshal([]byte(lines[1]), &b1)
	if b1.Role != "claude" || b1.Text != "Hi there!" {
		t.Errorf("block 1: role=%q text=%q", b1.Role, b1.Text)
	}
	if b1.RoundIndex != 0 || b1.BlockIndex != 3 {
		t.Errorf("block 1: round=%d block=%d", b1.RoundIndex, b1.BlockIndex)
	}

	var b2 ExportBlock
	json.Unmarshal([]byte(lines[2]), &b2)
	if b2.Role != "claude" || b2.Text != "Done!" || b2.RoundIndex != 1 {
		t.Errorf("block 2: role=%q text=%q round=%d", b2.Role, b2.Text, b2.RoundIndex)
	}
}

func TestGenerateJSONLInvalidBlocks(t *testing.T) {
	transcript, session := testTranscript()

	// Include some invalid block references that should be skipped.
	blocks := [][2]int{{0, 0}, {99, 0}, {0, 99}}
	out := GenerateJSONL(session, transcript, blocks)

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (invalid refs skipped), got %d", len(lines))
	}
}

func TestGenerateMarkdownAll(t *testing.T) {
	transcript, session := testTranscript()
	content := string(GenerateMarkdown(session, transcript, nil))

	checks := []string{
		"---\nsession: test-session\n",
		"project: /Users/kfu/code/foo\n",
		"exported_blocks: 7\n",
		"---",
		"## Round 1 (2026-02-26T11:00:00Z)",
		"```prompt\nhello\n```",
		"```thinking\nlet me think about this\n```",
		"```tool_use\nRead: /foo/bar.go\n```",
		"```assistant\nHi there!\n```",
		"```assistant\nHow can I help?\n```",
		"## Round 2 (2026-02-26T11:01:00Z)",
		"```prompt\nfix bug\n```",
		"```assistant\nDone!\n```",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("markdown missing: %q", check)
		}
	}
}

func TestGenerateMarkdownSelectedBlocks(t *testing.T) {
	transcript, session := testTranscript()

	// Select: round 0 block 0 (you) + block 3 (claude), round 1 block 1 (claude).
	blocks := [][2]int{{0, 0}, {0, 3}, {1, 1}}
	content := string(GenerateMarkdown(session, transcript, blocks))

	// Frontmatter should show 3 exported blocks.
	if !strings.Contains(content, "exported_blocks: 3") {
		t.Error("expected exported_blocks: 3")
	}

	// Round 1 should have prompt and assistant.
	if !strings.Contains(content, "```prompt\nhello\n```") {
		t.Error("should contain prompt block")
	}
	if !strings.Contains(content, "```assistant\nHi there!\n```") {
		t.Error("should contain assistant block")
	}

	// Thinking and tool should NOT be present (not selected).
	if strings.Contains(content, "thinking") {
		t.Error("should not contain thinking block")
	}
	if strings.Contains(content, "tool_use") {
		t.Error("should not contain tool block")
	}

	// Round 2: only claude.
	if !strings.Contains(content, "```assistant\nDone!\n```") {
		t.Error("should contain Done assistant block")
	}
	if strings.Contains(content, "```prompt\nfix bug\n```") {
		t.Error("round 2 should not contain prompt block")
	}
}

func TestGenerateMarkdownOmitsEmptyRounds(t *testing.T) {
	transcript, session := testTranscript()

	// Select only blocks from round 1, nothing from round 0.
	blocks := [][2]int{{1, 0}, {1, 1}}
	content := string(GenerateMarkdown(session, transcript, blocks))

	// Only Round 2 header should be present.
	if strings.Contains(content, "## Round 1") {
		t.Error("round 1 should be omitted (no blocks selected)")
	}
	if !strings.Contains(content, "## Round 2") {
		t.Error("round 2 should be present")
	}
}

func TestGenerateMarkdownMergesConsecutiveTools(t *testing.T) {
	session := SessionSummary{SessionID: "s1", Project: "/foo"}
	transcript := &Transcript{
		SessionID: "s1",
		Rounds: []Round{{
			Index:         0,
			UserTimestamp: "2026-02-26T11:00:00Z",
			Blocks: []Block{
				{Role: "you", Text: "do stuff"},
				{Role: "tool", ToolCall: &ToolCall{Name: "Read", InputSummary: "/a.go"}},
				{Role: "tool", ToolCall: &ToolCall{Name: "Edit", InputSummary: "/a.go"}},
				{Role: "tool", ToolCall: &ToolCall{Name: "Bash", InputSummary: "go test"}},
				{Role: "claude", Text: "Done"},
			},
		}},
	}
	content := string(GenerateMarkdown(session, transcript, nil))

	// All three tool blocks should be in one fenced block.
	want := "```tool_use\nRead: /a.go\nEdit: /a.go\nBash: go test\n```"
	if !strings.Contains(content, want) {
		t.Errorf("expected merged tool block, got:\n%s", content)
	}

	// Should NOT have separate tool_use blocks.
	count := strings.Count(content, "```tool_use")
	if count != 1 {
		t.Errorf("expected 1 tool_use fence, got %d", count)
	}
}

func TestGenerateMarkdownEscapesBackticks(t *testing.T) {
	session := SessionSummary{SessionID: "s1", Project: "/foo"}
	transcript := &Transcript{
		SessionID: "s1",
		Rounds: []Round{{
			Index:         0,
			UserTimestamp: "2026-02-26T11:00:00Z",
			Blocks: []Block{
				{Role: "claude", Text: "Here is code:\n```go\nfmt.Println(\"hi\")\n```\nDone."},
			},
		}},
	}
	content := string(GenerateMarkdown(session, transcript, nil))

	// The outer fence must use 4 backticks since content has triple backticks.
	want := "````assistant\nHere is code:\n```go\nfmt.Println(\"hi\")\n```\nDone.\n````"
	if !strings.Contains(content, want) {
		t.Errorf("expected escaped fence, got:\n%s", content)
	}
}

func TestConfigDir(t *testing.T) {
	// With XDG set.
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	got := ConfigDir()
	if got != "/tmp/xdg/agent-sessions" {
		t.Errorf("got %q with XDG set", got)
	}

	// Without XDG.
	t.Setenv("XDG_CONFIG_HOME", "")
	got = ConfigDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "agent-sessions")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
