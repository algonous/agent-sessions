package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/algonous/agent-sessions/internal/data"
	"github.com/algonous/agent-sessions/internal/publish"
	"github.com/algonous/md2html/md2html"
)

// Server handles HTTP requests for the agent-sessions web UI.
type Server struct {
	roots []data.SourceRoot
	webFS fs.FS

	mu       sync.RWMutex
	sessions []data.SessionSummary

	// Broadcast: closed and replaced on each session update.
	broadcastMu sync.Mutex
	broadcast   chan struct{}

	historyStop  chan struct{}
	historyStops []chan struct{}
}

// New creates a new Server.
func New(roots []data.SourceRoot, sessions []data.SessionSummary, webFS fs.FS) *Server {
	return &Server{
		roots:     roots,
		sessions:  sessions,
		webFS:     webFS,
		broadcast: make(chan struct{}),
	}
}

// StartHistoryTail begins tailing history.jsonl for new session events.
// Existing content was already loaded at startup; this tails from the end.
func (s *Server) StartHistoryTail() {
	s.historyStop = make(chan struct{})
	for _, root := range s.roots {
		historyPath := root.Dir + "/history.jsonl"
		offset, _ := data.FileSize(historyPath)
		stop := make(chan struct{})
		s.historyStops = append(s.historyStops, stop)
		lines := data.TailFile(historyPath, offset, stop)
		go func(source, dir string, ch <-chan []byte) {
			for line := range ch {
				s.processHistoryLine(line, source, dir)
			}
		}(root.Source, root.Dir, lines)
	}
}

// StopHistoryTail stops the history tailing goroutine.
func (s *Server) StopHistoryTail() {
	if s.historyStop != nil {
		close(s.historyStop)
	}
	for _, ch := range s.historyStops {
		close(ch)
	}
	s.historyStops = nil
}

// processHistoryLine handles a single new line from history.jsonl.
func (s *Server) processHistoryLine(line []byte, source, rootDir string) {
	update := data.ParseHistoryLineForSource(line, source, rootDir)
	if update == nil {
		return
	}

	s.mu.Lock()
	found := false
	for i := range s.sessions {
		if s.sessions[i].SessionID == update.SessionID {
			if update.Timestamp > s.sessions[i].LastTS {
				s.sessions[i].LastTS = update.Timestamp
			}
			s.sessions[i].MessageCount++
			found = true
			break
		}
	}
	if !found {
		s.sessions = append(s.sessions, data.SessionSummary{
			SessionID:    update.SessionID,
			RawSessionID: update.RawSessionID,
			Source:       update.Source,
			DataDir:      update.DataDir,
			Project:      update.Project,
			ProjectName:  defaultProjectName(update.Source, update.ProjectName),
			FirstMessage: update.Display,
			FirstTS:      update.Timestamp,
			LastTS:       update.Timestamp,
			MessageCount: 1,
		})
	}
	sort.Slice(s.sessions, func(i, j int) bool {
		return s.sessions[i].LastTS > s.sessions[j].LastTS
	})
	s.mu.Unlock()

	// Wake all session stream subscribers.
	s.broadcastMu.Lock()
	ch := s.broadcast
	s.broadcast = make(chan struct{})
	s.broadcastMu.Unlock()
	close(ch)
}

func defaultProjectName(source, current string) string {
	if current != "" {
		return current
	}
	return source
}

// waitSessionUpdate returns a channel that closes on the next session update.
func (s *Server) waitSessionUpdate() <-chan struct{} {
	s.broadcastMu.Lock()
	ch := s.broadcast
	s.broadcastMu.Unlock()
	return ch
}

// Handler returns the HTTP handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Embedded static files.
	staticFS, _ := fs.Sub(s.webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// SPA fallback: serve index.html for all non-API, non-static GET requests.
	indexHTML, _ := fs.ReadFile(s.webFS, "web/index.html")
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	// REST API endpoints.
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/sessions/{id}/path", s.handleSessionPath)
	mux.HandleFunc("GET /api/transcript/{id}", s.handleTranscript)
	mux.HandleFunc("POST /api/export", s.handleExport)
	mux.HandleFunc("POST /api/publish", s.handlePublish)

	// SSE streaming endpoints.
	mux.HandleFunc("GET /api/sessions/stream", s.handleSessionStream)
	mux.HandleFunc("GET /api/transcript/{id}/stream", s.handleTranscriptStream)

	return mux
}

