package data

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GenerateJSONL returns JSONL bytes with one line per selected block.
// When blocks is nil, all blocks in the transcript are exported.
// Each element of blocks is [roundIdx, blockIdx].
func GenerateJSONL(session SessionSummary, transcript *Transcript, blocks [][2]int) []byte {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if blocks == nil {
		for _, r := range transcript.Rounds {
			for bi, b := range r.Blocks {
				enc.Encode(makeExportBlock(session.SessionID, r.Index, bi, b))
			}
		}
	} else {
		roundMap := indexRounds(transcript.Rounds)
		for _, pair := range blocks {
			ri, bi := pair[0], pair[1]
			r, ok := roundMap[ri]
			if !ok || bi < 0 || bi >= len(r.Blocks) {
				continue
			}
			enc.Encode(makeExportBlock(session.SessionID, ri, bi, r.Blocks[bi]))
		}
	}

	return []byte(buf.String())
}

// GenerateMarkdown returns markdown bytes with selected blocks grouped by round.
// When blocks is nil, all blocks in the transcript are exported.
// Each element of blocks is [roundIdx, blockIdx].
func GenerateMarkdown(session SessionSummary, transcript *Transcript, blocks [][2]int) []byte {
	var buf strings.Builder

	totalBlocks := 0
	if blocks == nil {
		for _, r := range transcript.Rounds {
			totalBlocks += len(r.Blocks)
		}
	} else {
		totalBlocks = len(blocks)
	}

	// YAML frontmatter.
	fmt.Fprintf(&buf, "---\n")
	fmt.Fprintf(&buf, "session: %s\n", session.SessionID)
	fmt.Fprintf(&buf, "project: %s\n", session.Project)
	fmt.Fprintf(&buf, "exported_blocks: %d\n", totalBlocks)
	fmt.Fprintf(&buf, "---\n\n")

	if blocks == nil {
		for _, r := range transcript.Rounds {
			writeMarkdownRound(&buf, r, nil)
		}
	} else {
		// Group selected block indices by round.
		selected := make(map[int][]int)
		for _, pair := range blocks {
			selected[pair[0]] = append(selected[pair[0]], pair[1])
		}
		for _, r := range transcript.Rounds {
			if bis, ok := selected[r.Index]; ok {
				sort.Ints(bis)
				writeMarkdownRound(&buf, r, bis)
			}
		}
	}

	return []byte(buf.String())
}

func writeMarkdownRound(buf *strings.Builder, r Round, blockIndices []int) {
	ts := r.UserTimestamp
	if ts == "" {
		ts = "unknown"
	}
	fmt.Fprintf(buf, "## Round %d (%s)\n\n", r.Index+1, ts)

	// Collect the blocks to write (all or selected).
	var selected []Block
	if blockIndices == nil {
		selected = r.Blocks
	} else {
		for _, bi := range blockIndices {
			if bi >= 0 && bi < len(r.Blocks) {
				selected = append(selected, r.Blocks[bi])
			}
		}
	}

	// Write blocks, merging consecutive tool blocks into one fenced block.
	for i := 0; i < len(selected); i++ {
		b := selected[i]
		if b.Role == "tool" {
			// Collect the entire run of consecutive tool blocks.
			var lines []string
			for i < len(selected) && selected[i].Role == "tool" {
				if selected[i].ToolCall != nil {
					if selected[i].ToolCall.InputSummary != "" {
						lines = append(lines, selected[i].ToolCall.Name+": "+selected[i].ToolCall.InputSummary)
					} else {
						lines = append(lines, selected[i].ToolCall.Name)
					}
				}
				i++
			}
			i-- // outer loop will increment
			if len(lines) > 0 {
				fmt.Fprintf(buf, "```tool_use\n%s\n```\n\n", strings.Join(lines, "\n"))
			}
		} else {
			writeMarkdownBlock(buf, b)
		}
	}
}

func writeMarkdownBlock(buf *strings.Builder, b Block) {
	var tag string
	switch b.Role {
	case "you", "context":
		tag = "prompt"
	case "thinking":
		tag = "thinking"
	case "claude":
		tag = "assistant"
	default:
		return
	}
	fence := makeFence(b.Text)
	fmt.Fprintf(buf, "%s%s\n%s\n%s\n\n", fence, tag, b.Text, fence)
}

// makeFence returns a backtick fence that is longer than any backtick
// sequence found in text, with a minimum of 3.
func makeFence(text string) string {
	max := 0
	cur := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '`' {
			cur++
			if cur > max {
				max = cur
			}
		} else {
			cur = 0
		}
	}
	n := max + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

func makeExportBlock(sessionID string, roundIndex, blockIndex int, b Block) ExportBlock {
	eb := ExportBlock{
		SessionID:  sessionID,
		RoundIndex: roundIndex,
		BlockIndex: blockIndex,
		Role:       b.Role,
	}
	if b.ToolCall != nil {
		eb.Name = b.ToolCall.Name
		eb.InputSummary = b.ToolCall.InputSummary
	} else {
		eb.Text = b.Text
	}
	return eb
}

func indexRounds(rounds []Round) map[int]*Round {
	m := make(map[int]*Round, len(rounds))
	for i := range rounds {
		m[rounds[i].Index] = &rounds[i]
	}
	return m
}

// ConfigDir returns the agent-sessions config directory, respecting XDG_CONFIG_HOME.
func ConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "agent-sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "agent-sessions")
}
