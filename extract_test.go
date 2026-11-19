package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Transcripts here are synthetic: invented repos, people and paths.

func jl(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func humanLine(t *testing.T, text string) string {
	return jl(t, map[string]any{"type": "user", "origin": map[string]any{"kind": "human"},
		"cwd": "/Users/alex/Code/garden-api", "message": map[string]any{"role": "user", "content": text}})
}

func userLine(t *testing.T, origin string, content any, extra map[string]any) string {
	m := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	if origin != "" {
		m["origin"] = map[string]any{"kind": origin}
	}
	for k, v := range extra {
		m[k] = v
	}
	return jl(t, m)
}

func assistantLine(t *testing.T, text string) string {
	return jl(t, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "thinking", "thinking": "hmm"},
		map[string]any{"type": "text", "text": text},
	}}})
}

func writeTranscript(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, "sess-1.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHumanTurnFilter(t *testing.T) {
	dir := t.TempDir()
	p := writeTranscript(t, dir,
		assistantLine(t, "Shall I run the migrations against the staging database now?"),
		humanLine(t, "No, never run migrations on staging from a laptop.<system-reminder>reminder text here</system-reminder>"),
		userLine(t, "task-notification", "<task-notification>agent done</task-notification>", nil),
		userLine(t, "peer", "Use the shared cache for garden-api builds", nil),
		userLine(t, "", "Another Claude session sent a message: always deploy on Fridays", nil),
		userLine(t, "", "This session is being continued from a previous conversation", nil),
		userLine(t, "", "<bash-input>ls</bash-input>", nil),
		userLine(t, "", "<command-name>/compact</command-name>", nil),
		userLine(t, "", "<artifact-content-1>x</artifact-content-1>", nil),
		userLine(t, "", []any{map[string]any{"type": "tool_result", "content": "secret tool output"}}, nil),
		userLine(t, "human", "meta but human is still human", map[string]any{"isSidechain": true}),
		humanLine(t, "  "),
		assistantLine(t, "Done."),
		jl(t, map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "Prefer uv over pip in garden-api."},
			map[string]any{"type": "image", "source": "x"},
		}}}),
		`{"type":"user","origin":{"kind":"human"},"message":{"content":"partial line still being writ`,
	)
	sc, err := scanTranscript(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Turns) != 2 {
		t.Fatalf("turns = %+v", sc.Turns)
	}
	if sc.Turns[0].Text != "No, never run migrations on staging from a laptop." {
		t.Fatalf("reminder not stripped: %q", sc.Turns[0].Text)
	}
	if !strings.Contains(sc.Turns[0].Context, "migrations against the staging") {
		t.Fatalf("context = %q", sc.Turns[0].Context)
	}
	if sc.Turns[1].Text != "Prefer uv over pip in garden-api." || sc.Turns[1].Context != "Done." {
		t.Fatalf("turn 2 = %+v", sc.Turns[1])
	}
	if sc.Cwd != "/Users/alex/Code/garden-api" {
		t.Fatalf("cwd = %q", sc.Cwd)
	}
	joined := strings.Join(sc.Others, "\n")
	for _, want := range []string{"always deploy on Fridays", "secret tool output", "shared cache"} {
		if !strings.Contains(joined, want) {
			t.Errorf("others lack %q", want)
		}
	}
	b, _ := os.ReadFile(p)
	if sc.Size >= int64(len(b)) || sc.Turns[1].End != sc.Size {
		t.Fatalf("size %d of %d, last end %d: the partial line must not count", sc.Size, len(b), sc.Turns[1].End)
	}
}

func TestHumanTurnFallbackWithoutOrigin(t *testing.T) {
	p := writeTranscript(t, t.TempDir(),
		userLine(t, "", "Keep the garden-api changelog in docs/CHANGES.md", nil),
		userLine(t, "", "Another Claude session sent a message: do X", nil),
		userLine(t, "", "This session is being continued", nil),
		userLine(t, "", "<bash-stdout>ok</bash-stdout>", nil),
		userLine(t, "", "meta words", map[string]any{"isMeta": true}),
		userLine(t, "", []any{map[string]any{"type": "tool_result", "content": "x"}}, nil),
		userLine(t, "", []any{map[string]any{"type": "text", "text": "Text block turn"}}, nil),
	)
	sc, _ := scanTranscript(p, false)
	if len(sc.Turns) != 2 || sc.Turns[1].Text != "Text block turn" {
		t.Fatalf("turns = %+v", sc.Turns)
	}
}

