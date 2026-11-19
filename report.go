package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// reportCmd is `claude-dream report [--days N]`: what happened recently and
// what is waiting for review. It is the first thing to read.
func reportCmd(args []string, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	days := fs.Int("days", 1, "days to cover")
	if fs.Parse(args) != nil || *days < 1 {
		return 2
	}
	cutoff := env.Now().Add(-time.Duration(*days) * 24 * time.Hour)
	inRange := func(ts string) bool {
		t, err := time.Parse(time.RFC3339, ts)
		return err == nil && !t.Before(cutoff)
	}
	w := func(format string, a ...any) { _, _ = fmt.Fprintf(stdout, format, a...) }
	w("claude-dream report, last %d day(s)\n", *days)

	// Injections.
	var events int
	arms := map[string]map[string]bool{}
	top := map[string]int{}
	_ = eachLine(filepath.Join(env.StateDir, "injections.jsonl"), func(line []byte, _ int64) {
		var e struct {
			TS       string   `json:"ts"`
			Session  string   `json:"session_id"`
			Arm      string   `json:"arm"`
			Memories []string `json:"memories"`
		}
		if json.Unmarshal(line, &e) != nil || !inRange(e.TS) {
			return
		}
		events++
		if arms[e.Arm] == nil {
			arms[e.Arm] = map[string]bool{}
		}
		arms[e.Arm][e.Session] = true
		for _, m := range e.Memories {
			top[m]++
		}
	})
	w("\nInjections: %d events", events)
	for _, arm := range sortedKeys(arms) {
		w(", %d sessions in arm %s", len(arms[arm]), arm)
	}
	w("\n")
	names := sortedKeys(top)
	sort.SliceStable(names, func(i, j int) bool { return top[names[i]] > top[names[j]] })
	for i, n := range names {
		if i == 10 {
			break
		}
		w("  %3d  %s\n", top[n], n)
	}

	// Extractions.
	var runs, proposed, written int
	dropped, skipped, errs := map[string]int{}, map[string]int{}, map[string]int{}
	propDirs := map[string]bool{}
	_ = eachLine(filepath.Join(env.StateDir, "extract.jsonl"), func(line []byte, _ int64) {
		var e extractLog
		if json.Unmarshal(line, &e) != nil {
			return
		}
		for _, p := range e.Proposed {
			propDirs[filepath.Dir(p)] = true
		}
		if !inRange(e.TS) {
			return
		}
		runs++
		proposed += len(e.Proposed)
		written += len(e.Written)
		for k, v := range e.Dropped {
			dropped[k] += v
		}
		if e.Skipped != "" {
			skipped[e.Skipped]++
		}
		if e.Error != "" {
			errs[firstLine(e.Error)]++
		}
	})
	w("\nExtractions: %d runs, %d proposed, %d written\n", runs, proposed, written)
	w("  dropped: %s\n  skipped: %s\n  errors: %s\n", formatCounts(dropped), formatCounts(skipped), formatCounts(errs))
	var c callCount
	if b, err := os.ReadFile(callsFile(env)); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Day != env.Now().Local().Format("2006-01-02") {
		c.Count = 0
	}
	w("  LLM calls today: %d of %d\n", c.Count, maxCalls(env))

	// Dream.
	reports, _ := filepath.Glob(filepath.Join(env.StateDir, "dream", "*.md"))
	sort.Strings(reports)
	if len(reports) == 0 {
		w("\nDream: no report yet\n")
	} else {
		w("\nDream: latest report %s\n", tildePath(env, reports[len(reports)-1]))
	}

	// Proposals awaiting review.
	for _, d := range dreamDirs(env) {
		propDirs[filepath.Join(d, ".proposed")] = true
	}
	var props []string
	for _, d := range sortedKeys(propDirs) {
		m, _ := filepath.Glob(filepath.Join(d, "*.md"))
		props = append(props, m...)
	}
	sort.Strings(props)
	w("\nProposals awaiting review: %d\n", len(props))
	for _, p := range props {
		w("  %s\n", tildePath(env, p))
	}
	return 0
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// dreamCmd is `claude-dream dream [--dry-run]`. A dry run prints the review
// list and writes nothing; a real run takes the worker lock.
func dreamCmd(args []string, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("dream", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry-run", false, "print the review list; write nothing, call no LLM")
	if fs.Parse(args) != nil {
		return 2
	}
	if !*dry {
		if mode(env, "CLAUDE_DREAM_DREAM") == "off" {
			_, _ = fmt.Fprintln(stderr, "claude-dream: dream is off (CLAUDE_DREAM_DREAM=off)")
			return 0
		}
		lock := acquireLock(env.StateDir, env.Now())
		if lock == nil {
			_, _ = fmt.Fprintln(stderr, "claude-dream: the worker is running; try again later")
			return 1
		}
		defer lock.release()
	}
	text, counts := dreamPass(context.Background(), env, *dry)
	if *dry {
		_, _ = io.WriteString(stdout, text)
	} else {
		_, _ = fmt.Fprintf(stdout, "wrote %s\n", tildePath(env, filepath.Join(env.StateDir, "dream", env.Now().Local().Format("2006-01-02")+".md")))
	}
	_, _ = fmt.Fprintf(stdout, "counts: %s\n", formatCounts(counts))
	return 0
}

// dryRunResult is what `extract --dry-run` prints, as JSON.
type dryRunResult struct {
	Transcript   string         `json:"transcript"`
	HumanTurns   int            `json:"human_turns"`
	Chunks       int            `json:"chunks"`
	Candidates   []dryCandidate `json:"candidates"`
	Dropped      map[string]int `json:"dropped"`
	NotInHuman   int            `json:"quotes_not_in_human_turns"`
	AlsoNonHuman int            `json:"quotes_also_in_non_human_text"`
	Skipped      string         `json:"skipped,omitempty"`
	Error        string         `json:"error,omitempty"`
}

type dryCandidate struct {
	candidate
	AlsoInNonHuman bool `json:"quote_also_in_non_human_text"`
}

// extractCmd is `claude-dream extract --dry-run <transcript>`: run the real
// extraction over one transcript and print the candidates. Nothing is
// written except the LLM call count; offsets are ignored.
func extractCmd(args []string, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry-run", false, "print candidates; write nothing")
	if fs.Parse(args) != nil || !*dry || fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: claude-dream extract --dry-run <transcript.jsonl>")
		return 2
	}
	path := fs.Arg(0)
	res := dryRunResult{Transcript: filepath.Base(path), Dropped: map[string]int{}, Candidates: []dryCandidate{}}
	defer func() {
		b, _ := json.MarshalIndent(res, "", "  ")
		_, _ = fmt.Fprintln(stdout, string(b))
	}()
	sc, err := scanTranscript(path, true)
	if err != nil {
		res.Error = err.Error()
		return 1
	}
	if sc.Headless {
		res.Skipped = "headless"
		return 0
	}
	res.HumanTurns = len(sc.Turns)
	if len(sc.Turns) < 2 {
		res.Skipped = "few_turns"
		return 0
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	proj, glob, label := targetDirs(env, session, sc.Cwd)
	if proj == "" && glob == "" {
		res.Skipped = "no_memory_dir"
		return 0
	}
	ix := newMemIndex(env, proj, glob, label)
	var others []string
	for _, o := range sc.Others {
		others = append(others, normQuote(o))
	}
	for _, chunk := range chunkTurns(sc.Turns) {
		cands, err := runChunk(context.Background(), env, chunk, ix, res.Dropped)
		if err != nil {
			res.Error = err.Error()
			return 1
		}
		res.Chunks++
		for _, c := range cands {
			q := normQuote(c.EvidenceQuote)
			inHuman := false
			for _, t := range sc.Turns {
				inHuman = inHuman || strings.Contains(normQuote(t.Text), q)
			}
			if !inHuman {
				res.NotInHuman++
			}
			d := dryCandidate{candidate: c}
			for _, o := range others {
				if strings.Contains(o, q) {
					d.AlsoInNonHuman = true
					res.AlsoNonHuman++
					break
				}
			}
			res.Candidates = append(res.Candidates, d)
		}
	}
	return 0
}
