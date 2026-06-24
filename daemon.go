package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/algonous/agent-sessions/internal/data"
	"github.com/algonous/agent-sessions/internal/server"
)

const (
	viewerProtocolVersion = 1
	viewerServeEnv        = "AGENT_SESSIONS_SERVE_INTERNAL"
	defaultViewAddr       = "127.0.0.1:0"
	configDirRel          = ".config/agent-sessions"
	serverJSONFile        = "server.json"
	serverLockFile        = "server.lock"
	runLockFile           = "server.run.lock"
	serverLogFile         = "server.log"
)

var errNoSessions = errors.New("no sessions found")

type viewOptions struct {
	claudeDir  string
	dataDirs   string
	addr       string
	foreground bool
}

type viewConfig struct {
	Roots []data.SourceRoot
	Addr  string
}

type viewerStore struct {
	configRoot string
}

type viewerServerInfo struct {
	PID             int               `json:"pid"`
	ProtocolVersion int               `json:"protocol_version"`
	WebURL          string            `json:"web_url"`
	APIURL          string            `json:"api_url"`
	StartedAt       string            `json:"started_at"`
	Addr            string            `json:"addr"`
	Roots           []data.SourceRoot `json:"roots"`
	SessionCount    int               `json:"session_count"`
}

type viewerInfoResponse struct {
	PID             int               `json:"pid"`
	ProtocolVersion int               `json:"protocol_version"`
	WebURL          string            `json:"web_url"`
	APIURL          string            `json:"api_url"`
	StartedAt       string            `json:"started_at"`
	Addr            string            `json:"addr"`
	Roots           []data.SourceRoot `json:"roots"`
	SessionCount    int               `json:"session_count"`
	ConfigDir       string            `json:"config_dir"`
	ServerJSON      string            `json:"server_json"`
	ServerLock      string            `json:"server_lock"`
	RunLock         string            `json:"run_lock"`
	ServerLog       string            `json:"server_log"`
}

type stringList []string

func (l *stringList) String() string {
	return strings.Join(*l, ",")
}

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func runView(args []string) {
	opts := parseViewFlags("view", args, true)
	if opts.foreground {
		if err := runViewForeground(opts); err != nil {
			exitCommand(err)
		}
		return
	}
	info, err := ensureViewerService(opts)
	if err != nil {
		if errors.Is(err, errNoSessions) {
			fmt.Println("No sessions found.")
			return
		}
		exitCommand(err)
	}
	fmt.Println(info.WebURL)
}

func runOpen(args []string) {
	opts := parseViewFlags("open", args, false)
	if opts.foreground {
		exitCommand(errors.New("--foreground is only supported by view"))
	}
	info, err := ensureViewerService(opts)
	if err != nil {
		if errors.Is(err, errNoSessions) {
			fmt.Println("No sessions found.")
			return
		}
		exitCommand(err)
	}
	if err := openBrowser(info.WebURL); err != nil {
		exitCommand(err)
	}
	fmt.Println(info.WebURL)
}

func runStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 0 {
		exitCommand(errors.New("usage: agent-sessions stop"))
	}

	store, err := newViewerStore()
	if err != nil {
		exitCommand(err)
	}
	lock, err := lockViewerServer(store)
	if err != nil {
		exitCommand(err)
	}
	defer lock.Close()

	if stopRecordedViewerServer(store) {
		fmt.Println("stopped")
	} else {
		fmt.Println("no live service matching the recorded metadata; cleared")
	}
	_ = os.Remove(store.serverPath())
}

func runInfo(args []string) {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 0 {
		exitCommand(errors.New("usage: agent-sessions info"))
	}

	store, err := newViewerStore()
	if err != nil {
		exitCommand(err)
	}
	fileInfo, err := readViewerServerInfo(store)
	if err != nil {
		exitCommand(errors.New("no recorded viewer service"))
	}
	live, ok := probeViewerStatus(fileInfo.APIURL)
	if !ok || live.PID != fileInfo.PID {
		exitCommand(errors.New("no live viewer service matching the recorded metadata"))
	}
	writeJSONToStdout(infoResponse(store, live))
}

func runServe(args []string) {
	if os.Getenv(viewerServeEnv) != "1" {
		exitCommand(errors.New("serve is internal; run agent-sessions to start the viewer"))
	}
	var roots stringList
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", defaultViewAddr, "listen address")
	fs.Var(&roots, "root", "internal source=dir root")
	fs.Parse(args)
	if fs.NArg() != 0 {
		exitCommand(errors.New("usage: agent-sessions serve"))
	}
	cfg, err := viewConfigFromRootFlags(*addr, []string(roots))
	if err != nil {
		exitCommand(err)
	}
	if err := serveViewerDaemon(cfg); err != nil {
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		exitCommand(err)
	}
}