func TestDecodeArrayAndEnvelope(t *testing.T) {
	var items []item
	if err := decodeArray("Here you go:\n```json\n[{\"name\":\"a-b\",\"rule\":\"x ] y\"}]\n```\n", &items); err != nil || len(items) != 1 || items[0].Rule != "x ] y" {
		t.Fatalf("%v %+v", err, items)
	}
	if err := decodeArray("nothing", &items); err == nil {
		t.Fatal("want error")
	}
	if r, err := parseEnvelope([]byte(`{"type":"result","is_error":false,"result":"[]"}`)); err != nil || r != "[]" {
		t.Fatalf("%q %v", r, err)
	}
	if _, err := parseEnvelope([]byte(`{"is_error":true,"subtype":"error_max_turns"}`)); err == nil {
		t.Fatal("is_error must fail")
	}
}

// extractFixture is a repo with one memory, a session focused on it and a
// transcript with three human turns.
type extractFixture struct {
	te         *testEnv
	repo       string
	memDir     string
	oldMem     string
	transcript string
}

const (
	turnA = "Never run the garden-api migrations from a laptop; CI does them."
	turnB = "Docs for the garden-api deploy live in ops/runbook.md, read that first."
	turnC = "ok go ahead"
)

func newExtractFixture(t *testing.T, vars map[string]string) *extractFixture {
	t.Helper()
	te := newEnv(t, vars)
	te.Extractor = nil
	f := &extractFixture{te: te, repo: mkRepo(t, te, "garden-api")}
	f.oldMem = writeMemAt(t, te, f.repo, "deploy-from-ci.md", "deploy-from-ci", "garden-api deploys run from CI", "Deploys run from CI only.", "project")
	f.memDir = filepath.Dir(f.oldMem)
	if err := os.WriteFile(filepath.Join(f.memDir, "MEMORY.md"), []byte("# Memory\n\n- [Deploy from CI](deploy-from-ci.md) — garden-api deploys run from CI\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.transcript = writeTranscript(t, t.TempDir(),
		assistantLine(t, "I could run the migrations from here; staging is reachable."),
		humanLine(t, turnA),
		assistantLine(t, "Understood."),
		humanLine(t, turnB),
		humanLine(t, turnC),
	)
	return f
}

func (f *extractFixture) end(t *testing.T) {
	t.Helper()
	runHook(t, f.te.Env, event("SessionEnd", "sess-1", f.repo, map[string]any{"transcript_path": f.transcript}))
	work(f.te.Env)
}

func itemJSON(t *testing.T, items ...map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(items)
	return string(b)
}

func addItem(name, quote string) map[string]any {
	return map[string]any{"action": "add", "scope": "project", "type": "feedback", "name": name,
		"description": "garden-api: no migrations from laptops", "rule": "Run garden-api migrations only from CI.",
		"why": "Laptops are not trusted for staging.", "how_to_apply": "When asked to migrate, point at CI.", "evidence_quote": quote}
}

func readExtractLog(t *testing.T, te *testEnv) []extractLog {
	t.Helper()
	var out []extractLog
	_ = eachLine(filepath.Join(te.state, "extract.jsonl"), func(line []byte, _ int64) {
		var e extractLog
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	})
	return out
}

func TestExtractProposeIsDefault(t *testing.T) {
	f := newExtractFixture(t, nil)
	f.te.llm.answers = []string{itemJSON(t,
		addItem("no-laptop-migrations", "never run the garden-api   migrations from a laptop"), // case and spaces differ: passes
		addItem("staging-reachable", "I could run the migrations from here"),                   // assistant context: fails
		addItem("short-quote", "ok go ahead"),                                                  // too short
		map[string]any{"action": "noop"},
		addItem("Bad Name", "Docs for the garden-api deploy live in ops/runbook.md"),
	)}
	before, _ := os.ReadFile(filepath.Join(f.memDir, "MEMORY.md"))
	f.end(t)

	if len(f.te.llm.prompts) != 1 {
		t.Fatalf("calls = %d", len(f.te.llm.prompts))
	}
	pr := f.te.llm.prompts[0]
	for _, want := range []string{"deploy-from-ci | project | garden-api deploys run from CI", "USER:\n" + turnA, "CONTEXT (assistant, not evidence):\nI could run"} {
		if !strings.Contains(pr, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if f.te.llm.systems[0] != extractSystemPrompt {
		t.Error("wrong system prompt")
	}
	prop := filepath.Join(f.memDir, ".proposed", "2026-11-19-no-laptop-migrations.md")
	b, err := os.ReadFile(prop)
	if err != nil {
		t.Fatal(err)
	}
	m := parseMemory(string(b))
	if m.Name != "no-laptop-migrations" || m.Type != "feedback" || !strings.Contains(string(b), "proposed_action: add") ||
		!strings.Contains(m.Body, "**Why:** Laptops") || !strings.Contains(m.Body, `Evidence: "never run the garden-api migrations from a laptop" (session sess-1, 2026-11-19)`) {
		t.Fatalf("proposal:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(f.memDir, "no-laptop-migrations.md")); err == nil {
		t.Fatal("propose mode wrote a live file")
	}
	after, _ := os.ReadFile(filepath.Join(f.memDir, "MEMORY.md"))
	if !bytes.Equal(before, after) {
		t.Fatal("propose mode changed MEMORY.md")
	}
	lg := readExtractLog(t, f.te)
	if len(lg) != 1 || lg[0].Mode != "propose" || lg[0].Turns != 3 || lg[0].Chunks != 1 || len(lg[0].Proposed) != 1 {
		t.Fatalf("log = %+v", lg)
	}
	want := map[string]int{"quote_not_in_human_turn": 1, "quote_too_short": 1, "noop": 1, "bad_name": 1}
	for k, v := range want {
		if lg[0].Dropped[k] != v {
			t.Errorf("dropped[%s] = %d, want %d (%v)", k, lg[0].Dropped[k], v, lg[0].Dropped)
		}
	}
	// The proposal is never injected.
	_, out := runHook(t, f.te.Env, event("SessionStart", "other", f.repo, nil))
	if strings.Contains(out, "no-laptop-migrations") {
		t.Fatal("proposal injected")
	}
}

func TestExtractApplyAddsFileAndPointer(t *testing.T) {
	f := newExtractFixture(t, map[string]string{"CLAUDE_DREAM_EXTRACT": "apply"})
	f.te.llm.answers = []string{itemJSON(t, addItem("no-laptop-migrations", turnA))}
	f.end(t)
	p := filepath.Join(f.memDir, "no-laptop-migrations.md")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "proposed_") || parseMemory(string(b)).Description != "garden-api: no migrations from laptops" {
		t.Fatalf("file:\n%s", b)
	}
	idx, _ := os.ReadFile(filepath.Join(f.memDir, "MEMORY.md"))
	if !strings.HasSuffix(string(idx), "- [No laptop migrations](no-laptop-migrations.md) — garden-api: no migrations from laptops\n") ||
		!strings.Contains(string(idx), "(deploy-from-ci.md)") {
		t.Fatalf("index:\n%s", idx)
	}
	if lg := readExtractLog(t, f.te); len(lg[0].Written) != 1 {
		t.Fatalf("log = %+v", lg)
	}
}

func TestExtractUpdateWritesV2AndSupersedes(t *testing.T) {
	f := newExtractFixture(t, map[string]string{"CLAUDE_DREAM_EXTRACT": "apply"})
	// An unrelated file already uses the -v2 name: the next free one is -v3.
	writeMemAt(t, f.te, f.repo, "deploy-from-ci-v2.md", "deploy-from-ci-v2", "garden-api other", "x", "project")
	if err := supersede(filepath.Join(f.memDir, "deploy-from-ci-v2.md"), "elsewhere", time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f.oldMem)
	it := addItem("ignored-for-updates", turnA)
	it["action"], it["target"] = "supersede", "deploy-from-ci"
	f.te.llm.answers = []string{itemJSON(t, it)}
	f.end(t)
	nb, err := os.ReadFile(filepath.Join(f.memDir, "deploy-from-ci-v3.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nb), "Replaces: `deploy-from-ci.md`") {
		t.Fatalf("new file does not name its source:\n%s", nb)
	}
	after, _ := os.ReadFile(f.oldMem)
	om := parseMemory(string(after))
	if om.SupersededBy != "deploy-from-ci-v3" || om.Body != parseMemory(string(before)).Body {
		t.Fatalf("old file:\n%s", after)
	}
}

func TestExtractRejectsUnknownTargetAndCollision(t *testing.T) {
	f := newExtractFixture(t, map[string]string{"CLAUDE_DREAM_EXTRACT": "apply"})
	upd := addItem("x", turnA)
	upd["action"], upd["target"] = "update", "../../etc/passwd"
	f.te.llm.answers = []string{itemJSON(t, upd, addItem("deploy-from-ci", turnB), addItem("fresh-one", turnB), addItem("fresh-one", turnA))}
	before, _ := os.ReadFile(f.oldMem)
	f.end(t)
	lg := readExtractLog(t, f.te)
	if lg[0].Dropped["unknown_target"] != 1 || lg[0].Dropped["name_collision"] != 2 || len(lg[0].Written) != 1 {
		t.Fatalf("log = %+v", lg[0])
	}
	if after, _ := os.ReadFile(f.oldMem); !bytes.Equal(before, after) {
		t.Fatal("colliding add touched the existing file")
	}
}

func TestExtractOff(t *testing.T) {
	f := newExtractFixture(t, map[string]string{"CLAUDE_DREAM_EXTRACT": "off"})
	f.end(t)
	if len(f.te.llm.prompts) != 0 {
		t.Fatal("off mode called the model")
	}
	if _, err := os.Stat(filepath.Join(f.memDir, ".proposed")); err == nil {
		t.Fatal("off mode proposed")
	}
}

func TestExtractOffsetsPreventRework(t *testing.T) {
	f := newExtractFixture(t, nil)
	f.end(t)
	f.end(t) // PostCompact then SessionEnd: nothing new
	if len(f.te.llm.prompts) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.te.llm.prompts))
	}
	// One more human turn is not enough on its own; it waits.
	add := func(s string) {
		fh, _ := os.OpenFile(f.transcript, os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = fh.WriteString(s)
		_ = fh.Close()
	}
	add(humanLine(t, "Use the blue-green switch for garden-api releases."))
	f.end(t)
	if len(f.te.llm.prompts) != 1 {
		t.Fatal("one new turn should not call the model")
	}
	add(humanLine(t, "And tag releases as gardenapi-vX.Y.Z."))
	f.end(t)
	if len(f.te.llm.prompts) != 2 {
		t.Fatalf("calls = %d, want 2", len(f.te.llm.prompts))
	}
	p := f.te.llm.prompts[1]
	if strings.Contains(p, turnA) || !strings.Contains(p, "blue-green") || !strings.Contains(p, "gardenapi-vX.Y.Z") {
		t.Fatalf("second prompt has the wrong turns:\n%s", p)
	}
	lg := readExtractLog(t, f.te)
	if lg[1].Skipped != "few_turns" || lg[2].Skipped != "few_turns" {
		t.Fatalf("log = %+v", lg)
	}
}

func TestExtractSkipsWorkerSessionAndFewTurns(t *testing.T) {
	f := newExtractFixture(t, nil)
	runHook(t, f.te.Env, event("SessionEnd", "w", f.te.state, map[string]any{"transcript_path": f.transcript}))
	work(f.te.Env)
	if len(f.te.llm.prompts) != 0 {
		t.Fatal("worker's own session extracted")
	}
	if lg := readExtractLog(t, f.te); len(lg) != 1 || lg[0].Skipped != "worker_session" {
		t.Fatalf("log = %+v", lg)
	}
}

func TestExtractCapLeavesJobsQueued(t *testing.T) {
	f := newExtractFixture(t, map[string]string{"CLAUDE_DREAM_MAX_CALLS": "1"})
	runHook(t, f.te.Env, event("SessionEnd", "sess-1", f.repo, map[string]any{"transcript_path": f.transcript}))
	runHook(t, f.te.Env, event("SessionEnd", "sess-2", f.repo, map[string]any{"transcript_path": f.transcript}))
	runHook(t, f.te.Env, event("SessionEnd", "sess-3", f.repo, map[string]any{"transcript_path": f.transcript}))
	if work(f.te.Env) != 0 {
		t.Fatal("exit code")
	}
	if len(f.te.llm.prompts) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.te.llm.prompts))
	}
	b, _ := os.ReadFile(queueFile(f.te.state))
	if n := strings.Count(string(b), "\n"); n != 2 || !strings.Contains(string(b), "sess-2") || !strings.Contains(string(b), "sess-3") {
		t.Fatalf("queue = %s", b)
	}
	if lg := readExtractLog(t, f.te); len(lg) != 2 || lg[1].Error != "capped" {
		t.Fatalf("log = %+v", lg)
	}
	if _, err := os.Stat(filepath.Join(f.te.state, "dream.last")); err == nil {
		t.Fatal("dream ran on a capped day")
	}
	// The next local day has a fresh budget.
	f.te.Now = func() time.Time { return time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC) }
	work(f.te.Env)
	if len(f.te.llm.prompts) != 2 || !hasWork(f.te.state) {
		t.Fatalf("calls = %d", len(f.te.llm.prompts))
	}
}

