package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/algonous/agent-sessions/internal/data"
)

func TestViewConfigFromOptionsDiscoversDefaultRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	codexDir := filepath.Join(home, ".codex")
	mustMkdir(t, claudeDir)
	mustMkdir(t, codexDir)

	cfg, err := viewConfigFromOptions(viewOptions{addr: defaultViewAddr})
	if err != nil {
		t.Fatal(err)
	}

	want := []data.SourceRoot{
		{Source: data.SourceClaude, Dir: claudeDir},
		{Source: data.SourceCodex, Dir: codexDir},
	}
	if !sameRoots(cfg.Roots, want) {
		t.Fatalf("roots = %#v, want %#v", cfg.Roots, want)
	}
	if cfg.Addr != defaultViewAddr {
		t.Fatalf("addr = %q, want %q", cfg.Addr, defaultViewAddr)
	}
}

func TestViewConfigFromOptionsExpandsDataDirsAndDedupes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	codexDir := filepath.Join(home, "nested", ".codex")
	mustMkdir(t, claudeDir)
	mustMkdir(t, codexDir)

	cfg, err := viewConfigFromOptions(viewOptions{
		dataDirs: "~/.claude," + claudeDir + "," + codexDir,
		addr:     " 127.0.0.1:9999 ",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []data.SourceRoot{
		{Source: data.SourceClaude, Dir: claudeDir},
		{Source: data.SourceCodex, Dir: codexDir},
	}
	if !sameRoots(cfg.Roots, want) {
		t.Fatalf("roots = %#v, want %#v", cfg.Roots, want)
	}
	if cfg.Addr != "127.0.0.1:9999" {
		t.Fatalf("addr = %q", cfg.Addr)
	}
}

func TestSameViewConfigIgnoresRootOrder(t *testing.T) {
	a := viewConfig{
		Addr: defaultViewAddr,
		Roots: []data.SourceRoot{
			{Source: data.SourceCodex, Dir: "/tmp/.codex"},
			{Source: data.SourceClaude, Dir: "/tmp/.claude"},
		},
	}
	b := viewerServerInfo{
		Addr: defaultViewAddr,
		Roots: []data.SourceRoot{
			{Source: data.SourceClaude, Dir: "/tmp/.claude"},
			{Source: data.SourceCodex, Dir: "/tmp/.codex"},
		},
	}
	if !sameViewConfig(b, a) {
		t.Fatalf("sameViewConfig returned false")
	}
	b.Addr = "127.0.0.1:9000"
	if sameViewConfig(b, a) {
		t.Fatalf("sameViewConfig returned true for different addr")
	}
}

func TestViewConfigFromRootFlagsRejectsUnknownSource(t *testing.T) {
	if _, err := viewConfigFromRootFlags(defaultViewAddr, []string{"unknown=/tmp/x"}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestServeViewerDaemonRejectsHeldRunLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := newViewerStore()
	if err != nil {
		t.Fatal(err)
	}
	runLock, err := os.OpenFile(store.runLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer runLock.Close()
	if err := syscall.Flock(int(runLock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(runLock.Fd()), syscall.LOCK_UN)

	err = serveViewerDaemon(viewConfig{
		Addr:  defaultViewAddr,
		Roots: []data.SourceRoot{{Source: data.SourceClaude, Dir: filepath.Join(home, ".claude")}},
	})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("serveViewerDaemon error = %v, want already running", err)
	}
}

func TestEnsureViewerServiceKeepsRecordedServerWhenPreflightFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	mustMkdir(t, claudeDir)
	if err := os.WriteFile(filepath.Join(claudeDir, "history.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := newViewerStore()
	if err != nil {
		t.Fatal(err)
	}
	stale := viewerServerInfo{
		PID:             999999,
		ProtocolVersion: 0,
		WebURL:          "http://127.0.0.1:1",
		APIURL:          "http://127.0.0.1:1",
		Addr:            defaultViewAddr,
		Roots:           []data.SourceRoot{{Source: data.SourceClaude, Dir: claudeDir}},
	}
	if err := writeViewerServerInfo(store, stale); err != nil {
		t.Fatal(err)
	}

	_, err = ensureViewerService(viewOptions{claudeDir: claudeDir, addr: defaultViewAddr})
	if !errors.Is(err, errNoSessions) {
		t.Fatalf("ensureViewerService error = %v, want errNoSessions", err)
	}
	got, err := readViewerServerInfo(store)
	if err != nil {
		t.Fatalf("server.json was removed: %v", err)
	}
	if got.PID != stale.PID || got.APIURL != stale.APIURL {
		t.Fatalf("server.json changed: %#v", got)
	}
}

func TestWaitForViewerRunLockReleaseTimesOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := newViewerStore()
	if err != nil {
		t.Fatal(err)
	}
	runLock, err := os.OpenFile(store.runLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer runLock.Close()
	if err := syscall.Flock(int(runLock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(runLock.Fd()), syscall.LOCK_UN)

	err = waitForViewerRunLockRelease(store, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("waitForViewerRunLockRelease error = %v, want timeout", err)
	}
}

func sameRoots(got, want []data.SourceRoot) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