func parseViewFlags(name string, args []string, allowForeground bool) viewOptions {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	opts := viewOptions{}
	fs.StringVar(&opts.claudeDir, "claude-dir", "", "legacy single data directory (for compatibility)")
	fs.StringVar(&opts.dataDirs, "data-dirs", "", "comma-separated data roots; if omitted, auto-discovers all supported agent roots under $HOME")
	fs.StringVar(&opts.addr, "addr", defaultViewAddr, "listen address (host:port, use port 0 for auto)")
	if allowForeground {
		fs.BoolVar(&opts.foreground, "foreground", false, "run the HTTP server in the foreground instead of daemon mode")
	}
	fs.Parse(args)
	if fs.NArg() != 0 {
		exitCommand(fmt.Errorf("usage: agent-sessions %s [view options]", name))
	}
	if opts.addr == "" {
		opts.addr = defaultViewAddr
	}
	return opts
}

func runViewForeground(opts viewOptions) error {
	cfg, err := viewConfigFromOptions(opts)
	if err != nil {
		return err
	}
	srv, sessionCount, err := loadViewServer(cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String()
	store, err := newViewerStore()
	if err != nil {
		ln.Close()
		return err
	}
	info := viewerServerInfo{
		PID:             os.Getpid(),
		ProtocolVersion: viewerProtocolVersion,
		WebURL:          url,
		APIURL:          url,
		StartedAt:       time.Now().UTC().Format(time.RFC3339),
		Addr:            cfg.Addr,
		Roots:           cfg.Roots,
		SessionCount:    sessionCount,
	}
	fmt.Printf("agent-sessions serving %d sessions at %s\n", sessionCount, url)
	return http.Serve(ln, viewerHandler(srv.Handler(), store, info))
}

func ensureViewerService(opts viewOptions) (viewerServerInfo, error) {
	cfg, err := viewConfigFromOptions(opts)
	if err != nil {
		return viewerServerInfo{}, err
	}
	store, err := newViewerStore()
	if err != nil {
		return viewerServerInfo{}, err
	}
	lock, err := lockViewerServer(store)
	if err != nil {
		return viewerServerInfo{}, err
	}
	defer lock.Close()

	if info, ok, err := readLiveViewerServerInfo(store, cfg); ok || err != nil {
		return info, err
	}

	if _, err := loadViewSessions(cfg.Roots); err != nil {
		return viewerServerInfo{}, err
	}
	if stopRecordedViewerServer(store) {
		if err := waitForViewerRunLockRelease(store, 3*time.Second); err != nil {
			return viewerServerInfo{}, err
		}
	}
	if err := os.Remove(store.serverPath()); err != nil && !os.IsNotExist(err) {
		return viewerServerInfo{}, err
	}

	exe, err := os.Executable()
	if err != nil {
		return viewerServerInfo{}, err
	}
	logFile, err := os.OpenFile(store.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return viewerServerInfo{}, err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, serveArgs(cfg)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), viewerServeEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return viewerServerInfo{}, err
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if info, ok, err := readLiveViewerServerInfo(store, cfg); ok || err != nil {
			return info, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return viewerServerInfo{}, fmt.Errorf("agent-sessions service did not start within 10s; it may still be starting, retry agent-sessions or see %s", store.logPath())
}

func serveViewerDaemon(cfg viewConfig) error {
	store, err := newViewerStore()
	if err != nil {
		return err
	}
	runLock, err := os.OpenFile(store.runLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(runLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = runLock.Close()
		if isLockBusy(err) {
			return errors.New("another agent-sessions viewer daemon is already running")
		}
		return err
	}

	srv, sessionCount, err := loadViewServer(cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String()
	info := viewerServerInfo{
		PID:             os.Getpid(),
		ProtocolVersion: viewerProtocolVersion,
		WebURL:          url,
		APIURL:          url,
		StartedAt:       time.Now().UTC().Format(time.RFC3339),
		Addr:            cfg.Addr,
		Roots:           cfg.Roots,
		SessionCount:    sessionCount,
	}
	if err := writeViewerServerInfo(store, info); err != nil {
		ln.Close()
		return err
	}
	fmt.Printf("agent-sessions serving %d sessions at %s\n", sessionCount, url)
	httpSrv := &http.Server{Handler: viewerHandler(srv.Handler(), store, info)}
	done := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-signals:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
		case <-done:
		}
	}()
	err = httpSrv.Serve(ln)
	close(done)
	signal.Stop(signals)
	removeViewerServerInfoIfMatching(store, info)
	runtime.KeepAlive(runLock)
	return err
}

func loadViewServer(cfg viewConfig) (*server.Server, int, error) {
	sessions, err := loadViewSessions(cfg.Roots)
	if err != nil {
		return nil, 0, err
	}
	srv := server.New(cfg.Roots, sessions, webFS)
	srv.StartHistoryTail()
	return srv, len(sessions), nil
}

func loadViewSessions(roots []data.SourceRoot) ([]data.SessionSummary, error) {
	sessions, err := data.LoadSessionsMulti(roots)
	if err != nil {
		return nil, fmt.Errorf("loading sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, errNoSessions
	}
	return sessions, nil
}

func viewerHandler(app http.Handler, store viewerStore, info viewerServerInfo) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, info)
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, infoResponse(store, info))
	})
	mux.Handle("/", app)
	return mux
}