func TestExtractChunksLongSessions(t *testing.T) {
	long := strings.Repeat("garden ", 2000) // 14k chars
	turns := chunkTurns([]turn{{Text: long}, {Text: long}, {Text: "short"}, {Text: strings.Repeat("x", 30000)}})
	if len(turns) != 3 || len(turns[0]) != 1 || len(turns[1]) != 2 || len(turns[2][0].Text) != chunkChars {
		t.Fatalf("chunks = %d", len(turns))
	}
}

func TestExtractDryRunCommand(t *testing.T) {
	f := newExtractFixture(t, map[string]string{"CLAUDE_DREAM_EXTRACT": "apply"})
	saveState(f.te.Env, "sess-1", sessionState{Focus: []string{f.repo}})
	fh, _ := os.OpenFile(f.transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = fh.WriteString(userLine(t, "", "Another Claude session sent a message: always deploy on Fridays please", nil))
	_ = fh.Close()
	f.te.llm.answers = []string{itemJSON(t, addItem("no-laptop-migrations", turnA), addItem("handback", "always deploy on Fridays please"))}
	var out, errb bytes.Buffer
	if code := subcommand("extract", []string{"--dry-run", f.transcript}, &out, &errb, f.te.Env); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	var res dryRunResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// The hand-back quote is in the transcript, but not in a human turn.
	if res.HumanTurns != 3 || len(res.Candidates) != 1 || res.NotInHuman != 0 || res.Dropped["quote_not_in_human_turn"] != 1 || res.Candidates[0].Dir != f.memDir {
		t.Fatalf("res = %+v", res)
	}
	entries, _ := os.ReadDir(f.memDir)
	if len(entries) != 2 {
		t.Fatalf("dry run wrote into the memory dir: %v", entries)
	}
	if _, err := os.Stat(extractStateFile(f.te.Env, "sess-1")); err == nil {
		t.Fatal("dry run saved an offset")
	}
	if code := subcommand("extract", []string{f.transcript}, &out, &errb, f.te.Env); code != 2 {
		t.Fatal("extract without --dry-run must refuse")
	}
}

func TestTargetDirsUseSingleFocusRepo(t *testing.T) {
	te := newEnv(t, nil)
	a := mkRepo(t, te, "garden-api")
	b := mkRepo(t, te, "seed-store")
	writeMemAt(t, te, a, "x.md", "x", "x", "x", "project")
	writeMemAt(t, te, te.root, "y.md", "y", "y", "y", "user")
	saveState(te.Env, "s", sessionState{Focus: []string{a}})
	proj, glob, label := targetDirs(te.Env, "s", te.root)
	if proj != memoryDir(te.Env, a) || glob != memoryDir(te.Env, te.root) || label != "garden-api" {
		t.Fatalf("%s %s %s", proj, glob, label)
	}
	saveState(te.Env, "s", sessionState{Focus: []string{a, b}})
	if proj, _, _ := targetDirs(te.Env, "s", te.root); proj != glob {
		t.Fatalf("two focus repos must use the cwd dir, got %s", proj)
	}
	// A focus repo with no memory dir falls back to the cwd's; none is created.
	saveState(te.Env, "s", sessionState{Focus: []string{b}})
	if proj, _, _ := targetDirs(te.Env, "s", te.root); proj != glob || isDir(memoryDir(te.Env, b)) {
		t.Fatal("fallback failed or a memory dir was created")
	}
}

func TestYAMLValue(t *testing.T) {
	for in, want := range map[string]string{
		"plain words":      "plain words",
		"has: colon":       `"has: colon"`,
		"- dash":           `"- dash"`,
		"multi\nline text": "multi line text",
		`"quoted" x`:       `"\"quoted\" x"`,
	} {
		if got := yamlValue(in); got != want {
			t.Errorf("yamlValue(%q) = %q, want %q", in, got, want)
		}
	}
}
