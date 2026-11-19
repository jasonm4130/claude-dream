package main

import (
	"os"
	"path/filepath"
	"time"
)

// Env carries what tests replace.
type Env struct {
	StateDir    string // queue, lock and injection log
	ProjectsDir string // ~/.claude/projects
	Getenv      func(string) string
	Now         func() time.Time
	// Spawn starts a detached `claude-dream work`. It must not wait for it.
	Spawn func() error
	// Extractor turns a queued job into memory changes.
	Extractor Extractor
}

func defaultEnv() Env {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	var projects string
	if home, err := os.UserHomeDir(); err == nil {
		projects = filepath.Join(home, ".claude", "projects")
	}
	return Env{
		StateDir:    filepath.Join(cache, "claude-dream"),
		ProjectsDir: projects,
		Getenv:      os.Getenv,
		Now:         time.Now,
		Spawn:       spawnWorker,
		Extractor:   noopExtractor{},
	}
}

// writeAtomic writes data to file through a temp file and a rename.
func writeAtomic(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), file)
}
