package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Env carries what tests replace.
type Env struct {
	StateDir    string // queue, lock, session focus files and injection log
	ProjectsDir string // ~/.claude/projects
	Home        string // the user's home directory
	Getenv      func(string) string
	Now         func() time.Time
	// ReadDir lists a directory; tests wrap it to see what a hook reads.
	ReadDir func(string) ([]fs.DirEntry, error)
	// Spawn starts a detached `claude-dream work`. It must not wait for it.
	Spawn func() error
	// Extractor turns a queued job into memory changes; nil means the
	// built-in extractor, which calls the model through LLM.
	Extractor Extractor
	// LLM sends one prompt to the model (claude -p, inert).
	LLM LLM
}

func defaultEnv() Env {
	home, _ := os.UserHomeDir()
	var projects string
	if home != "" {
		projects = filepath.Join(home, ".claude", "projects")
	}
	state := stateDirFor(os.Getenv("XDG_STATE_HOME"), home)
	return Env{
		StateDir:    state,
		ProjectsDir: projects,
		Home:        home,
		Getenv:      os.Getenv,
		Now:         time.Now,
		ReadDir:     os.ReadDir,
		Spawn:       spawnWorker,
		LLM:         claudeLLM{Dir: state},
	}
}

// stateDirFor is a durable state directory, not a cache: the injection log
// feeds a long-running comparison.
func stateDirFor(xdg, home string) string {
	switch {
	case xdg != "" && filepath.IsAbs(xdg):
		return filepath.Join(xdg, "claude-dream")
	case home != "":
		return filepath.Join(home, ".local", "state", "claude-dream")
	}
	return filepath.Join(os.TempDir(), "claude-dream")
}

func (e Env) readDir(dir string) ([]fs.DirEntry, error) {
	if e.ReadDir == nil {
		return os.ReadDir(dir)
	}
	return e.ReadDir(dir)
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
