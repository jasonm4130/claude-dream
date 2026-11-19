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
	"time"
)

// Extractor turns one queued job into memory changes. A nil Env.Extractor
// means the built-in one, extractJob; tests replace it.
type Extractor interface {
	Extract(ctx context.Context, job Job) error
}

const maxWorkerRun = 30 * time.Minute // a worker stops taking jobs after this

func lockFile(stateDir string) string { return filepath.Join(stateDir, "worker.lock") }

// workerLock is the held worker lock: an flock on an open fd, kept for the
// holder's lifetime. The kernel drops it when the process dies, so a stale
// lock cannot exist. The file also holds the PID and start time for people.
type workerLock struct{ f *os.File }

// acquireLock takes the exclusive worker lock without waiting. It returns
// nil when another process holds it. now is written for the age check.
func acquireLock(stateDir string, now time.Time) *workerLock {
	if os.MkdirAll(stateDir, 0o755) != nil {
		return nil
	}
	f, err := os.OpenFile(lockFile(stateDir), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()
		return nil
	}
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("%d %s\n", os.Getpid(), now.UTC().Format(time.RFC3339))), 0)
	}
	return &workerLock{f: f}
}

// release unlocks; only the holder has a workerLock to call it on. The file
// stays: removing it would let a later worker lock a different inode.
func (l *workerLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// holderAge reads when the current lock holder started, from the lock file.
func holderAge(stateDir string, now time.Time) (time.Duration, bool) {
	b, err := os.ReadFile(lockFile(stateDir))
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, fields[1])
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// logWorker appends one line to worker.jsonl.
func logWorker(env Env, event string, detail any) {
	appendJSONL(filepath.Join(env.StateDir, "worker.jsonl"), map[string]any{
		"ts": env.Now().UTC().Format(time.RFC3339), "pid": os.Getpid(), "event": event, "detail": detail,
	})
}

// work is the `claude-dream work` entry point. It exits 0 always.
func work(env Env) int {
	defer func() { _ = recover() }()
	start := env.Now()
	for range 3 {
		lock := acquireLock(env.StateDir, env.Now())
		if lock == nil {
			// Never steal a lock; just note a holder that looks stuck.
			if age, ok := holderAge(env.StateDir, env.Now()); ok && age > maxWorkerRun {
				logWorker(env, "holder_stuck", age.Round(time.Second).String())
			}
			return 0
		}
		stopped := drain(context.Background(), env, start)
		if !stopped {
			maybeDream(context.Background(), env)
		}
		lock.release()
		// A job enqueued after the last drain but before the unlock would
		// find the lock held and be stranded; look again. Stopped jobs wait
		// for a later worker.
		if stopped || !hasWork(env.StateDir) {
			break
		}
	}
	return 0
}

func hasWork(stateDir string) bool {
	if st, err := os.Stat(queueFile(stateDir)); err == nil && st.Size() > 0 {
		return true
	}
	return len(adoptable(stateDir)) > 0
}

// adoptable lists claimed queue files this process may run: its own, or
// those whose claiming PID is dead. A live worker's claim is never touched.
func adoptable(stateDir string) []string {
	m, _ := filepath.Glob(filepath.Join(stateDir, "queue.claimed-*"))
	var out []string
	for _, f := range m {
		pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(f), "queue.claimed-"))
		if err != nil || pid == os.Getpid() || !pidAlive(pid) {
			out = append(out, f)
		}
	}
	return out
}

// drain processes claimed leftovers from a crashed worker, then claims the
// queue by renaming it, so a hook appending meanwhile starts a fresh file.
// It stops, and reports true, when the daily LLM cap is reached or the
// worker has run for 30 minutes; the jobs not yet done go back on the queue.
func drain(ctx context.Context, env Env, start time.Time) (stopped bool) {
	for {
		claimed := adoptable(env.StateDir)
		if len(claimed) == 0 {
			dst := filepath.Join(env.StateDir, "queue.claimed-"+strconv.Itoa(os.Getpid()))
			if os.Rename(queueFile(env.StateDir), dst) != nil {
				return false
			}
			claimed = []string{dst}
		}
		for _, f := range claimed {
			if processFile(ctx, env, f, start) {
				stopped = true
			}
			_ = os.Remove(f) // queue files, not memory: safe to drop once handled
		}
		if stopped {
			return true
		}
	}
}

// processFile runs each job in a claimed queue file. Once the worker must
// stop, that job and every later one are appended to the queue again.
func processFile(ctx context.Context, env Env, path string, start time.Time) (stopped bool) {
	_ = eachLine(path, func(line []byte, _ int64) {
		var job Job
		if json.Unmarshal(line, &job) != nil {
			return
		}
		if !stopped && env.Now().Sub(start) > maxWorkerRun {
			stopped = true
			logWorker(env, "aged_out", "stopped after "+maxWorkerRun.String()+"; jobs left queued")
		}
		if !stopped {
			var err error
			if env.Extractor != nil {
				err = env.Extractor.Extract(ctx, job)
			} else {
				err = extractJob(ctx, env, job)
			}
			stopped = errors.Is(err, errCapped) // one bad job must not stop the rest
		}
		if stopped {
			_ = appendJob(env.StateDir, job)
		}
	})
	return stopped
}
