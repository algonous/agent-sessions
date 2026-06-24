package data

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Claude history line.
type historyEntry struct {
	SessionID      string                   `json:"sessionId"`
	Timestamp      int64                    `json:"timestamp"`
	Project        string                   `json:"project"`
	Display        string                   `json:"display"`
	PastedContents map[string]pastedContent `json:"pastedContents,omitempty"`
}

type pastedContent struct {
	Content string `json:"content"`
}

// Codex history line.
type codexHistoryEntry struct {
	SessionID string `json:"session_id"`
	TS        int64  `json:"ts"`
	Text      string `json:"text"`
}

// LoadSessions keeps backward compatibility for single-root callers.
func LoadSessions(rootDir string) ([]SessionSummary, error) {
	source := sourceFromDir(rootDir)
	if source == "" {
		source = SourceClaude
	}
	return LoadSessionsMulti([]SourceRoot{{Source: source, Dir: rootDir}})
}

// LoadSessionsMulti loads and merges sessions from multiple roots.
func LoadSessionsMulti(roots []SourceRoot) ([]SessionSummary, error) {
	if len(roots) == 0 {
		return nil, nil
	}

	var all []SessionSummary
	for _, root := range roots {
		source := root.Source
		if source == "" {
			source = sourceFromDir(root.Dir)
		}
		if source == "" {
			continue
		}

		var sessions []SessionSummary
		var err error
		switch source {
		case SourceClaude:
			sessions, err = loadClaudeSessions(root.Dir)
		case SourceCodex:
			sessions, err = loadCodexSessions(root.Dir)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		all = append(all, sessions...)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].LastTS > all[j].LastTS })
	return all, nil
}

func loadCodexSessions(codexDir string) ([]SessionSummary, error) {
	path := filepath.Join(codexDir, "history.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	transcriptBySID := findCodexTranscriptPaths(codexDir)
	type group struct {
		s  *SessionSummary
		ms []string
	}
	groups := make(map[string]*group)
	var order []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var e codexHistoryEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil || e.SessionID == "" {
			continue
		}
		g, ok := groups[e.SessionID]
		if !ok {
			rawID := e.SessionID
			ts := normalizeEpochMillis(e.TS)
			s := SessionSummary{
				SessionID:    MakeSessionKey(SourceCodex, rawID),
				RawSessionID: rawID,
				Source:       SourceCodex,
				DataDir:      codexDir,
				FirstMessage: e.Text,
				FirstTS:      ts,
				LastTS:       ts,
				FilePath:     transcriptBySID[rawID],
			}
			if s.FilePath != "" {
				s.Project = codexProjectFromTranscript(s.FilePath)
				s.ProjectName = projectName(s.Project)
			}
			if s.ProjectName == "" {
				s.ProjectName = SourceCodex
			}
			g = &group{s: &s}
			groups[e.SessionID] = g
			order = append(order, e.SessionID)
		}
		g.s.MessageCount++
		if e.Text != "" {
			g.ms = append(g.ms, e.Text)
		}
		ts := normalizeEpochMillis(e.TS)
		if ts < g.s.FirstTS {
			g.s.FirstTS = ts
			g.s.FirstMessage = e.Text
		}
		if ts > g.s.LastTS {
			g.s.LastTS = ts
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	out := make([]SessionSummary, 0, len(order))
	for _, sid := range order {
		g := groups[sid]
		g.s.AllMessages = strings.Join(g.ms, "\n")
		out = append(out, *g.s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, nil
}

func findCodexTranscriptPaths(codexDir string) map[string]string {
	base := filepath.Join(codexDir, "sessions")
	result := map[string]string{}

	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		parts := strings.Split(name, "-")
		if len(parts) < 2 {
			return nil
		}
		sid := strings.Join(parts[len(parts)-5:], "-")
		if prev, ok := result[sid]; ok {
			prevInfo, _ := os.Stat(prev)
			newInfo, _ := os.Stat(path)
			if prevInfo != nil && newInfo != nil && prevInfo.ModTime().After(newInfo.ModTime()) {
				return nil
			}
		}
		result[sid] = path
		return nil
	})
	return result
}

func codexProjectFromTranscript(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var entry struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry.Type == "session_meta" && entry.Payload.Cwd != "" {
			return entry.Payload.Cwd
		}
	}
	return ""
}

func loadClaudeSessions(claudeDir string) ([]SessionSummary, error) {
	groups := make(map[string]*SessionSummary)
	messages := make(map[string][]string)
	var order []string

	path := filepath.Join(claudeDir, "history.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var e historyEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil || e.SessionID == "" {
			continue
		}
		rawID := e.SessionID
		s, ok := groups[rawID]
		if !ok {
			s = &SessionSummary{
				SessionID:    MakeSessionKey(SourceClaude, rawID),
				RawSessionID: rawID,
				Source:       SourceClaude,
				DataDir:      claudeDir,
				Project:      e.Project,
				ProjectName:  projectName(e.Project),
				FirstMessage: e.Display,
				FirstTS:      e.Timestamp,
				LastTS:       e.Timestamp,
			}
			groups[rawID] = s
			order = append(order, rawID)
		}
		s.MessageCount++
		if e.Display != "" && e.Display != "exit" {
			messages[rawID] = append(messages[rawID], e.Display)
		}
		for _, pc := range e.PastedContents {
			if pc.Content != "" {
				messages[rawID] = append(messages[rawID], pc.Content)
			}
		}
		if e.Timestamp < s.FirstTS {
			s.FirstTS = e.Timestamp
			s.FirstMessage = e.Display
		}
		if e.Timestamp > s.LastTS {
			s.LastTS = e.Timestamp
		}
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	discoverClaudeOrphans(claudeDir, groups, messages, &order)
	indexClaudeTranscriptText(claudeDir, groups, messages)

	result := make([]SessionSummary, 0, len(groups))
	for _, rawID := range order {
		s := groups[rawID]
		s.AllMessages = strings.Join(messages[rawID], "\n")
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LastTS > result[j].LastTS })
	return result, nil
}

