package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Extractor turns one queued job into memory changes. A nil Env.Extractor
// means the built-in one, extractJob; tests replace it.
type Extractor interface {
	Extract(ctx context.Context, job Job) error
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
		capped := drain(context.Background(), env)
		if !capped {
			maybeDream(context.Background(), env)
		}
		releaseLock(env.StateDir)
		// A job enqueued after the last drain but before the unlock would
		// find the lock held and be stranded; look again. Capped jobs wait
		// for tomorrow's budget.
		if capped || !hasWork(env.StateDir) {
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
// It stops, and reports true, when the daily LLM cap is reached; the jobs
// not yet done go back on the queue.
func drain(ctx context.Context, env Env) (capped bool) {
	for {
		claimed, _ := filepath.Glob(filepath.Join(env.StateDir, "queue.claimed-*"))
		if len(claimed) == 0 {
			dst := filepath.Join(env.StateDir, "queue.claimed-"+strconv.Itoa(os.Getpid()))
			if os.Rename(queueFile(env.StateDir), dst) != nil {
				return false
			}
			claimed = []string{dst}
		}
		for _, f := range claimed {
			if processFile(ctx, env, f) {
				capped = true
			}
			_ = os.Remove(f) // queue files, not memory: safe to drop once handled
		}
		if capped {
			return true
		}
	}
}

// processFile runs each job in a claimed queue file. Once the cap is hit,
// that job and every later one are appended to the queue again.
func processFile(ctx context.Context, env Env, path string) (capped bool) {
	_ = eachLine(path, func(line []byte, _ int64) {
		var job Job
		if json.Unmarshal(line, &job) != nil {
			return
		}
		if !capped {
			var err error
			if env.Extractor != nil {
				err = env.Extractor.Extract(ctx, job)
			} else {
				err = extractJob(ctx, env, job)
			}
			capped = errors.Is(err, errCapped) // one bad job must not stop the rest
		}
		if capped {
			_ = appendJob(env.StateDir, job)
		}
	})
	return capped
}