func infoResponse(store viewerStore, info viewerServerInfo) viewerInfoResponse {
	return viewerInfoResponse{
		PID:             info.PID,
		ProtocolVersion: info.ProtocolVersion,
		WebURL:          info.WebURL,
		APIURL:          info.APIURL,
		StartedAt:       info.StartedAt,
		Addr:            info.Addr,
		Roots:           info.Roots,
		SessionCount:    info.SessionCount,
		ConfigDir:       store.configRoot,
		ServerJSON:      store.serverPath(),
		ServerLock:      store.lockPath(),
		RunLock:         store.runLockPath(),
		ServerLog:       store.logPath(),
	}
}

func serveArgs(cfg viewConfig) []string {
	args := []string{"serve", "--addr", cfg.Addr}
	for _, root := range cfg.Roots {
		args = append(args, "--root", root.Source+"="+root.Dir)
	}
	return args
}

func viewConfigFromOptions(opts viewOptions) (viewConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return viewConfig{}, err
	}
	roots := make([]data.SourceRoot, 0, 4)
	if strings.TrimSpace(opts.claudeDir) != "" {
		dir, err := normalizePath(home, opts.claudeDir)
		if err != nil {
			return viewConfig{}, err
		}
		source := data.DetectSourceFromDir(dir)
		if source == "" {
			source = data.SourceClaude
		}
		roots = append(roots, data.SourceRoot{Source: source, Dir: dir})
	} else if strings.TrimSpace(opts.dataDirs) != "" {
		for _, raw := range strings.Split(opts.dataDirs, ",") {
			dir := strings.TrimSpace(raw)
			if dir == "" {
				continue
			}
			normalized, err := normalizePath(home, dir)
			if err != nil {
				return viewConfig{}, err
			}
			source := data.DetectSourceFromDir(normalized)
			if source != "" {
				roots = append(roots, data.SourceRoot{Source: source, Dir: normalized})
			}
		}
	} else {
		roots = data.DiscoverDefaultRoots(home)
		for i := range roots {
			roots[i].Dir, err = normalizePath(home, roots[i].Dir)
			if err != nil {
				return viewConfig{}, err
			}
		}
	}
	return viewConfig{Roots: canonicalRoots(roots), Addr: normalizedAddr(opts.addr)}, nil
}

func viewConfigFromRootFlags(addr string, values []string) (viewConfig, error) {
	roots := make([]data.SourceRoot, 0, len(values))
	for _, value := range values {
		source, dir, ok := strings.Cut(value, "=")
		if !ok || source == "" || dir == "" {
			return viewConfig{}, fmt.Errorf("invalid root %q", value)
		}
		if _, ok := data.AdapterBySource(source); !ok {
			return viewConfig{}, fmt.Errorf("unsupported source %q", source)
		}
		roots = append(roots, data.SourceRoot{Source: source, Dir: filepath.Clean(dir)})
	}
	if len(roots) == 0 {
		return viewConfig{}, errors.New("serve requires at least one --root")
	}
	return viewConfig{Roots: canonicalRoots(roots), Addr: normalizedAddr(addr)}, nil
}

func canonicalRoots(roots []data.SourceRoot) []data.SourceRoot {
	out := make([]data.SourceRoot, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root.Source == "" || root.Dir == "" {
			continue
		}
		clean := data.SourceRoot{Source: root.Source, Dir: filepath.Clean(root.Dir)}
		key := clean.Source + "\x00" + clean.Dir
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clean)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

func normalizePath(home, raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = abs
	}
	return filepath.Clean(path), nil
}

func normalizedAddr(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return defaultViewAddr
	}
	return strings.TrimSpace(addr)
}

