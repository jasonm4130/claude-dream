package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Extractor turns one queued job into memory changes.
type Extractor interface {
	Extract(ctx context.Context, job Job) error
}

// noopExtractor is the phase 1 stub.
//
// TODO(phase 2): replace with an extractor that runs claudeCommand, reads the
// session transcript with eachLine, and writes through memstore.go.
type noopExtractor struct{}

func (noopExtractor) Extract(context.Context, Job) error { return nil }

// claudeCommand builds the headless extraction call. Phase 2 will run it;
// nothing runs it yet. The environment stops the child's own hooks from
// queueing more work and keeps it from writing auto memory itself.
func claudeCommand(ctx context.Context, job Job, prompt string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", "haiku", prompt)
	cmd.Dir = job.Cwd
	cmd.Env = append(os.Environ(),
		"CLAUDE_DREAM_WORKER=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
	)
	return cmd
}

func lockFile(stateDir string) string { return filepath.Join(stateDir, "worker.lock") }

// acquireLock takes the exclusive worker lock, replacing one whose PID is
// dead. It reports false when another live worker holds it.
func acquireLock(stateDir string) bool {
	if os.MkdirAll(stateDir, 0o755) != nil {
		return false
	}
	path := lockFile(stateDir)
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := fmt.Fprint(f, os.Getpid())
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
				return false
			}
			return true
		}
		if !errors.Is(err, os.ErrExist) {
			return false
		}
		b, rerr := os.ReadFile(path)
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if rerr == nil && perr == nil && pidAlive(pid) {
			return false
		}
		// Stale (dead PID), or empty because its owner is mid-write. An
		// empty lock is treated as stale only when unreadable as a PID;
		// the worst case is two workers draining one queue, which the
		// claim rename below keeps safe.
		_ = os.Remove(path)
	}
	return false
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func releaseLock(stateDir string) { _ = os.Remove(lockFile(stateDir)) }

// work is the `claude-dream work` entry point. It exits 0 always.
func work(env Env) int {
	defer func() { _ = recover() }()
	for range 3 {
		if !acquireLock(env.StateDir) {
			return 0
		}
		drain(context.Background(), env)
		releaseLock(env.StateDir)
		// A job enqueued after the last drain but before the unlock would
		// find the lock held and be stranded; look again.
		if !hasWork(env.StateDir) {
			break
		}
	}
	return 0
}

func hasWork(stateDir string) bool {
	if st, err := os.Stat(queueFile(stateDir)); err == nil && st.Size() > 0 {
		return true
	}
	m, _ := filepath.Glob(filepath.Join(stateDir, "queue.claimed-*"))
	return len(m) > 0
}

// drain processes claimed leftovers from a crashed worker, then claims the
// queue by renaming it, so a hook appending meanwhile starts a fresh file.
func drain(ctx context.Context, env Env) {
	for {
		claimed, _ := filepath.Glob(filepath.Join(env.StateDir, "queue.claimed-*"))
		if len(claimed) == 0 {
			dst := filepath.Join(env.StateDir, "queue.claimed-"+strconv.Itoa(os.Getpid()))
			if os.Rename(queueFile(env.StateDir), dst) != nil {
				return
			}
			claimed = []string{dst}
		}
		for _, f := range claimed {
			processFile(ctx, env, f)
			_ = os.Remove(f) // queue files, not memory: safe to drop once handled
		}
	}
}

func processFile(ctx context.Context, env Env, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		var job Job
		if len(line) > 0 && json.Unmarshal(line, &job) == nil && env.Extractor != nil {
			_ = env.Extractor.Extract(ctx, job) // one bad job must not stop the rest
		}
		if err != nil {
			return
		}
	}
}
