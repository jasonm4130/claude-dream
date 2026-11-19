package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	dreamEvery      = 20 * time.Hour
	proposalMaxAge  = 7 * 24 * time.Hour
	dupJaccard      = 0.6
	minLLMMemories  = 4
	dreamBodyChars  = 1500
	dreamInputChars = 24000
)

var (
	linkRe   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	anchorRe = regexp.MustCompile("`([^`\n]+)`")
	lineNoRe = regexp.MustCompile(`:[0-9]+(?::[0-9]+)?$`)
)

// finding is one item of the dream report.
type finding struct {
	Kind   string // index_missing, index_dead, dead_anchor, duplicate, pending_proposal, contradiction, merge, stale, llm_pending, llm_capped, llm_error
	Files  []string
	Detail string
	Action string // what apply mode does, or did
}

type dirReport struct {
	Dir      string
	Findings []finding
}

// dreamDirs lists the memory dirs in scope: each root and each direct child
// dir of a root, encoded with projectDirName. Names are never decoded.
func dreamDirs(env Env) []string {
	var out []string
	seen := map[string]bool{}
	addDir := func(path string) {
		d := memoryDir(env, path)
		if d != "" && !seen[d] && isDir(d) {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, r := range rootsOf(env) {
		addDir(r)
		entries, err := env.readDir(r)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") && (e.IsDir() || isDir(filepath.Join(r, e.Name()))) {
				addDir(filepath.Join(r, e.Name()))
			}
		}
	}
	return out
}

// maybeDream runs the dream pass from the worker when the last one is more
// than 20 hours old.
func maybeDream(ctx context.Context, env Env) {
	if mode(env, "CLAUDE_DREAM_DREAM") == "off" {
		return
	}
	if st, err := os.Stat(filepath.Join(env.StateDir, "dream.last")); err == nil && env.Now().Sub(st.ModTime()) < dreamEvery {
		return
	}
	dreamPass(ctx, env, false)
}

// dreamPass checks every memory dir in scope and writes the review list to
// stateDir/dream/<date>.md. In apply mode it fixes index drift, archives
// confirmed-stale files and writes merges. A dry run writes nothing at all
// and makes no LLM call. It returns the report and counts per kind.
func dreamPass(ctx context.Context, env Env, dryRun bool) (string, map[string]int) {
	md := mode(env, "CLAUDE_DREAM_DREAM")
	apply := md == "apply" && !dryRun
	checked := map[string]time.Time{}
	checkedPath := filepath.Join(env.StateDir, "dream", "checked.json")
	if b, err := os.ReadFile(checkedPath); err == nil {
		_ = json.Unmarshal(b, &checked)
	}
	counts := map[string]int{}
	var reports []dirReport
	capped := false
	for _, dir := range dreamDirs(env) {
		r := dreamDir(ctx, env, dir, apply, dryRun, checked, &capped)
		for _, f := range r.Findings {
			counts[f.Kind]++
		}
		if len(r.Findings) > 0 {
			reports = append(reports, r)
		}
	}
	label := md
	if dryRun {
		label += ", dry run"
	}
	text := renderDream(env, reports, counts, label)
	if dryRun {
		return text, counts
	}
	if b, err := json.Marshal(checked); err == nil {
		_ = writeAtomic(checkedPath, b)
	}
	_ = writeAtomic(filepath.Join(env.StateDir, "dream", env.Now().Local().Format("2006-01-02")+".md"), []byte(text))
	last := filepath.Join(env.StateDir, "dream.last")
	if writeAtomic(last, nil) == nil {
		_ = os.Chtimes(last, env.Now(), env.Now())
	}
	return text, counts
}