func discoverClaudeOrphans(claudeDir string, groups map[string]*SessionSummary, messages map[string][]string, order *[]string) {
	projectsDir := filepath.Join(claudeDir, "projects")
	projEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}

	for _, projEntry := range projEntries {
		if !projEntry.IsDir() {
			continue
		}
		projPath := filepath.Join(projectsDir, projEntry.Name())
		files, err := os.ReadDir(projPath)
		if err != nil {
			continue
		}
		for _, file := range files {
			name := file.Name()
			if file.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") {
				continue
			}
			rawID := strings.TrimSuffix(name, ".jsonl")
			if _, ok := groups[rawID]; ok {
				continue
			}
			s := orphanClaudeSessionFromTranscript(filepath.Join(projPath, name), rawID, projEntry.Name(), claudeDir)
			if s == nil {
				continue
			}
			groups[rawID] = s
			messages[rawID] = []string{s.FirstMessage}
			*order = append(*order, rawID)
		}
	}
}

func orphanClaudeSessionFromTranscript(path, rawID, encodedProject, claudeDir string) *SessionSummary {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, _ := f.Stat()

	project := strings.ReplaceAll(encodedProject, "-", "/")
	s := &SessionSummary{
		SessionID:    MakeSessionKey(SourceClaude, rawID),
		RawSessionID: rawID,
		Source:       SourceClaude,
		DataDir:      claudeDir,
		Project:      project,
		ProjectName:  projectName(project),
		FilePath:     path,
	}
	if info != nil {
		s.LastTS = info.ModTime().UnixMilli()
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var entry transcriptEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry.Type != "user" || entry.Message == nil {
			if entry.Timestamp != "" && s.FirstTS == 0 {
				if t, err := parseTimestamp(entry.Timestamp); err == nil {
					s.FirstTS = t
				}
			}
			continue
		}
		var msg transcriptMessage
		if json.Unmarshal(entry.Message, &msg) != nil {
			continue
		}
		content := strings.TrimSpace(string(msg.Content))
		if len(content) > 0 && content[0] == '"' {
			var text string
			if json.Unmarshal(msg.Content, &text) == nil && text != "" {
				s.FirstMessage = text
				if s.FirstTS == 0 {
					if t, err := parseTimestamp(entry.Timestamp); err == nil {
						s.FirstTS = t
					}
				}
				s.MessageCount = 1
				return s
			}
		}
	}
	if s.FirstTS == 0 {
		s.FirstTS = s.LastTS
	}
	return s
}

func indexClaudeTranscriptText(claudeDir string, groups map[string]*SessionSummary, messages map[string][]string) {
	projectsDir := filepath.Join(claudeDir, "projects")
	projEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}

	type fileJob struct {
		rawID string
		path  string
	}
	var jobs []fileJob

	for _, projEntry := range projEntries {
		if !projEntry.IsDir() {
			continue
		}
		projPath := filepath.Join(projectsDir, projEntry.Name())
		files, err := os.ReadDir(projPath)
		if err != nil {
			continue
		}
		for _, file := range files {
			name := file.Name()
			if file.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") {
				continue
			}
			rawID := strings.TrimSuffix(name, ".jsonl")
			if _, ok := groups[rawID]; !ok {
				continue
			}
			p := filepath.Join(projPath, name)
			if groups[rawID].FilePath == "" {
				groups[rawID].FilePath = p
			}
			jobs = append(jobs, fileJob{rawID: rawID, path: p})
		}
	}

	type indexResult struct {
		rawID string
		texts []string
	}
	results := make(chan indexResult, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())

	for _, job := range jobs {
		wg.Add(1)
		go func(j fileJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			texts := extractClaudeTranscriptTexts(j.path)
			if len(texts) > 0 {
				results <- indexResult{rawID: j.rawID, texts: texts}
			}
		}(job)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	for r := range results {
		messages[r.rawID] = append(messages[r.rawID], r.texts...)
	}
}