// --- JSON response types ---

type sessionJSON struct {
	SessionID    string `json:"session_id"`
	Source       string `json:"source"`
	Project      string `json:"project"`
	ProjectName  string `json:"project_name"`
	FilePath     string `json:"file_path,omitempty"`
	FirstMessage string `json:"first_message"`
	AllMessages  string `json:"all_messages"`
	FirstTS      int64  `json:"first_ts"`
	LastTS       int64  `json:"last_ts"`
	MessageCount int    `json:"message_count"`
}

type sessionPathJSON struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
}

type transcriptJSON struct {
	SessionID string      `json:"session_id"`
	Rounds    []roundJSON `json:"rounds"`
}

type roundJSON struct {
	Index         int         `json:"index"`
	UserTimestamp string      `json:"user_timestamp"`
	IsContext     bool        `json:"is_context"`
	Blocks        []blockJSON `json:"blocks"`
	Usage         usageJSON   `json:"usage"`
}

type blockJSON struct {
	Role         string `json:"role"`
	HTML         string `json:"html,omitempty"`
	Name         string `json:"name,omitempty"`
	InputSummary string `json:"input_summary,omitempty"`
	InputJSON    string `json:"input_json,omitempty"`
}

type usageJSON struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

type exportRequest struct {
	SessionID string  `json:"session_id"`
	Format    string  `json:"format"`
	Blocks    [][]int `json:"blocks,omitempty"`
}

type publishRequest struct {
	SessionID string  `json:"session_id"`
	Title     string  `json:"title"`
	Blocks    [][]int `json:"blocks,omitempty"`
}

// SSE event types.

type sseBlockEvent struct {
	RoundIndex    int    `json:"round_index"`
	NewRound      bool   `json:"new_round,omitempty"`
	IsContext     bool   `json:"is_context,omitempty"`
	UserTimestamp string `json:"user_timestamp,omitempty"`
	Role          string `json:"role"`
	HTML          string `json:"html,omitempty"`
	Name          string `json:"name,omitempty"`
	InputSummary  string `json:"input_summary,omitempty"`
	InputJSON     string `json:"input_json,omitempty"`
}

type sseUsageEvent struct {
	RoundIndex    int   `json:"round_index"`
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// --- REST Handlers ---

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	result := s.buildSessionList()
	s.mu.RUnlock()
	writeJSON(w, result)
}