func dreamDir(ctx context.Context, env Env, dir string, apply, dryRun bool, checked map[string]time.Time, capped *bool) dirReport {
	r := dirReport{Dir: dir}
	add := func(f finding) { r.Findings = append(r.Findings, f) }
	verb := func(do, would string) string {
		if apply {
			return do
		}
		return "would " + would
	}
	live := loadMemories(env, dir, nil)
	byFile := map[string]Memory{}
	for _, m := range live {
		byFile[filepath.Base(m.Path)] = m
	}

	dead := map[string]bool{} // files with a dead anchor
	for _, m := range live {
		for _, a := range deadAnchors(env, m) {
			dead[filepath.Base(m.Path)] = true
			add(finding{Kind: "dead_anchor", Files: []string{filepath.Base(m.Path)}, Detail: "`" + a + "` does not exist",
				Action: "stale candidate; archived only if the LLM check also calls it stale"})
		}
	}
	for i := range live {
		for j := i + 1; j < len(live); j++ {
			a, b := live[i], live[j]
			sameName := a.Name != "" && strings.EqualFold(a.Name, b.Name)
			if sameName || jaccard(tokens(a.Description), tokens(b.Description)) >= dupJaccard {
				detail := "descriptions overlap"
				if sameName {
					detail = "same name"
				}
				add(finding{Kind: "duplicate", Files: []string{filepath.Base(a.Path), filepath.Base(b.Path)}, Detail: detail, Action: "review; merge by hand or let the LLM check propose one"})
			}
		}
	}
	if props, err := os.ReadDir(filepath.Join(dir, ".proposed")); err == nil {
		for _, e := range props {
			info, err := e.Info()
			if err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && env.Now().Sub(info.ModTime()) > proposalMaxAge {
				add(finding{Kind: "pending_proposal", Files: []string{".proposed/" + e.Name()}, Detail: "unreviewed for over 7 days", Action: "review: promote by hand or delete the proposal"})
			}
		}
	}

	var llm []dreamItem
	if len(live) >= minLLMMemories {
		newest := time.Time{}
		for _, m := range live {
			if m.ModTime.After(newest) {
				newest = m.ModTime
			}
		}
		switch {
		case !newest.After(checked[dir]):
		case dryRun:
			add(finding{Kind: "llm_pending", Detail: "changed since the last LLM check", Action: "a real run asks the model for contradictions and merges"})
		case *capped:
			add(finding{Kind: "llm_capped", Detail: "daily LLM call cap reached", Action: "checked on a later run"})
		default:
			items, err := dreamLLM(ctx, env, live)
			switch {
			case errors.Is(err, errCapped):
				*capped = true
				add(finding{Kind: "llm_capped", Detail: "daily LLM call cap reached", Action: "checked on a later run"})
			case err != nil:
				add(finding{Kind: "llm_error", Detail: firstLine(err.Error()), Action: "retried on a later run"})
			default:
				checked[dir] = newest
				llm = validDreamItems(items, byFile)
			}
		}
	}
	date := env.Now().Local().Format("2006-01-02")
	for _, it := range llm {
		f := finding{Kind: it.Kind, Files: it.Files, Detail: oneLine(it.Reason)}
		switch it.Kind {
		case "contradiction":
			f.Action = "review by hand"
			if s := oneLine(it.SuggestedText); s != "" {
				f.Action += "; suggested: " + s
			}
		case "stale":
			if !dead[it.Files[0]] {
				f.Action = "review by hand (no dead path backs this up)"
				break
			}
			f.Action = verb("archived to .archive/", "archive to .archive/ (also has a dead path)")
			if apply {
				if _, err := archiveMemory(byFile[it.Files[0]].Path); err != nil {
					f.Action = "archive failed: " + firstLine(err.Error())
				}
			}
		case "merge":
			if strings.TrimSpace(it.SuggestedText) == "" {
				f.Action = "review by hand (no merged text)"
				break
			}
			f.Action = verb("merged into a new file; sources superseded", "write a merged file and supersede the sources")
			if apply {
				if stem, err := writeMerge(env, dir, it, byFile, date); err != nil {
					f.Action = "merge failed: " + firstLine(err.Error())
				} else {
					f.Action = "merged into " + stem + ".md; sources superseded"
				}
			}
		}
		add(f)
	}

	// Index drift last, so it sees what apply mode just changed.
	missing, deadLinks := indexDrift(env, dir)
	if apply && len(deadLinks) > 0 {
		_ = removeDeadPointers(dir, deadLinks)
	}
	for _, l := range deadLinks {
		add(finding{Kind: "index_dead", Files: []string{l}, Detail: "MEMORY.md links to a missing file", Action: verb("removed the pointer line", "remove the pointer line")})
	}
	for _, m := range missing {
		stem := strings.TrimSuffix(filepath.Base(m.Path), ".md")
		if apply {
			_ = appendPointer(dir, stem, m.Description)
		}
		add(finding{Kind: "index_missing", Files: []string{filepath.Base(m.Path)}, Detail: "not in MEMORY.md", Action: verb("appended a pointer", "append a pointer")})
	}
	return r
}