func extractClaudeTranscriptTexts(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var texts []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"type":"user"`)) && !bytes.Contains(line, []byte(`"type":"assistant"`)) {
			continue
		}
		var entry transcriptEntry
		if json.Unmarshal(line, &entry) != nil || entry.Message == nil {
			continue
		}
		switch entry.Type {
		case "user":
			var msg transcriptMessage
			if json.Unmarshal(entry.Message, &msg) != nil {
				continue
			}
			content := strings.TrimSpace(string(msg.Content))
			if len(content) > 0 && content[0] == '"' {
				var text string
				if json.Unmarshal(msg.Content, &text) == nil && text != "" {
					texts = append(texts, text)
				}
			}
		case "assistant":
			var msg transcriptMessage
			if json.Unmarshal(entry.Message, &msg) != nil {
				continue
			}
			content := strings.TrimSpace(string(msg.Content))
			if len(content) > 0 && content[0] == '[' {
				var blocks []contentBlock
				if json.Unmarshal(msg.Content, &blocks) == nil {
					for _, b := range blocks {
						if b.Type == "text" && b.Text != "" {
							texts = append(texts, b.Text)
						}
					}
				}
			}
		}
	}
	return texts
}

func parseTimestamp(ts string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", ts)
		if err != nil {
			return 0, err
		}
	}
	return t.UnixMilli(), nil
}

func normalizeEpochMillis(ts int64) int64 {
	// Convert unix seconds to milliseconds. Keep true millisecond epochs unchanged.
	if ts >= 1_000_000_000 && ts < 1_000_000_000_000 {
		return ts * 1000
	}
	return ts
}

func projectName(project string) string {
	if project == "" {
		return ""
	}
	return filepath.Base(strings.TrimRight(project, "/"))
}

// SessionUpdate is produced by ParseHistoryLine for a single history line.
type SessionUpdate struct {
	SessionID    string
	RawSessionID string
	Source       string
	DataDir      string
	Project      string
	ProjectName  string
	Display      string
	Timestamp    int64
}

// ParseHistoryLineForSource parses a single history line based on source.
func ParseHistoryLineForSource(line []byte, source, rootDir string) *SessionUpdate {
	switch source {
	case SourceClaude:
		var e historyEntry
		if err := json.Unmarshal(line, &e); err != nil || e.SessionID == "" {
			return nil
		}
		return &SessionUpdate{
			SessionID:    MakeSessionKey(SourceClaude, e.SessionID),
			RawSessionID: e.SessionID,
			Source:       SourceClaude,
			DataDir:      rootDir,
			Project:      e.Project,
			ProjectName:  projectName(e.Project),
			Display:      e.Display,
			Timestamp:    normalizeEpochMillis(e.Timestamp),
		}
	case SourceCodex:
		var e codexHistoryEntry
		if err := json.Unmarshal(line, &e); err != nil || e.SessionID == "" {
			return nil
		}
		return &SessionUpdate{
			SessionID:    MakeSessionKey(SourceCodex, e.SessionID),
			RawSessionID: e.SessionID,
			Source:       SourceCodex,
			DataDir:      rootDir,
			Display:      e.Text,
			Timestamp:    normalizeEpochMillis(e.TS),
		}
	default:
		return nil
	}
}

// ParseHistoryLine keeps backward compatibility with Claude-only callers.
func ParseHistoryLine(line []byte) *SessionUpdate {
	return ParseHistoryLineForSource(line, SourceClaude, "")
}

func FindTranscriptPath(claudeDir string, rawSessionID string) (string, error) {
	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", err
	}
	filename := rawSessionID + ".jsonl"
	var matches []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, entry.Name(), filename)
		if _, err := os.Stat(candidate); err == nil {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return "", os.ErrNotExist
	case 1:
		return matches[0], nil
	default:
		// The same session id can appear in multiple project dirs after a
		// `claude --resume <id>` from a different cwd. Refuse to pick one
		// arbitrarily; the caller should resolve by project instead.
		return "", fmt.Errorf("ambiguous Claude transcript for session %s: %d matches (%s)", rawSessionID, len(matches), strings.Join(matches, ", "))
	}
}

func EncodeProjectDir(project string) string {
	return ClaudeProjectDirName(project)
}

func ClaudeProjectDirName(project string) string {
	var b strings.Builder
	for _, r := range project {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func ResolveTranscriptPath(s SessionSummary) (string, error) {
	switch s.Source {
	case SourceClaude:
		// Prefer the transcript under the session's own project. The same raw
		// session id can have fragments in multiple project dirs after a
		// `claude --resume <id>` from a different cwd; a pre-resolved FilePath
		// (or an arbitrary FindTranscriptPath match) may point at the wrong one.
		if s.Project != "" {
			candidate := filepath.Join(s.DataDir, "projects", ClaudeProjectDirName(s.Project), s.RawSessionID+".jsonl")
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
		if s.FilePath != "" {
			return s.FilePath, nil
		}
		return FindTranscriptPath(s.DataDir, s.RawSessionID)
	case SourceCodex:
		if s.FilePath != "" {
			return s.FilePath, nil
		}
		if m := findCodexTranscriptPaths(s.DataDir); m != nil {
			if p, ok := m[s.RawSessionID]; ok {
				return p, nil
			}
		}
		return "", os.ErrNotExist
	default:
		return "", fmt.Errorf("unknown source: %s", s.Source)
	}
}