func (s *Server) handleSessionPath(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := s.findSession(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	path, err := data.ResolveTranscriptPath(session)
	if err != nil {
		http.Error(w, "transcript not found", http.StatusNotFound)
		return
	}

	// Cache resolved path for later session list payloads.
	s.mu.Lock()
	for i := range s.sessions {
		if s.sessions[i].SessionID == id {
			s.sessions[i].FilePath = path
			break
		}
	}
	s.mu.Unlock()

	writeJSON(w, sessionPathJSON{
		SessionID: id,
		FilePath:  path,
	})
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := s.findSession(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	path, err := data.ResolveTranscriptPath(session)
	if err != nil {
		http.Error(w, "transcript not found", http.StatusNotFound)
		return
	}

	transcript, err := data.LoadTranscript(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rounds := make([]roundJSON, len(transcript.Rounds))
	for i, rd := range transcript.Rounds {
		blocks := make([]blockJSON, len(rd.Blocks))
		for j, b := range rd.Blocks {
			if b.Role == "tool" && b.ToolCall != nil {
				blocks[j] = blockJSON{
					Role:         "tool",
					Name:         b.ToolCall.Name,
					InputSummary: b.ToolCall.InputSummary,
					InputJSON:    b.ToolCall.InputJSON,
				}
			} else {
				blocks[j] = blockJSON{
					Role: b.Role,
					HTML: renderMarkdown(b.Text),
				}
			}
		}

		rounds[i] = roundJSON{
			Index:         rd.Index,
			UserTimestamp: rd.UserTimestamp,
			IsContext:     rd.IsContext,
			Blocks:        blocks,
			Usage: usageJSON{
				InputTokens:   rd.Usage.InputTokens,
				OutputTokens:  rd.Usage.OutputTokens,
				CacheRead:     rd.Usage.CacheRead,
				CacheCreation: rd.Usage.CacheCreation,
			},
		}
	}

	writeJSON(w, transcriptJSON{SessionID: id, Rounds: rounds})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var req exportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Find session.
	s.mu.RLock()
	var session data.SessionSummary
	found := false
	for _, sess := range s.sessions {
		if sess.SessionID == req.SessionID {
			session = sess
			found = true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Load transcript.
	path, err := data.ResolveTranscriptPath(session)
	if err != nil {
		http.Error(w, "transcript not found", http.StatusNotFound)
		return
	}
	transcript, err := data.LoadTranscript(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Convert request blocks from [][]int to [][2]int.
	var blocks [][2]int
	if len(req.Blocks) > 0 {
		blocks = make([][2]int, 0, len(req.Blocks))
		for _, pair := range req.Blocks {
			if len(pair) == 2 {
				blocks = append(blocks, [2]int{pair[0], pair[1]})
			}
		}
	}

	var content []byte
	var filename string
	var contentType string
	switch req.Format {
	case "md":
		content = data.GenerateMarkdown(session, transcript, blocks)
		filename = req.SessionID + ".md"
		contentType = "text/markdown; charset=utf-8"
	default:
		content = data.GenerateJSONL(session, transcript, blocks)
		filename = req.SessionID + ".jsonl"
		contentType = "application/x-ndjson"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Write(content)
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}

	// Find session.
	s.mu.RLock()
	var session data.SessionSummary
	found := false
	for _, sess := range s.sessions {
		if sess.SessionID == req.SessionID {
			session = sess
			found = true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Load transcript.
	path, err := data.ResolveTranscriptPath(session)
	if err != nil {
		http.Error(w, "transcript not found", http.StatusNotFound)
		return
	}
	transcript, err := data.LoadTranscript(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Convert request blocks from [][]int to [][2]int.
	var blocks [][2]int
	if len(req.Blocks) > 0 {
		blocks = make([][2]int, 0, len(req.Blocks))
		for _, pair := range req.Blocks {
			if len(pair) == 2 {
				blocks = append(blocks, [2]int{pair[0], pair[1]})
			}
		}
	}

	md := data.GenerateMarkdown(session, transcript, blocks)

	pub := &publish.GitLab{}
	result, err := pub.Publish(r.Context(), publish.Snippet{
		Title:    req.Title,
		Filename: req.SessionID + ".md",
		Content:  string(md),
	})
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "auth") || strings.Contains(errMsg, "401") {
			w.Header().Set("X-Publish-Error", "auth")
			http.Error(w, errMsg, http.StatusUnauthorized)
		} else {
			http.Error(w, errMsg, http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, map[string]string{"url": result.URL})
}

// --- SSE Handlers ---

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()

	// Send initial session list.
	s.sendSessionList(w, flusher)

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		updateCh := s.waitSessionUpdate()
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-updateCh:
			s.sendSessionList(w, flusher)
		}
	}
}

func (s *Server) sendSessionList(w http.ResponseWriter, flusher http.Flusher) {
	s.mu.RLock()
	result := s.buildSessionList()
	s.mu.RUnlock()

	jsonData, _ := json.Marshal(result)
	fmt.Fprintf(w, "event: sessions\ndata: %s\n\n", jsonData)
	flusher.Flush()
}

func (s *Server) buildSessionList() []sessionJSON {
	result := make([]sessionJSON, len(s.sessions))
	for i, sess := range s.sessions {
		result[i] = sessionJSON{
			SessionID:    sess.SessionID,
			Source:       sess.Source,
			Project:      sess.Project,
			ProjectName:  sess.ProjectName,
			FilePath:     sess.FilePath,
			FirstMessage: sess.FirstMessage,
			AllMessages:  sess.AllMessages,
			FirstTS:      sess.FirstTS,
			LastTS:       sess.LastTS,
			MessageCount: sess.MessageCount,
		}
	}
	return result
}

func (s *Server) handleTranscriptStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	id := r.PathValue("id")
	session, ok := s.findSession(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	path, err := data.ResolveTranscriptPath(session)
	if err != nil {
		http.Error(w, "transcript not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	stopCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()

	// Tail from beginning -- backlog + live.
	lines := data.TailFile(path, 0, stopCh)
	var claudeStreamer *data.TranscriptStreamer
	var codexStreamer *data.CodexTranscriptStreamer
	if session.Source == data.SourceCodex {
		codexStreamer = data.NewCodexTranscriptStreamer()
	} else {
		claudeStreamer = data.NewTranscriptStreamer()
	}

	// Per-round cumulative usage for aggregation.
	roundUsage := make(map[int]*sseUsageEvent)

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case line, ok := <-lines:
			if !ok {
				return
			}
			var ev *data.StreamEvent
			if codexStreamer != nil {
				ev = codexStreamer.ProcessLine(line)
			} else {
				ev = claudeStreamer.ProcessLine(line)
			}
			if ev == nil {
				continue
			}
			s.sendStreamEvent(w, flusher, ev, roundUsage)
		}
	}
}

func (s *Server) findSession(id string) (data.SessionSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.SessionID == id {
			return sess, true
		}
	}
	return data.SessionSummary{}, false
}

func (s *Server) sendStreamEvent(w http.ResponseWriter, flusher http.Flusher, ev *data.StreamEvent, roundUsage map[int]*sseUsageEvent) {
	// Send each block as a separate SSE event.
	for i, block := range ev.Blocks {
		sse := sseBlockEvent{
			RoundIndex: ev.RoundIndex,
			Role:       block.Role,
		}
		// Only the first block in a new round carries round metadata.
		if ev.NewRound && i == 0 {
			sse.NewRound = true
			sse.IsContext = ev.IsContext
			sse.UserTimestamp = ev.UserTimestamp
		}
		if block.Role == "tool" && block.ToolCall != nil {
			sse.Name = block.ToolCall.Name
			sse.InputSummary = block.ToolCall.InputSummary
			sse.InputJSON = block.ToolCall.InputJSON
		} else {
			sse.HTML = renderMarkdown(block.Text)
		}

		jsonData, _ := json.Marshal(sse)
		fmt.Fprintf(w, "event: block\ndata: %s\n\n", jsonData)
	}

	// Aggregate and send cumulative usage.
	if ev.Usage != nil {
		u, ok := roundUsage[ev.RoundIndex]
		if !ok {
			u = &sseUsageEvent{RoundIndex: ev.RoundIndex}
			roundUsage[ev.RoundIndex] = u
		}
		// output_tokens: summed; input/cache: max.
		u.OutputTokens += ev.Usage.OutputTokens
		if ev.Usage.InputTokens > u.InputTokens {
			u.InputTokens = ev.Usage.InputTokens
		}
		if ev.Usage.CacheCreation > u.CacheCreation {
			u.CacheCreation = ev.Usage.CacheCreation
		}
		if ev.Usage.CacheRead > u.CacheRead {
			u.CacheRead = ev.Usage.CacheRead
		}

		jsonData, _ := json.Marshal(u)
		fmt.Fprintf(w, "event: usage\ndata: %s\n\n", jsonData)
	}

	flusher.Flush()
}

// --- Rendering ---

// renderMarkdown converts a markdown string to HTML using the md2html pipeline.
func renderMarkdown(text string) string {
	if text == "" {
		return ""
	}
	ast, err := md2html.ParseMarkdownToAST(text)
	if err != nil {
		return htmlEscape(text)
	}
	ir := md2html.ASTToIR(ast)
	return md2html.IRToHTML(ir)
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}
