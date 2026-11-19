package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Repos here are directories under a temp root; memory dirs live under a
// temp projects dir. Names are invented.

func mkRepo(t *testing.T, te *testEnv, repo string) string {
	t.Helper()
	p := filepath.Join(te.root, repo)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeMem writes a memory file for a repo, or for the root itself when repo is "".
func writeMem(t *testing.T, te *testEnv, repo, file, name, desc, body string) string {
	t.Helper()
	path := te.root
	if repo != "" {
		path = mkRepo(t, te, repo)
	}
	return writeMemAt(t, te, path, file, name, desc, body, "project")
}

func writeMemAt(t *testing.T, te *testEnv, path, file, name, desc, body, typ string) string {
	t.Helper()
	dir := filepath.Join(te.proj, projectDirName(path), "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, file)
	data := "---\nname: " + name + "\ndescription: " + desc + "\nmetadata:\n  type: " + typ + "\n---\n" + body + "\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func event(name, id, cwd string, extra map[string]any) map[string]any {
	m := map[string]any{"hook_event_name": name, "session_id": id, "cwd": cwd}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func prompt(id, cwd, text string) map[string]any {
	return event("UserPromptSubmit", id, cwd, map[string]any{"prompt": text})
}

func tool(id, cwd string, input map[string]any) map[string]any {
	return event("PostToolUse", id, cwd, map[string]any{"tool_name": "Read", "tool_input": input})
}

func additional(t *testing.T, out, wantEvent string) string {
	t.Helper()
	var o struct {
		H struct {
			Name string `json:"hookEventName"`
			Ctx  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.H.Name != wantEvent {
		t.Fatalf("bad output %q: %v", out, err)
	}
	return o.H.Ctx
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

func memoryReads(te *testEnv) int {
	n := 0
	for _, d := range te.reads {
		if strings.HasPrefix(d, te.proj) {
			n++
		}
	}
	return n
}

func TestSessionStartInRootInjectsNothing(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "garden-api", "a.md", "garden-api deploys", "garden-api deploy notes", "x")
	writeMem(t, te, "", "r.md", "root garden-api", "root garden-api deploy", "x")
	code, out := runHook(t, te.Env, event("SessionStart", "s", te.root, nil))
	if code != 0 || out != "" {
		t.Fatalf("want silence, got %q", out)
	}
	if _, err := os.Stat(filepath.Join(te.state, "injections.jsonl")); err == nil {
		t.Fatal("logged an injection that never happened")
	}
}

func TestSessionStartInsideRepo(t *testing.T) {
	te := newEnv(t, nil)
	repo := mkRepo(t, te, "garden-api")
	writeMemAt(t, te, repo, "a.md", "Release flow", "How garden-api ships", "Tag then push.", "project")
	writeMemAt(t, te, repo, "b.md", "Never mock the db", "Testing rule", "Use a real db.", "feedback")
	writeMemAt(t, te, repo, "MEMORY.md", "index", "index", "index", "project")
	arch := filepath.Join(te.proj, projectDirName(repo), "memory", ".archive")
	_ = os.MkdirAll(arch, 0o755)
	_ = os.WriteFile(filepath.Join(arch, "old.md"), []byte("---\nname: archived\n---\nx"), 0o644)
	_ = os.WriteFile(filepath.Join(filepath.Dir(arch), "sup.md"), []byte("---\nname: superseded one\nsuperseded_by: x\n---\nx"), 0o644)

	_, out := runHook(t, te.Env, event("SessionStart", "s", filepath.Join(repo, "cmd", "api"), nil))
	ctx := additional(t, out, "SessionStart")
	if !strings.HasPrefix(ctx, "claude-dream: memories for garden-api (from ~/.claude/projects/") {
		t.Fatalf("header: %q", ctx)
	}
	for _, bad := range []string{"index", "archived", "superseded one"} {
		if strings.Contains(ctx, bad) {
			t.Fatalf("injected %q:\n%s", bad, ctx)
		}
	}
	if strings.Index(ctx, "Never mock") > strings.Index(ctx, "Release flow") {
		t.Fatalf("feedback must rank before project:\n%s", ctx)
	}
	log := readLog(t, te)
	if len(log) != 1 || log[0]["event"] != "SessionStart" || log[0]["session_id"] != "s" || log[0]["arm"] != "on" || log[0]["chars"].(float64) == 0 {
		t.Fatalf("log = %v", log)
	}
	// Same session, same repo again (resume): nothing new.
	if _, out = runHook(t, te.Env, event("SessionStart", "s", repo, map[string]any{"source": "resume"})); out != "" {
		t.Fatalf("resume re-injected: %q", out)
	}
	// A cwd outside every root is in no repo.
	if _, out = runHook(t, te.Env, event("SessionStart", "o", "/Users/alex/elsewhere/garden-api", nil)); out != "" {
		t.Fatalf("outside root injected: %q", out)
	}
}

func TestPromptNamesRepoOnce(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "garden-api", "a.md", "Release flow", "How garden-api ships", "Tag then push.")
	_, out := runHook(t, te.Env, prompt("s", te.root, "can you check the Garden API rate limits"))
	ctx := additional(t, out, "UserPromptSubmit")
	if !strings.Contains(ctx, "Release flow") {
		t.Fatalf("ctx = %q", ctx)
	}
	if _, out = runHook(t, te.Env, prompt("s", te.root, "and garden-api again")); out != "" {
		t.Fatalf("second prompt injected %q", out)
	}
	writeMem(t, te, "garden-api", "b.md", "Late file", "added later", "x")
	if _, out = runHook(t, te.Env, prompt("s", te.root, "garden_api once more")); out != "" {
		t.Fatalf("already-focused repo injected again: %q", out)
	}
}

func TestPromptPlainWordAndAmbiguous(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "mapper", "a.md", "Mapper facts", "mapper", "x")
	writeMem(t, te, "garden-api", "b.md", "Garden facts", "garden", "x")
	writeMem(t, te, "site-v2", "c.md", "Site facts", "site", "x")
	mkRepo(t, te, "kiln.md") // a name-shaped directory, not a repo of interest
	for _, p := range []string{
		"the mapper is slow", "run the Mapper job",
		"compare garden-api with site-v2",
		"garden api and site v2 differ",
	} {
		if _, out := runHook(t, te.Env, prompt("s", te.root, p)); out != "" {
			t.Fatalf("%q injected %q", p, out)
		}
	}
	if err := os.WriteFile(filepath.Join(te.root, "notes-2026"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out := runHook(t, te.Env, prompt("s", te.root, "see notes-2026")); out != "" {
		t.Fatalf("a file counted as a repo: %q", out)
	}
	// An unambiguous prompt still works afterwards in the same session.
	if _, out := runHook(t, te.Env, prompt("s", te.root, "tell me about SITE-V2")); !strings.Contains(out, "Site facts") {
		t.Fatalf("out = %q", out)
	}
}

func TestPostToolUsePathsAndFastPath(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "garden-api", "a.md", "Release flow", "How garden-api ships", "Tag then push.")
	mkRepo(t, te, "site-v2")

	_, out := runHook(t, te.Env, tool("s", te.root, map[string]any{"file_path": "/Users/alex/Documents/notes.txt"}))
	if out != "" || memoryReads(te) != 0 {
		t.Fatalf("unrelated path: out %q, memory reads %d", out, memoryReads(te))
	}
	_, out = runHook(t, te.Env, tool("s", te.root, map[string]any{
		"edits": []any{map[string]any{"path": filepath.Join(te.root, "garden-api", "cmd", "main.go")}},
	}))
	if !strings.Contains(additional(t, out, "PostToolUse"), "Release flow") {
		t.Fatalf("out = %q", out)
	}
	te.reads = nil
	_, out = runHook(t, te.Env, tool("s", te.root, map[string]any{"file_path": filepath.Join(te.root, "garden-api", "x.go")}))
	if out != "" || memoryReads(te) != 0 || len(te.reads) != 0 {
		t.Fatalf("known repo: out %q, reads %v", out, te.reads)
	}
	// A bash command, the bare repo path, and a ~/ path all count.
	for _, mk := range []func(root string) map[string]any{
		func(r string) map[string]any { return map[string]any{"command": "cd " + r + "/site-v2 && ls"} },
		func(r string) map[string]any { return map[string]any{"path": r + "/site-v2"} },
	} {
		te2 := newEnv(t, nil)
		writeMem(t, te2, "site-v2", "c.md", "Site facts", "x", "x")
		if _, out := runHook(t, te2.Env, tool("t", te2.root, mk(te2.root))); !strings.Contains(out, "Site facts") {
			t.Fatalf("out = %q", out)
		}
	}
	te3 := newEnv(t, map[string]string{"CLAUDE_DREAM_ROOTS": ""}) // default root is $HOME/Code
	writeMem(t, te3, "site-v2", "c.md", "Site facts", "x", "x")
	if _, out := runHook(t, te3.Env, tool("t", "/x", map[string]any{"file_path": "~/Code/site-v2/a.go"})); !strings.Contains(out, "Site facts") {
		t.Fatalf("tilde: out = %q", out)
	}
	// A file directly under the root is not a repo.
	if err := os.WriteFile(filepath.Join(te.root, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out := runHook(t, te.Env, tool("u", te.root, map[string]any{"file_path": filepath.Join(te.root, "AGENTS.md")})); out != "" {
		t.Fatalf("file counted: %q", out)
	}
}

func TestPromptKeywordPath(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "", "hit.md", "Billing webhook retries", "Stripe webhook retry schedule", "x")
	writeMem(t, te, "", "one.md", "Webhook basics", "unrelated tea", "x")
	writeMem(t, te, "", "none.md", "Tea preferences", "Likes oolong", "x")
	_, out := runHook(t, te.Env, prompt("s", te.root, "why do the webhook retry attempts stop?"))
	ctx := additional(t, out, "UserPromptSubmit")
	if !strings.Contains(ctx, "Billing webhook retries") || strings.Contains(ctx, "Webhook basics") || strings.Contains(ctx, "Tea preferences") {
		t.Fatalf("ctx:\n%s", ctx)
	}
	if _, out = runHook(t, te.Env, prompt("s", te.root, "webhook retry attempts again")); out != "" {
		t.Fatalf("injected twice: %q", out)
	}
	// Focus repos take part too.
	writeMem(t, te, "garden-api", "g.md", "Cache invalidation", "Redis cache invalidation rules", "x")
	runHook(t, te.Env, tool("s", te.root, map[string]any{"file_path": filepath.Join(te.root, "garden-api", "a.go")}))
	writeMem(t, te, "garden-api", "h.md", "Queue sharding", "Redis queue sharding layout", "x")
	_, out = runHook(t, te.Env, prompt("s", te.root, "how does redis queue sharding work"))
	if !strings.Contains(out, "Queue sharding") {
		t.Fatalf("focus-repo keyword memory missing: %q", out)
	}
}

func TestSessionCapAcrossEvents(t *testing.T) {
	te := newEnv(t, map[string]string{"CLAUDE_DREAM_SESSION_CHARS": "1500", "CLAUDE_DREAM_INJECT_CHARS": "5000"})
	body := strings.Repeat("z", 1000)
	writeMem(t, te, "garden-api", "a.md", "Alpha fact", "x", body)
	writeMem(t, te, "site-v2", "b.md", "Beta fact", "x", body)
	_, out := runHook(t, te.Env, tool("s", te.root, map[string]any{"file_path": te.root + "/garden-api/a.go"}))
	if !strings.Contains(out, "Alpha fact") {
		t.Fatalf("out = %q", out)
	}
	if _, out = runHook(t, te.Env, tool("s", te.root, map[string]any{"file_path": te.root + "/site-v2/a.go"})); out != "" {
		t.Fatalf("cap exceeded: %q", out)
	}
	b, _ := os.ReadFile(stateFile(te.Env, "s"))
	var st sessionState
	if json.Unmarshal(b, &st) != nil || len(st.Focus) != 2 || st.Chars > 1500 {
		t.Fatalf("state = %s", b)
	}
}

func TestPerEventBudget(t *testing.T) {
	te := newEnv(t, nil)
	for _, n := range []string{"a", "b", "c", "d"} {
		writeMem(t, te, "garden-api", n+".md", "Fact "+n, "x", strings.Repeat("z", 1000))
	}
	_, out := runHook(t, te.Env, event("SessionStart", "s", te.root+"/garden-api", nil))
	if ctx := additional(t, out, "SessionStart"); len(ctx) > defaultInjectChars || strings.Count(ctx, "\n## ") != 1 {
		t.Fatalf("len %d, memories %d", len(ctx), strings.Count(ctx, "\n## "))
	}
	te.Getenv = func(k string) string {
		return map[string]string{"CLAUDE_DREAM_INJECT_CHARS": "999999"}[k]
	}
	if injectBudget(te.Env) != maxInjectChars {
		t.Fatalf("hard cap = %d", injectBudget(te.Env))
	}
	te.Getenv = func(k string) string { return map[string]string{"CLAUDE_DREAM_INJECT_CHARS": "junk"}[k] }
	if injectBudget(te.Env) != defaultInjectChars {
		t.Fatal("junk override should fall back")
	}
}

func TestCompactReinjects(t *testing.T) {
	te := newEnv(t, nil)
	writeMem(t, te, "garden-api", "a.md", "Release flow", "x", "Tag then push.")
	runHook(t, te.Env, tool("s", te.root, map[string]any{"file_path": te.root + "/garden-api/a.go"}))
	if _, out := runHook(t, te.Env, event("SessionStart", "s", te.root, map[string]any{"source": "resume"})); out != "" {
		t.Fatalf("resume injected %q", out)
	}
	_, out := runHook(t, te.Env, event("SessionStart", "s", te.root, map[string]any{"source": "compact"}))
	if !strings.Contains(additional(t, out, "SessionStart"), "Release flow") {
		t.Fatalf("compact did not re-inject: %q", out)
	}
	if _, out = runHook(t, te.Env, tool("s", te.root, map[string]any{"file_path": te.root + "/garden-api/a.go"})); out != "" {
		t.Fatalf("after compact, repeat injected %q", out)
	}
}

func TestABOffArmWithholdsEverywhere(t *testing.T) {
	var onID, offID string
	for i := 0; onID == "" || offID == ""; i++ {
		id := "sess-" + itoa(i)
		if abArm(id) == "on" && onID == "" {
			onID = id
		} else if abArm(id) == "off" && offID == "" {
			offID = id
		}
	}
	te := newEnv(t, map[string]string{"CLAUDE_DREAM_AB": "1"})
	writeMem(t, te, "garden-api", "a.md", "Release flow", "How garden-api ships", "x")
	writeMem(t, te, "site-v2", "b.md", "Site facts", "How site-v2 ships", "x")
	writeMem(t, te, "", "r.md", "Billing webhook retries", "Stripe webhook retry schedule", "x")

	evs := []map[string]any{
		event("SessionStart", offID, te.root+"/garden-api", nil),
		prompt(offID, te.root, "look at site-v2 and the webhook retry schedule"),
		tool(offID, te.root, map[string]any{"file_path": te.root + "/site-v2/a.go"}),
	}
	for _, e := range evs {
		if _, out := runHook(t, te.Env, e); out != "" {
			t.Fatalf("off arm injected on %v: %q", e["hook_event_name"], out)
		}
	}
	log := readLog(t, te)
	if len(log) != 2 { // SessionStart and the prompt; the tool path was already in focus
		t.Fatalf("log = %v", log)
	}
	for _, l := range log {
		if l["arm"] != "off" || l["chars"].(float64) != 0 || l["withheld"] == nil || l["session_id"] != offID {
			t.Fatalf("log line = %v", l)
		}
	}
	if _, out := runHook(t, te.Env, event("SessionStart", onID, te.root+"/garden-api", nil)); out == "" {
		t.Fatal("on arm injected nothing")
	}
	if l := readLog(t, te); l[len(l)-1]["arm"] != "on" || l[len(l)-1]["repos"] == nil {
		t.Fatalf("log = %v", l)
	}
}

func TestStateDirAndPrune(t *testing.T) {
	if got := stateDirFor("/x/state", "/Users/alex"); got != "/x/state/claude-dream" {
		t.Fatalf("xdg: %q", got)
	}
	if got := stateDirFor("", "/Users/alex"); got != "/Users/alex/.local/state/claude-dream" {
		t.Fatalf("default: %q", got)
	}
	te := newEnv(t, nil)
	dir := filepath.Join(te.state, "sessions")
	_ = os.MkdirAll(dir, 0o755)
	oldF, newF := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	_ = os.WriteFile(oldF, []byte("{}"), 0o644)
	_ = os.WriteFile(newF, []byte("{}"), 0o644)
	past := te.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(oldF, past, past)
	runHook(t, te.Env, event("SessionStart", "s", te.root, nil))
	if _, err := os.Stat(oldF); err == nil {
		t.Fatal("old session file kept")
	}
	if _, err := os.Stat(newF); err != nil {
		t.Fatal("recent session file removed")
	}
}
