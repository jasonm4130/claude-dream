package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Job is one unit of work for the worker.
type Job struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Event          string `json:"event"`
	Trigger        string `json:"trigger,omitempty"` // PostCompact
	Reason         string `json:"reason,omitempty"`  // SessionEnd
	TS             string `json:"ts"`
}

func queueFile(stateDir string) string { return filepath.Join(stateDir, "queue.jsonl") }

// enqueue appends a job and starts the worker. It never waits and ignores
// every error: a lost job only means one session is not processed.
func enqueue(p Payload, env Env) {
	job := Job{
		SessionID: p.SessionID, TranscriptPath: p.TranscriptPath, Cwd: p.Cwd,
		Event: p.HookEventName, Trigger: p.Trigger, Reason: p.Reason,
		TS: env.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if appendJob(env.StateDir, job) != nil {
		return
	}
	if env.Spawn != nil {
		_ = env.Spawn()
	}
}

// appendJob writes the job as one line with a single O_APPEND write, which
// concurrent hooks cannot interleave.
func appendJob(stateDir string, job Job) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(queueFile(stateDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// spawnWorker starts `claude-dream work` in its own session with stdio on
// /dev/null, and returns without waiting for it.
func spawnWorker() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = devnull.Close() }()
	cmd := exec.Command(exe, "work")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
