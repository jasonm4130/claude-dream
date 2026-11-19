package main

import (
	"bytes"
	"context"
	"encoding/json"
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
}

func newEnv(t *testing.T, vars map[string]string) *testEnv {
	t.Helper()
	root := t.TempDir()
	te := &testEnv{state: filepath.Join(root, "cache", "claude-dream"), proj: filepath.Join(root, "projects")}
	te.Env = Env{
		StateDir: te.state, ProjectsDir: te.proj,
		Getenv:    func(k string) string { return vars[k] },
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

func writeMem(t *testing.T, te *testEnv, file, name, desc, body string) string {
	t.Helper()
	dir := filepath.Join(te.proj, projectDirName(cwd), "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, file)
	data := "---\nname: " + name + "\ndescription: " + desc + "\nmetadata:\n  type: project\n---\n" + body + "\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func start(id string) map[string]any {
	return map[string]any{"hook_event_name": "SessionStart", "session_id": id, "cwd": cwd}
}

func TestGuardEnvVars(t *testing.T) {
	for _, v := range []string{"CLAUDE_DREAM_WORKER", "CLAUDE_DREAM_OFF"} {
		te := newEnv(t, map[string]string{v: "1"})
		writeMem(t, te, "a.md", "widget-shop deploys", "widget-shop deploy notes", "body")
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

func additional(t *testing.T, out string) string {
	t.Helper()
	var o struct {
		H struct {
			Name string `json:"hookEventName"`
			Ctx  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.H.Name != "SessionStart" {
		t.Fatalf("bad output %q: %v", out, err)
	}
	return o.H.Ctx
}

func TestInjectRankingAndLog(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "high.md", "widget-shop release", "How widget-shop ships", "Tag then push.")
	writeMem(t, te, "mid.md", "shop notes", "Code layout for the shop", "Two path tokens only.")
	writeMem(t, te, "none.md", "tea preferences", "Likes oolong", "Irrelevant.")
	writeMem(t, te, "MEMORY.md", "widget-shop index", "widget-shop widget-shop", "index")
	arch := filepath.Join(te.proj, projectDirName(cwd), "memory", ".archive")
	_ = os.MkdirAll(arch, 0o755)
	_ = os.WriteFile(filepath.Join(arch, "old.md"), []byte("---\nname: widget-shop old\ndescription: widget-shop\n---\nx"), 0o644)

	_, out := runHook(t, te.Env, start("sess-1"))
	ctx := additional(t, out)
	if !strings.HasPrefix(ctx, injectHeader) {
		t.Fatalf("no header: %q", ctx)
	}
	if !strings.Contains(ctx, "widget-shop release") || strings.Contains(ctx, "tea preferences") ||
		strings.Contains(ctx, "index") || strings.Contains(ctx, "widget-shop old") {
		t.Fatalf("wrong selection:\n%s", ctx)
	}
	b, _ := os.ReadFile(filepath.Join(te.state, "injections.jsonl"))
	var rec struct {
		TS       string   `json:"ts"`
		Session  string   `json:"session_id"`
		Memories []string `json:"memories"`
	}
	if err := json.Unmarshal(b, &rec); err != nil || rec.Session != "sess-1" || len(rec.Memories) == 0 || rec.Memories[0] != "widget-shop release" || rec.TS == "" {
		t.Fatalf("log = %q (%v)", b, err)
	}
}

func TestInjectBelowThresholdEmitsNothing(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "a.md", "tea preferences", "Likes oolong", "x")
	writeMem(t, te, "b.md", "single token", "mentions widget only? no: Code", "x") // one path token = score 1
	code, out := runHook(t, te.Env, start("s"))
	if code != 0 || out != "" {
		t.Fatalf("want silence, got %q", out)
	}
	if _, err := os.Stat(filepath.Join(te.state, "injections.jsonl")); err == nil {
		t.Fatal("logged an injection that never happened")
	}
}

func TestInjectScopedToCwdDir(t *testing.T) {
	te := newEnv(t, nil)
	other := filepath.Join(te.proj, projectDirName("/Users/x/Code/other"), "memory")
	_ = os.MkdirAll(other, 0o755)
	_ = os.WriteFile(filepath.Join(other, "a.md"), []byte("---\nname: widget-shop\ndescription: widget-shop\n---\nx"), 0o644)
	if _, out := runHook(t, te.Env, start("s")); out != "" {
		t.Fatalf("injected from another project: %q", out)
	}
}

func TestInjectBudgetAndEnvOverride(t *testing.T) {
	body := strings.Repeat("z", 1000)
	fill := func(te *testEnv) {
		for _, n := range []string{"a", "b", "c", "d", "e"} {
			writeMem(t, te, n+".md", "widget-shop "+n, "widget-shop fact "+n, body)
		}
	}
	count := func(ctx string) int { return strings.Count(ctx, "\n## ") }

	te := newEnv(t, nil)
	fill(te)
	_, out := runHook(t, te.Env, start("s"))
	ctx := additional(t, out)
	if len(ctx) > defaultInjectChars || count(ctx) != 1 {
		t.Fatalf("default: len %d, memories %d", len(ctx), count(ctx))
	}

	te = newEnv(t, map[string]string{"CLAUDE_DREAM_INJECT_CHARS": "5000"})
	fill(te)
	_, out = runHook(t, te.Env, start("s"))
	ctx = additional(t, out)
	if len(ctx) > 5000 || count(ctx) < 4 {
		t.Fatalf("override: len %d, memories %d", len(ctx), count(ctx))
	}

	te = newEnv(t, map[string]string{"CLAUDE_DREAM_INJECT_CHARS": "999999"})
	if injectBudget(te.Env) != maxInjectChars {
		t.Fatalf("hard cap = %d", injectBudget(te.Env))
	}
	te = newEnv(t, map[string]string{"CLAUDE_DREAM_INJECT_CHARS": "junk"})
	if injectBudget(te.Env) != defaultInjectChars {
		t.Fatal("junk override should fall back")
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
	p := writeMem(t, te, "old.md", "widget-shop release", "How widget-shop ships", "Original body.")
	writeMem(t, te, "new.md", "widget-shop release v2", "How widget-shop ships now", "New body.")
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
	_, out := runHook(t, te.Env, start("s"))
	ctx := additional(t, out)
	if strings.Contains(ctx, "Original body") || !strings.Contains(ctx, "New body") {
		t.Fatalf("superseded memory injected or new one missing:\n%s", ctx)
	}
}

func TestSupersedeRefusesChangedFile(t *testing.T) {
	te := newEnv(t, nil)
	p := writeMem(t, te, "old.md", "n", "d", "b")
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

func readLog(t *testing.T, te *testEnv) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(te.state, "injections.jsonl"))
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestABArms(t *testing.T) {
	var onID, offID string
	for i := 0; onID == "" || offID == ""; i++ {
		id := "sess-" + itoa(i)
		if abArm(id) == "on" && onID == "" {
			onID = id
		} else if abArm(id) == "off" && offID == "" {
			offID = id
		}
	}
	if abArm(onID) != "on" {
		t.Fatal("arm not deterministic")
	}
	te := newEnv(t, map[string]string{"CLAUDE_DREAM_AB": "1"})
	writeMem(t, te, "a.md", "widget-shop release", "How widget-shop ships", "Body.")
	_, out := runHook(t, te.Env, start(onID))
	if out == "" {
		t.Fatal("on arm injected nothing")
	}
	_, out = runHook(t, te.Env, start(offID))
	if out != "" {
		t.Fatalf("off arm injected %q", out)
	}
	log := readLog(t, te)
	if len(log) != 2 || log[0]["arm"] != "on" || log[1]["arm"] != "off" || log[0]["chars"].(float64) == 0 || log[1]["chars"].(float64) != 0 {
		t.Fatalf("log = %v", log)
	}
	if log[1]["session_id"] != offID || log[0]["ts"] == "" {
		t.Fatalf("log = %v", log)
	}
	// Default: always on, arm "on", whatever the session id.
	te = newEnv(t, nil)
	writeMem(t, te, "a.md", "widget-shop release", "How widget-shop ships", "Body.")
	if _, out = runHook(t, te.Env, start(offID)); out == "" {
		t.Fatal("default must always inject")
	}
	if l := readLog(t, te); len(l) != 1 || l[0]["arm"] != "on" {
		t.Fatalf("log = %v", l)
	}
}
