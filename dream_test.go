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

// dreamFixture is one repo memory dir with four memories, a stale index and
// a dead home path. Home is the test's temp dir.
type dreamFixture struct {
	te    *testEnv
	repo  string
	dir   string
	files map[string]string // name -> path
}

func newDreamFixture(t *testing.T, vars map[string]string) *dreamFixture {
	t.Helper()
	te := newEnv(t, vars)
	f := &dreamFixture{te: te, repo: mkRepo(t, te, "garden-api"), files: map[string]string{}}
	if err := os.MkdirAll(filepath.Join(te.Home, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(te.Home, "notes", "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := func(file, desc, body string) {
		f.files[file] = writeMemAt(t, te, f.repo, file, strings.TrimSuffix(file, ".md"), desc, body, "project")
	}
	mk("seed-catalog.md", "where the garden-api seed catalog lives", "The catalog is in `~/notes/` and `~/old-garden/seeds.csv`; prod copy on `/srv/garden/seeds`.")
	mk("deploy-flow.md", "garden-api deploys go through the staging pipeline first", "Staging first, then prod.")
	mk("deploy-flow-copy.md", "garden-api deploys go through the staging pipeline", "Same thing, older wording.")
	mk("tag-format.md", "release tags look like gardenapi-vX.Y.Z", "Tags: gardenapi-v1.2.3. See `~/notes/*.md` and `~/Code/<repo>/x`.")
	f.dir = filepath.Dir(f.files["seed-catalog.md"])
	idx := "# Memory\n\n- [Seed catalog](seed-catalog.md) — catalog\n- [Deploy flow](deploy-flow.md) — deploys\n- [Gone](removed-long-ago.md) — was deleted by hand\n- [Site](https://example.com/a.md) — external\n"
	if err := os.WriteFile(filepath.Join(f.dir, "MEMORY.md"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	old := te.Now().Add(-8 * 24 * time.Hour)
	fresh := te.Now().Add(-1 * time.Hour)
	for name, when := range map[string]time.Time{"2026-11-01-old-idea.md": old, "2026-11-18-new-idea.md": fresh} {
		p := filepath.Join(f.dir, ".proposed", name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("---\nname: x\n---\nx\n"), 0o644)
		_ = os.Chtimes(p, when, when)
	}
	return f
}

// snapshot records every file's bytes under the memory dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func staleAnswer(file string) string {
	b, _ := json.Marshal([]map[string]any{
		{"kind": "stale", "files": []string{file}, "reason": "the catalog moved", "suggested_text": ""},
		{"kind": "stale", "files": []string{"deploy-flow.md"}, "reason": "no dead path backs this", "suggested_text": ""},
		{"kind": "merge", "files": []string{"deploy-flow.md", "deploy-flow-copy.md"}, "reason": "same fact",
			"suggested_text": "garden-api deploys go to staging first, then prod.", "description": "garden-api deploys: staging then prod"},
		{"kind": "contradiction", "files": []string{"deploy-flow.md", "../../escape.md"}, "reason": "invented file"},
		{"kind": "bogus", "files": []string{"deploy-flow.md"}},
	})
	return string(b)
}

func dreamCounts(t *testing.T, te *testEnv, args ...string) (string, map[string]int) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := subcommand("dream", args, &out, &errb, te.Env); code != 0 {
		t.Fatalf("dream: %d %s", code, errb.String())
	}
	counts := map[string]int{}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	last := lines[len(lines)-1]
	for _, kv := range strings.Fields(strings.TrimPrefix(last, "counts: ")) {
		k, v, _ := strings.Cut(kv, "=")
		var n int
		_ = json.Unmarshal([]byte(v), &n)
		counts[k] = n
	}
	return out.String(), counts
}

func TestDreamProposeTouchesNothing(t *testing.T) {
	f := newDreamFixture(t, nil)
	f.te.llm.answers = []string{staleAnswer("seed-catalog.md")}
	before := snapshot(t, f.te.proj)
	_, counts := dreamCounts(t, f.te)
	if after := snapshot(t, f.te.proj); len(after) != len(before) {
		t.Fatalf("files changed: %d -> %d", len(before), len(after))
	} else {
		for p, b := range before {
			if after[p] != b {
				t.Fatalf("%s changed in propose mode", p)
			}
		}
	}
	want := map[string]int{"index_missing": 2, "index_dead": 1, "dead_anchor": 1, "duplicate": 1, "pending_proposal": 1,
		"stale": 2, "merge": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("%s = %d, want %d (%v)", k, counts[k], v, counts)
		}
	}
	if counts["contradiction"] != 0 {
		t.Error("an item naming a missing file was kept")
	}
	rep, err := os.ReadFile(filepath.Join(f.te.state, "dream", "2026-11-19.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`~/old-garden/seeds.csv` does not exist", "would archive to .archive/", "would append a pointer", "`removed-long-ago.md`"} {
		if !strings.Contains(string(rep), want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if strings.Contains(string(rep), "/srv/garden") || strings.Contains(string(rep), "~/notes") {
		t.Error("existing or remote paths flagged")
	}
	if f.te.llm.systems[0] != dreamSystemPrompt {
		t.Error("wrong system prompt")
	}
	// A second run makes no LLM call: nothing changed since the check.
	_, _ = dreamCounts(t, f.te)
	if len(f.te.llm.prompts) != 1 {
		t.Fatalf("calls = %d", len(f.te.llm.prompts))
	}
}

func TestDreamApply(t *testing.T) {
	f := newDreamFixture(t, map[string]string{"CLAUDE_DREAM_DREAM": "apply"})
	f.te.llm.answers = []string{staleAnswer("seed-catalog.md")}
	body := func(name string) string {
		b, _ := os.ReadFile(f.files[name])
		return parseMemory(string(b)).Body
	}
	flowBody := body("deploy-flow.md")
	dreamCounts(t, f.te)

	if _, err := os.Stat(f.files["seed-catalog.md"]); err == nil {
		t.Fatal("confirmed-stale file not archived")
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".archive", "seed-catalog.md")); err != nil {
		t.Fatal("archive missing")
	}
	if _, err := os.Stat(f.files["deploy-flow.md"]); err != nil {
		t.Fatal("stale without a dead path was archived")
	}
	merged, err := os.ReadFile(filepath.Join(f.dir, "deploy-flow-v2.md"))
	if err != nil || !strings.Contains(string(merged), "Merged from: `deploy-flow.md`, `deploy-flow-copy.md`") {
		t.Fatalf("merge: %v\n%s", err, merged)
	}
	for _, n := range []string{"deploy-flow.md", "deploy-flow-copy.md"} {
		b, _ := os.ReadFile(f.files[n])
		if parseMemory(string(b)).SupersededBy != "deploy-flow-v2" {
			t.Fatalf("%s not superseded", n)
		}
	}
	if body("deploy-flow.md") != flowBody {
		t.Fatal("source body changed")
	}
	idx, _ := os.ReadFile(filepath.Join(f.dir, "MEMORY.md"))
	s := string(idx)
	for _, want := range []string{"(tag-format.md)", "(deploy-flow-v2.md)", "https://example.com/a.md"} {
		if !strings.Contains(s, want) {
			t.Errorf("index lacks %s:\n%s", want, s)
		}
	}
	for _, gone := range []string{"removed-long-ago.md", "(seed-catalog.md)"} {
		if strings.Contains(s, gone) {
			t.Errorf("index still has %s:\n%s", gone, s)
		}
	}
	if strings.Contains(s, "(deploy-flow-copy.md)") {
		t.Error("superseded file was added to the index")
	}
	// Proposals are never removed by the dream.
	if _, err := os.Stat(filepath.Join(f.dir, ".proposed", "2026-11-01-old-idea.md")); err != nil {
		t.Fatal("proposal removed")
	}
}

func TestDreamDryRunWritesNothingAndCallsNoLLM(t *testing.T) {
	f := newDreamFixture(t, map[string]string{"CLAUDE_DREAM_DREAM": "apply"})
	before := snapshot(t, f.te.proj)
	out, counts := dreamCounts(t, f.te, "--dry-run")
	if len(f.te.llm.prompts) != 0 || counts["llm_pending"] != 1 || counts["index_dead"] != 1 {
		t.Fatalf("calls %d counts %v", len(f.te.llm.prompts), counts)
	}
	if !strings.Contains(out, "dry run") {
		t.Fatal("report not printed")
	}
	after := snapshot(t, f.te.proj)
	for p, b := range before {
		if after[p] != b {
			t.Fatalf("%s changed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.te.state, "dream")); err == nil {
		t.Fatal("dry run wrote state")
	}
}

func TestWorkerDreamsAfter20Hours(t *testing.T) {
	te := newEnv(t, nil)
	last := filepath.Join(te.state, "dream.last")
	work(te.Env)
	st, err := os.Stat(last)
	if err != nil || !st.ModTime().Equal(te.Now()) {
		t.Fatalf("first run did not dream: %v", err)
	}
	report := filepath.Join(te.state, "dream", "2026-11-19.md")
	_ = os.Remove(report)
	start := te.Now()
	te.Now = func() time.Time { return start.Add(18 * time.Hour) }
	work(te.Env)
	if _, err := os.Stat(report); err == nil {
		t.Fatal("dreamed again within 20 h")
	}
	te.Now = func() time.Time { return start.Add(21 * time.Hour) }
	work(te.Env)
	if _, err := os.Stat(filepath.Join(te.state, "dream", te.Now().Local().Format("2006-01-02")+".md")); err != nil {
		t.Fatal("did not dream after 20 h")
	}
	// Off means never.
	te2 := newEnv(t, map[string]string{"CLAUDE_DREAM_DREAM": "off"})
	work(te2.Env)
	if _, err := os.Stat(filepath.Join(te2.state, "dream.last")); err == nil {
		t.Fatal("dream ran while off")
	}
}

func TestDreamCapStopsLLMChecks(t *testing.T) {
	f := newDreamFixture(t, map[string]string{"CLAUDE_DREAM_MAX_CALLS": "1"})
	if !takeCall(f.te.Env) {
		t.Fatal("first call refused")
	}
	_, counts := dreamCounts(t, f.te)
	if counts["llm_capped"] != 1 || len(f.te.llm.prompts) != 0 || counts["index_dead"] != 1 {
		t.Fatalf("counts %v", counts)
	}
}

func TestReportCommand(t *testing.T) {
	f := newExtractFixture(t, nil)
	f.te.llm.answers = []string{itemJSON(t, addItem("no-laptop-migrations", turnA), map[string]any{"action": "noop"})}
	f.end(t)
	writeMem(t, f.te, "garden-api", "release.md", "garden-api release", "how garden-api releases", "body")
	runHook(t, f.te.Env, event("SessionStart", "s9", filepath.Join(f.te.root, "garden-api"), nil))
	var out, errb bytes.Buffer
	if code := subcommand("report", []string{"--days", "2"}, &out, &errb, f.te.Env); code != 0 {
		t.Fatal(errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Injections: 1 events, 1 sessions in arm on",
		"Extractions: 1 runs, 1 proposed, 0 written",
		"dropped: noop=1",
		"LLM calls today: 1 of 40",
		"Dream: latest report ",
		"Proposals awaiting review: 1",
		".proposed/2026-11-19-no-laptop-migrations.md",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report lacks %q:\n%s", want, s)
		}
	}
	if code := subcommand("report", []string{"--days", "0"}, &out, &errb, f.te.Env); code != 2 {
		t.Fatal("bad --days accepted")
	}
}
