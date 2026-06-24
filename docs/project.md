# agent-sessions: Agent Session Browser

## Overview

A web-based application that browses Claude Code and Codex conversation history stored locally.

## Features

- Browse all Claude Code and Codex sessions with timestamps and project info
- Sort sessions by most recent activity
- View full transcripts organized by rounds; rounds ordered by time, blocks within each round ordered by time
- Fold/unfold blocks (context, tool, thinking folded by default)
- Token usage tracking per round (input, output, cache read, cache creation)
- Select blocks with checkboxes, copy text to clipboard
- Export sessions to JSONL or Markdown (downloads to browser)
- Filter sessions by project or content
- Deep links:
  - `/<sessionId>` opens a session
  - `/<sessionId>/<roundIdx>` opens and scrolls to a round
  - `/<sessionId>/<roundIdx>/<blockIdx>` opens and scrolls to a block

## Data Format (~/.claude/)

### Directory layout

```
~/.claude/
  history.jsonl                              # session index (one line per session event)
  projects/
    -Users-frank-code-foo/                   # encoded project path (/ -> -)
      <sessionId>.jsonl                      # transcript for one session
      agent-<id>.jsonl                       # subagent transcripts (not used)
    -Users-frank-code-bar/
      ...
```

### history.jsonl -- session index

One JSON line per session event. In current data this is primarily user-entered messages (including slash commands such as `/exit`).

Fields:

| Field            | Type              | Notes                                            |
|------------------|-------------------|--------------------------------------------------|
| `sessionId`      | string            | Groups lines into sessions                       |
| `timestamp`      | int64 (unix ms)   | When the message was sent                        |
| `project`        | string            | Absolute path of the working directory           |
| `display`        | string            | User-visible text for the event (free-form text, commands, etc.) |
| `pastedContents` | map[string]object | Optional; key is numeric string (`"1"`, `"2"`...). Value typically includes `id`, `type`, and either `content` or `contentHash` |

Grouping logic: all lines sharing the same `sessionId` form one session.

Exit-like entries in `display`:

- Exit-like is strictly defined as two commands: `"/exit"` and `"exit"`.
- Compare after trimming whitespace (`strings.TrimSpace`), so `"/exit "` is treated as `"/exit"`.
- Correct session state rule should be based on the chronologically last entry for a session: exited only if the last entry is one of the two exit commands above.
- Current `agent-sessions` codebase does **not** store an `exited` flag in `SessionSummary` or render live/exited status in UI.

Resuming a session (`claude --resume`) has two behaviors depending on whether the context window overflowed:

- **Context intact**: same session ID, same transcript file. New entries append to the same files.
- **Context overflowed**: new session ID, new transcript file. The first user message is usually a "continued from a previous conversation..." summary injected by Claude Code (see "System-injected context" below).

`history.jsonl` and transcript files overlap but are not equivalent:

- `history.jsonl` provides `project`, `display`, and `pastedContents`.
- Transcript files provide full turn-by-turn model/tool activity.
- Transcript entries do not contain `history.pastedContents` in the same shape.

### Transcript files -- per-session JSONL

Path: `~/.claude/projects/<encoded-project>/<sessionId>.jsonl`

Path encoding: non-alphanumeric path characters become `-`; existing `-`
characters are preserved. In practice this folds `/`, `_`, and `.` into `-`.
Example: `/Users/frank/code/foo` -> `-Users-frank-code-foo`

Each line is a JSON object with a top-level `type` field. Common observed types include:

| type                     | Description                                  |
|--------------------------|----------------------------------------------|
| `user`                   | User message or tool_result                  |
| `assistant`              | One model response (contains content blocks) |
| `progress`               | Streaming progress                           |
| `file-history-snapshot`  | File state snapshot                          |
| `queue-operation`        | Internal queue status                        |

`tool_use` and `thinking` are not top-level entry types. They are content block types *within* `assistant` entries (see "Content block types" below).

**Round segmentation**:

- A `user` entry whose `message.content` is a **JSON string** (a real user message) starts a **new round**.
- A `user` entry whose `message.content` is a **JSON array** (tool_result) continues the current round.
- All `assistant` entries following a user message belong to the same round. Thinking and tool_use blocks are inside these assistant entries and do not affect round boundaries.

**System-injected context**: Claude Code writes special user messages in specific situations. These are regular `user` entries but carry system context rather than real user input:

- `"This session is being continued from a previous conversation..."` -- written by `claude --resume` when a session ran out of context and is being continued with a summary of the prior conversation.
- `"Implement the following plan:"` -- written when Claude Code exits plan mode and begins implementing the approved plan.

### Content block types

Each `assistant` entry has `message.content` as a JSON array of content blocks. Each entry typically contains one block, but multiple assistant entries chain within the same round.

```json
{"type": "text", "text": "Here is the implementation..."}
```

```json
{"type": "thinking", "thinking": "Let me consider the approach..."}
```

```json
{"type": "tool_use", "name": "Read", "input": {"file_path": "/foo/bar.go"}}
```

### Usage tracking

Each assistant entry carries a `message.usage` object:

```json
{
  "input_tokens": 3,
  "output_tokens": 720,
  "cache_creation_input_tokens": 1593,
  "cache_read_input_tokens": 34582
}
```

Multiple assistant entries in the same round share the same `input_tokens` and `cache_*` values (they repeat per entry in the same API call), while `output_tokens` accumulates across entries.

## URL Routing

The current frontend supports deep linking:

- `/<sessionId>`: open session
- `/<sessionId>/<roundIdx>`: open and scroll to round
- `/<sessionId>/<roundIdx>/<blockIdx>`: open and scroll to block

Hovering a round/block header shows `#` anchor actions that copy the matching deep link.