// deadAnchors returns the home paths a memory names in backticks that no
// longer exist. Other paths are ignored: memories often describe remote
// hosts. Paths with placeholders are skipped.
func deadAnchors(env Env, m Memory) []string {
	if env.Home == "" {
		return nil
	}
	var out []string
	text := strings.ReplaceAll(m.Description+"\n"+m.Body, "```", "\n")
	for _, match := range anchorRe.FindAllStringSubmatch(text, -1) {
		fields := strings.Fields(match[1])
		if len(fields) == 0 {
			continue
		}
		raw := fields[0]
		var rest string
		switch {
		case strings.HasPrefix(raw, "~/"):
			rest = raw[2:]
		case strings.HasPrefix(raw, "$HOME/"):
			rest = raw[6:]
		case strings.HasPrefix(raw, "${HOME}/"):
			rest = raw[8:]
		default:
			continue
		}
		rest = lineNoRe.ReplaceAllString(strings.TrimRight(rest, ".,;)"), "")
		if rest == "" || strings.ContainsAny(rest, "<>{}$") {
			continue
		}
		p := filepath.Join(env.Home, rest)
		if strings.ContainsAny(rest, "*?[") {
			if m, err := filepath.Glob(p); err != nil || len(m) > 0 {
				continue
			}
		} else if _, err := os.Lstat(p); err == nil {
			continue
		}
		if !contains(out, raw) {
			out = append(out, raw)
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// localLink returns the file a MEMORY.md link points at, relative to the
// memory dir, or "" for URLs, anchors and absolute or home paths.
func localLink(target string) string {
	if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") ||
		strings.HasPrefix(target, "/") || strings.HasPrefix(target, "~") || strings.HasPrefix(target, "$") {
		return "" // only links relative to the memory dir are checked
	}
	target, _, _ = strings.Cut(target, "#")
	if target == "" {
		return ""
	}
	return filepath.Clean(target)
}

// indexDrift compares MEMORY.md with the files beside it: live memory files
// it does not link, and links to files that do not exist.
func indexDrift(env Env, dir string) (missing []Memory, dead []string) {
	b, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	linked := map[string]bool{}
	for _, m := range linkRe.FindAllStringSubmatch(string(b), -1) {
		t := localLink(m[1])
		if t == "" {
			continue
		}
		linked[t] = true
		if _, err := os.Lstat(filepath.Join(dir, t)); errors.Is(err, os.ErrNotExist) && !contains(dead, t) {
			dead = append(dead, t)
		}
	}
	for _, m := range loadMemories(env, dir, nil) {
		if !linked[filepath.Base(m.Path)] {
			missing = append(missing, m)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Path < missing[j].Path })
	return missing, dead
}

// removeDeadPointers drops MEMORY.md lines whose every local link is dead.
// MEMORY.md is an index, not a memory, so lines may go; memories never do.
func removeDeadPointers(dir string, dead []string) error {
	path := filepath.Join(dir, "MEMORY.md")
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var keep []string
	for _, line := range strings.SplitAfter(string(b), "\n") {
		links, allDead := 0, true
		for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
			if t := localLink(m[1]); t != "" {
				links++
				allDead = allDead && contains(dead, t)
			}
		}
		if links > 0 && allDead {
			continue
		}
		keep = append(keep, line)
	}
	return writeMemory(path, []byte(strings.Join(keep, "")), st.ModTime())
}

// dreamItem is one problem the model reports.
type dreamItem struct {
	Kind          string   `json:"kind"`
	Files         []string `json:"files"`
	Reason        string   `json:"reason"`
	SuggestedText string   `json:"suggested_text"`
	Description   string   `json:"description"`
}

func dreamLLM(ctx context.Context, env Env, live []Memory) ([]dreamItem, error) {
	per := min(dreamBodyChars, dreamInputChars/max(len(live), 1))
	var b strings.Builder
	for _, m := range live {
		body := strings.TrimSpace(m.Body)
		if len(body) > per {
			body = strings.ToValidUTF8(body[:per], "") + "…"
		}
		fmt.Fprintf(&b, "### %s\nname: %s\ntype: %s\ndescription: %s\n\n%s\n\n", filepath.Base(m.Path), m.Name, m.Type, oneLine(m.Description), body)
	}
	answer, err := callLLM(ctx, env, dreamSystemPrompt, b.String())
	if err != nil {
		return nil, err
	}
	var items []dreamItem
	if err := decodeArray(answer, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// validDreamItems keeps items whose kind is known and whose files all exist
// in this dir, looked up by name, never joined into a path.
func validDreamItems(items []dreamItem, byFile map[string]Memory) []dreamItem {
	var out []dreamItem
	for _, it := range items {
		ok := len(it.Files) > 0
		seen := map[string]bool{}
		for _, f := range it.Files {
			if _, found := byFile[f]; !found || seen[f] {
				ok = false
			}
			seen[f] = true
		}
		switch it.Kind {
		case "contradiction", "merge":
			ok = ok && len(it.Files) >= 2
		case "stale":
			ok = ok && len(it.Files) == 1
		default:
			ok = false
		}
		if ok {
			out = append(out, it)
		}
	}
	return out
}

// writeMerge writes the merged text as a new file named after the first
// source, then supersedes every source. No body is changed.
func writeMerge(env Env, dir string, it dreamItem, byFile map[string]Memory, date string) (string, error) {
	first := byFile[it.Files[0]]
	stem, err := newStem(candidate{Dir: dir, TargetPath: first.Path})
	if err != nil {
		return "", err
	}
	desc := it.Description
	if strings.TrimSpace(desc) == "" {
		desc = first.Description
	}
	var b strings.Builder
	b.WriteString("---\nname: " + stem + "\ndescription: " + yamlValue(desc) + "\n")
	b.WriteString("metadata:\n  type: " + yamlValue(first.Type) + "\n  modified: " + date + "\n---\n\n")
	b.WriteString(strings.TrimSpace(it.SuggestedText) + "\n\nMerged from: ")
	for i, f := range it.Files {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("`" + f + "`")
	}
	b.WriteString(" (claude-dream, " + date + ")\n")
	if err := writeMemory(filepath.Join(dir, stem+".md"), []byte(b.String()), time.Time{}); err != nil {
		return "", err
	}
	for _, f := range it.Files {
		if err := supersede(byFile[f].Path, stem, env.Now()); err != nil {
			return stem, err
		}
	}
	_ = appendPointer(dir, stem, desc)
	return stem, nil
}

func tildePath(env Env, p string) string {
	if env.Home != "" {
		if rest, ok := strings.CutPrefix(p, env.Home+"/"); ok {
			return "~/" + rest
		}
	}
	return p
}

func renderDream(env Env, reports []dirReport, counts map[string]int, label string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# claude-dream review, %s (%s)\n\n", env.Now().Local().Format("2006-01-02"), label)
	if len(reports) == 0 {
		b.WriteString("Nothing to review.\n")
	} else {
		b.WriteString("Counts: " + formatCounts(counts) + "\n")
	}
	for _, r := range reports {
		b.WriteString("\n## " + tildePath(env, r.Dir) + "\n\n")
		for _, f := range r.Findings {
			files := ""
			if len(f.Files) > 0 {
				files = " `" + strings.Join(f.Files, "`, `") + "`"
			}
			fmt.Fprintf(&b, "- **%s**%s: %s. Action: %s.\n", f.Kind, files, f.Detail, f.Action)
		}
	}
	return b.String()
}

func formatCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}