func sameViewConfig(info viewerServerInfo, cfg viewConfig) bool {
	if normalizedAddr(info.Addr) != normalizedAddr(cfg.Addr) {
		return false
	}
	infoRoots := canonicalRoots(info.Roots)
	cfgRoots := canonicalRoots(cfg.Roots)
	if len(infoRoots) != len(cfgRoots) {
		return false
	}
	for i := range infoRoots {
		if infoRoots[i] != cfgRoots[i] {
			return false
		}
	}
	return true
}

func newViewerStore() (viewerStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return viewerStore{}, err
	}
	store := viewerStore{configRoot: filepath.Join(home, configDirRel)}
	if err := os.MkdirAll(store.configRoot, 0o755); err != nil {
		return viewerStore{}, err
	}
	return store, nil
}

func (s viewerStore) serverPath() string {
	return filepath.Join(s.configRoot, serverJSONFile)
}

func (s viewerStore) lockPath() string {
	return filepath.Join(s.configRoot, serverLockFile)
}

func (s viewerStore) runLockPath() string {
	return filepath.Join(s.configRoot, runLockFile)
}

func (s viewerStore) logPath() string {
	return filepath.Join(s.configRoot, serverLogFile)
}

type viewerServerLock struct {
	file *os.File
}

func lockViewerServer(store viewerStore) (*viewerServerLock, error) {
	f, err := os.OpenFile(store.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &viewerServerLock{file: f}, nil
}

func (l *viewerServerLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func readViewerServerInfo(store viewerStore) (viewerServerInfo, error) {
	var info viewerServerInfo
	b, err := os.ReadFile(store.serverPath())
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(b, &info)
	return info, err
}

func writeViewerServerInfo(store viewerStore, info viewerServerInfo) error {
	if err := os.MkdirAll(store.configRoot, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(store.configRoot, serverJSONFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, store.serverPath()); err != nil {
		return err
	}
	committed = true
	return nil
}

func removeViewerServerInfoIfMatching(store viewerStore, info viewerServerInfo) {
	recorded, err := readViewerServerInfo(store)
	if err != nil {
		return
	}
	if recorded.PID != info.PID || recorded.APIURL != info.APIURL {
		return
	}
	_ = os.Remove(store.serverPath())
}

func readLiveViewerServerInfo(store viewerStore, cfg viewConfig) (viewerServerInfo, bool, error) {
	fileInfo, err := readViewerServerInfo(store)
	if err != nil {
		return viewerServerInfo{}, false, nil
	}
	live, ok := probeViewerStatus(fileInfo.APIURL)
	if !ok || live.PID != fileInfo.PID || live.ProtocolVersion != viewerProtocolVersion {
		return viewerServerInfo{}, false, nil
	}
	if !sameViewConfig(live, cfg) {
		return viewerServerInfo{}, false, fmt.Errorf("viewer already running at %s with different roots or addr; run agent-sessions stop first", live.WebURL)
	}
	return live, true, nil
}

func stopRecordedViewerServer(store viewerStore) bool {
	info, err := readViewerServerInfo(store)
	if err != nil || info.PID <= 0 {
		return false
	}
	live, ok := probeViewerStatus(info.APIURL)
	if !ok || live.PID != info.PID {
		return false
	}
	p, err := os.FindProcess(info.PID)
	if err != nil {
		return false
	}
	return p.Signal(syscall.SIGTERM) == nil
}

func waitForViewerRunLockRelease(store viewerStore, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		released, err := tryViewerRunLock(store)
		if err != nil {
			return err
		}
		if released {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("previous agent-sessions viewer did not stop within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func tryViewerRunLock(store viewerStore) (bool, error) {
	f, err := os.OpenFile(store.runLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if isLockBusy(err) {
			return false, nil
		}
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return false, err
	}
	return true, nil
}

func isLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

func probeViewerStatus(apiURL string) (viewerServerInfo, bool) {
	if apiURL == "" {
		return viewerServerInfo{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+"/api/status", nil)
	if err != nil {
		return viewerServerInfo{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return viewerServerInfo{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return viewerServerInfo{}, false
	}
	var info viewerServerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return viewerServerInfo{}, false
	}
	return info, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONToStdout(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func openBrowser(url string) error {
	name, args, err := browserOpenCommand(url)
	if err != nil {
		return err
	}
	return exec.Command(name, args...).Start()
}

func browserOpenCommand(url string) (string, []string, error) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}, nil
	case "linux":
		return "xdg-open", []string{url}, nil
	default:
		return "", nil, fmt.Errorf("opening browser is unsupported on %s", runtime.GOOS)
	}
}

func exitCommand(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
