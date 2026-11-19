package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Everything here is synthetic: no real transcripts or memory files.

type testEnv struct {
	Env
	spawns int
	state  string
	proj   string
	root   string   // the one repo root; repos are directories under it
	reads  []string // every directory the hooks listed
}

func newEnv(t *testing.T, vars map[string]string) *testEnv {
	t.Helper()
	tmp := t.TempDir()
	te := &testEnv{state: filepath.Join(tmp, "state", "claude-dream"), proj: filepath.Join(tmp, "projects"), root: filepath.Join(tmp, "Code")}
	if err := os.MkdirAll(te.root, 0o755); err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"CLAUDE_DREAM_ROOTS": te.root}
	for k, v := range vars {
		all[k] = v
	}
	te.Env = Env{
		StateDir: te.state, ProjectsDir: te.proj, Home: tmp,
		ReadDir:   func(d string) ([]fs.DirEntry, error) { te.reads = append(te.reads, d); return os.ReadDir(d) },
		Getenv:    func(k string) string { return all[k] },
		Now:       func() time.Time { return time.Date(2026, 11, 19, 1, 2, 3, 0, time.UTC) },
		Spawn:     func() error { te.spawns++; return nil },
		Extractor: noopExtractor{},
	}
	return te
}

func runHook(t *testing.T, env Env, payload map[string]any) (int, string) {
	t.Helper()
	in, _ := json.Marshal(payload)
	var out, errb bytes.Buffer
	code := run(bytes.NewReader(in), &out, &errb, env)
	if errb.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errb.String())
	}
	return code, out.String()
}

const cwd = "/Users/x/Code/widget-shop"

func TestGuardEnvVars(t *testing.T) {
	for _, v := range []string{"CLAUDE_DREAM_WORKER", "CLAUDE_DREAM_OFF"} {
		te := newEnv(t, map[string]string{v: "1"})
		writeMem(t, te, "widget-shop", "a.md", "widget-shop deploys", "widget-shop deploy notes", "body")
		for _, ev := range []string{"PostCompact", "SessionEnd", "SessionStart"} {
			code, out := runHook(t, te.Env, map[string]any{"hook_event_name": ev, "session_id": "s", "cwd": cwd})
			if code != 0 || out != "" {
				t.Fatalf("%s %s: code %d out %q", v, ev, code, out)
			}
		}
		if te.spawns != 0 {
			t.Fatalf("%s: spawned", v)
		}
		if _, err := os.Stat(queueFile(te.state)); err == nil {
			t.Fatalf("%s: queue written", v)
		}
	}
}

func TestFailOpen(t *testing.T) {
	te := newEnv(t, nil)
	code, out := runHook(t, te.Env, map[string]any{"hook_event_name": "Bogus"})
	if code != 0 || out != "" {
		t.Fatal("unknown event must be silent")
	}
	var o, e bytes.Buffer
	if run(strings.NewReader("not json"), &o, &e, te.Env) != 0 || o.Len() != 0 {
		t.Fatal("bad JSON must exit 0 silently")
	}
}

func TestEnqueueAndSpawn(t *testing.T) {
	te := newEnv(t, nil)
	runHook(t, te.Env, map[string]any{"hook_event_name": "PostCompact", "session_id": "s1", "transcript_path": "/t/1.jsonl", "cwd": cwd, "trigger": "auto"})
	runHook(t, te.Env, map[string]any{"hook_event_name": "SessionEnd", "session_id": "s2", "cwd": cwd, "reason": "clear"})
	if te.spawns != 2 {
		t.Fatalf("spawns = %d, want 2", te.spawns)
	}
	b, _ := os.ReadFile(queueFile(te.state))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("queue lines = %d", len(lines))
	}
	var j Job
	_ = json.Unmarshal([]byte(lines[0]), &j)
	if j.SessionID != "s1" || j.Event != "PostCompact" || j.Trigger != "auto" || j.TS != "2026-11-19T01:02:03Z" || j.TranscriptPath != "/t/1.jsonl" {
		t.Fatalf("job = %+v", j)
	}
	_ = json.Unmarshal([]byte(lines[1]), &j)
	if j.Reason != "clear" {
		t.Fatalf("job = %+v", j)
	}
}

type recorder struct{ got []Job }

func (r *recorder) Extract(_ context.Context, j Job) error { r.got = append(r.got, j); return nil }

