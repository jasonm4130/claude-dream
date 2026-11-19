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
	llm    *fakeLLM
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
	te.llm = &fakeLLM{}
	te.Env = Env{
		StateDir: te.state, ProjectsDir: te.proj, Home: tmp,
		ReadDir: func(d string) ([]fs.DirEntry, error) { te.reads = append(te.reads, d); return os.ReadDir(d) },
		Getenv:  func(k string) string { return all[k] },
		// Noon UTC is 2026-11-19 in every local time zone, so dates in
		// file names are stable wherever the tests run.
		Now:       func() time.Time { return time.Date(2026, 11, 19, 12, 2, 3, 0, time.UTC) },
		Spawn:     func() error { te.spawns++; return nil },
		Extractor: noopExtractor{},
		LLM:       te.llm,
	}
	return te
}

// noopExtractor ignores every job; tests that exercise extraction set
// Extractor to nil to get the built-in one.
type noopExtractor struct{}

func (noopExtractor) Extract(context.Context, Job) error { return nil }

// fakeLLM returns canned answers in order, then "[]". It never runs claude.
type fakeLLM struct {
	answers []string
	prompts []string
	systems []string
}

func (f *fakeLLM) Run(_ context.Context, system, prompt string) (string, error) {
	f.systems = append(f.systems, system)
	f.prompts = append(f.prompts, prompt)
	if len(f.answers) == 0 {
		return "[]", nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
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
	if j.SessionID != "s1" || j.Event != "PostCompact" || j.Trigger != "auto" || j.TS != "2026-11-19T12:02:03Z" || j.TranscriptPath != "/t/1.jsonl" {
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
	l := acquireLock(te.state, time.Now())
	if l == nil {
		t.Fatal("lock not released")
	}
	l.release()
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	l := acquireLock(dir, now)
	if l == nil {
		t.Fatal("first acquire failed")
	}
	if acquireLock(dir, now) != nil {
		t.Fatal("second acquire succeeded while held")
	}
	b, _ := os.ReadFile(lockFile(dir))
	if f := strings.Fields(string(b)); len(f) != 2 || f[0] != itoa(os.Getpid()) || f[1] != "2026-11-19T12:00:00Z" {
		t.Fatalf("lock holds %q", b)
	}
	// A held lock makes work() a no-op that leaves the queue alone, and a
	// holder older than 30 minutes is logged, never stolen.
	te := newEnv(t, nil)
	te.StateDir, te.state = dir, dir
	te.Now = func() time.Time { return now.Add(31 * time.Minute) }
	_ = appendJob(dir, Job{SessionID: "x"})
	rec := &recorder{}
	te.Extractor = rec
	if work(te.Env) != 0 || len(rec.got) != 0 || !hasWork(dir) {
		t.Fatal("worker ran despite held lock")
	}
	if lg, _ := os.ReadFile(filepath.Join(dir, "worker.jsonl")); !strings.Contains(string(lg), `"event":"holder_stuck"`) {
		t.Fatalf("stuck holder not logged: %s", lg)
	}
	// Only the holder releases; then the lock is free again.
	l.release()
	l.release() // twice is harmless
	l2 := acquireLock(dir, now)
	if l2 == nil {
		t.Fatal("released lock not reacquired")
	}
	l2.release()
}

func TestDrainAdoptsOnlyDeadOrOwnClaims(t *testing.T) {
	te := newEnv(t, nil)
	rec := &recorder{}
	te.Extractor = rec
	_ = os.MkdirAll(te.state, 0o755)
	write := func(name, id string) string {
		p := filepath.Join(te.state, name)
		b, _ := json.Marshal(Job{SessionID: id})
		_ = os.WriteFile(p, append(b, '\n'), 0o644)
		return p
	}
	live := write("queue.claimed-"+itoa(os.Getppid()), "live") // the test's parent: alive, not us
	write("queue.claimed-999999999", "dead")
	write("queue.claimed-"+itoa(os.Getpid()), "own")
	work(te.Env)
	var got []string
	for _, j := range rec.got {
		got = append(got, j.SessionID)
	}
	if strings.Join(got, ",") != "dead,own" && strings.Join(got, ",") != "own,dead" {
		t.Fatalf("ran %v", got)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("a live worker's claim was touched")
	}
}

func TestWorkerAgesOut(t *testing.T) {
	te := newEnv(t, nil)
	now := te.Now()
	te.Now = func() time.Time { return now }
	calls := 0
	te.Extractor = extractorFunc(func(Job) error {
		calls++
		now = now.Add(20 * time.Minute) // each job takes 20 minutes of the clock
		return nil
	})
	for _, id := range []string{"a", "b", "c", "d"} {
		_ = appendJob(te.state, Job{SessionID: id})
	}
	work(te.Env)
	if calls != 2 {
		t.Fatalf("ran %d jobs, want 2 before the 30 minute limit", calls)
	}
	b, _ := os.ReadFile(queueFile(te.state))
	if strings.Count(string(b), "\n") != 2 {
		t.Fatalf("queue = %s", b)
	}
	if lg, _ := os.ReadFile(filepath.Join(te.state, "worker.jsonl")); !strings.Contains(string(lg), "aged_out") {
		t.Fatal("age-out not logged")
	}
}

type extractorFunc func(Job) error

func (f extractorFunc) Extract(_ context.Context, j Job) error { return f(j) }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestClaudeCommandBuilder(t *testing.T) {
	cmd := claudeCommand(context.Background(), "/w", "sys")
	want := []string{"claude", "-p", "--model", "haiku", "--output-format", "json", "--tools", "",
		"--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "--system-prompt", "sys"}
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q", cmd.Args)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"CLAUDE_DREAM_WORKER=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	if cmd.WaitDelay != 10*time.Second {
		t.Errorf("WaitDelay = %v", cmd.WaitDelay)
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
