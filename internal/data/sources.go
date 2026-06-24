package data

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	SourceClaude = "claude"
	SourceCodex  = "codex"
)

// SourceRoot describes one history root and its source type.
type SourceRoot struct {
	Source string `json:"source"`
	Dir    string `json:"dir"`
}

type SourceAdapter struct {
	ID      string
	DirName string
	Display string
}

var sourceAdapters = []SourceAdapter{
	{
		ID:      SourceClaude,
		DirName: ".claude",
		Display: "Claude",
	},
	{
		ID:      SourceCodex,
		DirName: ".codex",
		Display: "Codex",
	},
}

func RegisteredSourceAdapters() []SourceAdapter {
	out := make([]SourceAdapter, len(sourceAdapters))
	copy(out, sourceAdapters)
	return out
}

func AdapterBySource(source string) (SourceAdapter, bool) {
	for _, a := range sourceAdapters {
		if a.ID == source {
			return a, true
		}
	}
	return SourceAdapter{}, false
}

func DiscoverDefaultRoots(home string) []SourceRoot {
	var roots []SourceRoot
	for _, a := range sourceAdapters {
		dir := filepath.Join(home, a.DirName)
		if _, err := os.Stat(dir); err == nil {
			roots = append(roots, SourceRoot{Source: a.ID, Dir: dir})
		}
	}
	return roots
}

func sourceFromDir(dir string) string {
	base := filepath.Base(strings.TrimRight(dir, "/"))
	for _, a := range sourceAdapters {
		if base == a.DirName {
			return a.ID
		}
	}
	return ""
}

func DetectSourceFromDir(dir string) string {
	return sourceFromDir(dir)
}

func MakeSessionKey(source, rawID string) string {
	return source + ":" + rawID
}

func SplitSessionKey(key string) (source, rawID string) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", key
	}
	return parts[0], parts[1]
}