func TestWorkerDrainsQueue(t *testing.T) {
	te := newEnv(t, nil)
	rec := &recorder{}
	te.Extractor = rec
	runHook(t, te.Env, map[string]any{"hook_event_name": "SessionEnd", "session_id": "a", "cwd": cwd})
	runHook(t, te.Env, map[string]any{"hook_event_name": "SessionEnd", "session_id": "b", "cwd": cwd})
	if work(te.Env) != 0 || len(rec.got) != 2 || rec.got[0].SessionID != "a" {
		t.Fatalf("got %+v", rec.got)
	}
	if hasWork(te.state) {
		t.Fatal("queue not drained")
	}
	if _, err := os.Stat(lockFile(te.state)); err == nil {
		t.Fatal("lock not released")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	if !acquireLock(dir) {
		t.Fatal("first acquire failed")
	}
	if acquireLock(dir) {
		t.Fatal("second acquire succeeded while held by a live PID")
	}
	releaseLock(dir)
	// Stale: a PID that is not running.
	if err := os.WriteFile(lockFile(dir), []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !acquireLock(dir) {
		t.Fatal("stale lock not replaced")
	}
	b, _ := os.ReadFile(lockFile(dir))
	if strings.TrimSpace(string(b)) != itoa(os.Getpid()) {
		t.Fatalf("lock holds %q", b)
	}
	// A held lock makes work() a no-op that leaves the queue alone.
	te := newEnv(t, nil)
	te.StateDir = dir
	_ = appendJob(dir, Job{SessionID: "x"})
	rec := &recorder{}
	te.Extractor = rec
	if work(te.Env) != 0 || len(rec.got) != 0 || !hasWork(dir) {
		t.Fatal("worker ran despite held lock")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestClaudeCommandBuilder(t *testing.T) {
	cmd := claudeCommand(context.Background(), Job{Cwd: "/w"}, "p")
	if got := strings.Join(cmd.Args, " "); got != "claude -p --model haiku p" {
		t.Fatalf("args = %q", got)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"CLAUDE_DREAM_WORKER=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	if cmd.Dir != "/w" {
		t.Errorf("dir = %q", cmd.Dir)
	}
}

func TestMemstoreConflictAndArchive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.md")
	if err := writeMemory(p, []byte("---\nname: m\n---\nv1"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	m, err := readMemory(p)
	if err != nil || m.Name != "m" || m.Body != "v1" {
		t.Fatalf("read %+v %v", m, err)
	}
	// Someone else edits it after our read.
	later := m.ModTime.Add(time.Hour)
	if err := os.WriteFile(p, []byte("---\nname: m\n---\nv2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(p, []byte("mine"), m.ModTime); err != ErrConflict {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "v2") {
		t.Fatal("conflicting write clobbered the file")
	}
	m2, _ := readMemory(p)
	if err := writeMemory(p, []byte("---\nname: m\n---\nv3"), m2.ModTime); err != nil {
		t.Fatalf("fresh write refused: %v", err)
	}
	if err := writeMemory(filepath.Join(dir, "new.md"), nil, time.Now()); err != ErrConflict {
		t.Fatal("expected conflict for vanished file")
	}
	// Archive twice with the same name: neither is lost.
	dst, err := archiveMemory(p)
	if err != nil || filepath.Base(filepath.Dir(dst)) != ".archive" {
		t.Fatalf("archive: %v %s", err, dst)
	}
	_ = os.WriteFile(p, []byte("again"), 0o644)
	dst2, err := archiveMemory(p)
	if err != nil || dst2 == dst {
		t.Fatalf("second archive: %v %s", err, dst2)
	}
}

func TestParseFrontmatter(t *testing.T) {
	m := parseMemory("---\nname: \"a b\"\ndescription: d\nmetadata:\n  type: feedback\n---\n\nbody\n")
	if m.Name != "a b" || m.Description != "d" || m.Type != "feedback" || strings.TrimSpace(m.Body) != "body" {
		t.Fatalf("%+v", m)
	}
	if parseMemory("no frontmatter").Body != "no frontmatter" {
		t.Fatal("body")
	}
}

func TestSupersedeKeepsBodyAndIsSkipped(t *testing.T) {
	te := newEnv(t, nil)
	p := writeMem(t, te, "widget-shop", "old.md", "widget-shop release", "How widget-shop ships", "Original body.")
	writeMem(t, te, "widget-shop", "new.md", "widget-shop release v2", "How widget-shop ships now", "New body.")
	before, _ := os.ReadFile(p)
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	if err := supersede(p, "widget-shop release v2", now); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	m := parseMemory(string(after))
	if m.SupersededBy != "widget-shop release v2" || !strings.Contains(string(after), "valid_until: 2026-11-19T00:00:00Z") {
		t.Fatalf("fields missing:\n%s", after)
	}
	if m.Body != parseMemory(string(before)).Body || m.Name != "widget-shop release" {
		t.Fatal("body or other fields changed")
	}
	_, out := runHook(t, te.Env, event("SessionStart", "s", te.root+"/widget-shop", nil))
	ctx := additional(t, out, "SessionStart")
	if strings.Contains(ctx, "Original body") || !strings.Contains(ctx, "New body") {
		t.Fatalf("superseded memory injected or new one missing:\n%s", ctx)
	}
}

func TestSupersedeRefusesChangedFile(t *testing.T) {
	te := newEnv(t, nil)
	p := writeMem(t, te, "widget-shop", "old.md", "n", "d", "b")
	// Simulate a change between read and write by calling writeMemory with a stale mtime.
	st, _ := os.Stat(p)
	stale := st.ModTime().Add(-time.Hour)
	if err := writeMemory(p, []byte("x"), stale); err != ErrConflict {
		t.Fatalf("err = %v", err)
	}
	if err := supersede(filepath.Join(t.TempDir(), "missing.md"), "x", time.Now()); err == nil {
		t.Fatal("expected error for missing file")
	}
}
