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
	chunkChars    = 24000 // human-turn text per LLM call
	maxItems      = 5     // items kept per chunk
	minQuoteChars = 12
	maxNameChars  = 60
)

var (
	kebabRe   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	versionRe = regexp.MustCompile(`-v[0-9]+$`)
)

// mode reads CLAUDE_DREAM_EXTRACT or CLAUDE_DREAM_DREAM: propose (default),
// apply or off. propose never touches a live memory file.
func mode(env Env, key string) string {
	switch v := strings.ToLower(strings.TrimSpace(env.Getenv(key))); v {
	case "apply", "off":
		return v
	}
	return "propose"
}

// item is one fact the model proposes.
type item struct {
	Action        string `json:"action"`
	Scope         string `json:"scope"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Rule          string `json:"rule"`
	Why           string `json:"why"`
	HowToApply    string `json:"how_to_apply"`
	EvidenceQuote string `json:"evidence_quote"`
	Target        string `json:"target"`
}

// candidate is an item that passed validation, with where it would go.
type candidate struct {
	item
	Dir        string `json:"target_dir"`  // memory dir it belongs in
	TargetPath string `json:"target_path"` // the memory it replaces, if any
	QuoteTurn  int    `json:"quote_turn"`  // index of the human turn quoted
}

// memIndex is the existing memories an extraction can see and replace.
type memIndex struct {
	projDir, globDir string
	projLabel        string
	mems             map[string]map[string]Memory // dir -> file stem -> memory
	pending          map[string]map[string]Memory // dir -> stem -> proposal awaiting review
	accepted         []string                     // descriptions accepted this run
	taken            map[string]bool              // dir/stem names used this run
}

func newMemIndex(env Env, projDir, globDir, projLabel string) *memIndex {
	ix := &memIndex{projDir: projDir, globDir: globDir, projLabel: projLabel,
		mems: map[string]map[string]Memory{}, pending: map[string]map[string]Memory{}, taken: map[string]bool{}}
	for _, d := range []string{projDir, globDir} {
		if d == "" || ix.mems[d] != nil {
			continue
		}
		ix.mems[d] = map[string]Memory{}
		for _, m := range loadMemories(env, d, nil) {
			ix.mems[d][strings.TrimSuffix(filepath.Base(m.Path), ".md")] = m
		}
		ix.pending[d] = map[string]Memory{}
		for _, m := range loadMemories(env, filepath.Join(d, ".proposed"), nil) {
			stem := strings.TrimSuffix(filepath.Base(m.Path), ".md")
			if len(stem) > len("2006-01-02-") {
				stem = stem[len("2006-01-02-"):]
			}
			ix.pending[d][stem] = m
		}
	}
	return ix
}

// duplicate reports whether desc overlaps (token Jaccard >= 0.6) a live
// memory or pending proposal in any dir of the index, or a fact accepted
// earlier in this run. The memory being replaced does not count.
func (ix *memIndex) duplicate(desc, except string) bool {
	words := tokens(desc)
	for _, set := range []map[string]map[string]Memory{ix.mems, ix.pending} {
		for _, mems := range set {
			for _, m := range mems {
				if m.Path != except && jaccard(words, tokens(m.Description)) >= dupJaccard {
					return true
				}
			}
		}
	}
	for _, d := range ix.accepted {
		if jaccard(words, tokens(d)) >= dupJaccard {
			return true
		}
	}
	return false
}

func (ix *memIndex) dirFor(scope string) string {
	if scope == "project" {
		return ix.projDir
	}
	return ix.globDir
}

// exists reports whether stem is used in dir: a live file (superseded or
// not), a proposal, or an earlier item in this run.
func (ix *memIndex) exists(dir, stem string) bool {
	if ix.taken[dir+"/"+stem] {
		return true
	}
	if _, err := os.Lstat(filepath.Join(dir, stem+".md")); err == nil {
		return true
	}
	return proposalExists(dir, stem)
}

// proposalExists reports whether .proposed/ holds <date>-<stem>.md for any date.
func proposalExists(dir, stem string) bool {
	m, _ := filepath.Glob(filepath.Join(dir, ".proposed", "*-"+stem+".md"))
	for _, p := range m {
		if b := filepath.Base(p); len(b) > len("2006-01-02-") && b[len("2006-01-02-"):] == stem+".md" {
			return true
		}
	}
	return false
}

func (ix *memIndex) prompt(turns []turn) string {
	var b strings.Builder
	b.WriteString("EXISTING MEMORIES\n")
	write := func(title, dir string) {
		b.WriteString(title + ":\n")
		if len(ix.mems[dir])+len(ix.pending[dir]) == 0 {
			b.WriteString("(none)\n")
		}
		stems := make([]string, 0, len(ix.mems[dir]))
		for stem := range ix.mems[dir] {
			stems = append(stems, stem)
		}
		sort.Strings(stems)
		for _, stem := range stems {
			m := ix.mems[dir][stem]
			fmt.Fprintf(&b, "- %s | %s | %s\n", stem, m.Type, oneLine(m.Description))
		}
		stems = stems[:0]
		for stem := range ix.pending[dir] {
			stems = append(stems, stem)
		}
		sort.Strings(stems)
		for _, stem := range stems {
			m := ix.pending[dir][stem]
			fmt.Fprintf(&b, "- %s | %s | %s | pending review\n", stem, m.Type, oneLine(m.Description))
		}
	}
	if ix.projDir != "" && ix.projDir != ix.globDir {
		write("scope project ("+ix.projLabel+")", ix.projDir)
	}
	if ix.globDir != "" {
		title := "scope global"
		if ix.projDir == ix.globDir {
			title = "scope project and global (" + ix.projLabel + ")"
		}
		write(title, ix.globDir)
	}
	b.WriteString("\nTURNS\n")
	for i, t := range turns {
		fmt.Fprintf(&b, "\n=== turn %d ===\n", i+1)
		if t.Context != "" {
			b.WriteString("CONTEXT (assistant, not evidence):\n" + t.Context + "\n")
		}
		b.WriteString("USER:\n" + t.Text + "\n")
	}
	return b.String()
}

// chunkTurns groups turns into prompts of at most chunkChars. A turn longer
// than that alone is cut; evidence is then checked against what was sent.
func chunkTurns(turns []turn) [][]turn {
	var out [][]turn
	var cur []turn
	size := 0
	for _, t := range turns {
		if len(t.Text) > chunkChars {
			t.Text = strings.ToValidUTF8(t.Text[:chunkChars], "")
		}
		n := len(t.Text) + len(t.Context)
		if len(cur) > 0 && size+n > chunkChars {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, t)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

var quoteFolds = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`)

// normQuote lowercases, folds typographic quotes and collapses whitespace.
func normQuote(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(quoteFolds.Replace(s))), " ")
}

func validType(t string) bool {
	_, ok := typePriority[t]
	return ok
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// validate checks one item. It returns a candidate, or the drop reason.
func validate(it item, turns []turn, ix *memIndex) (candidate, string) {
	c := candidate{item: it, QuoteTurn: -1}
	switch {
	case it.Action == "noop":
		return c, "noop"
	case it.Action != "add" && it.Action != "update" && it.Action != "supersede":
		return c, "bad_action"
	case it.Scope != "project" && it.Scope != "global":
		return c, "bad_scope"
	case !validType(it.Type):
		return c, "bad_type"
	case len(it.Name) > maxNameChars || !kebabRe.MatchString(it.Name):
		return c, "bad_name"
	case strings.TrimSpace(it.Rule) == "" || strings.TrimSpace(it.Description) == "":
		return c, "empty_rule"
	}
	q := normQuote(it.EvidenceQuote)
	if len(q) < minQuoteChars {
		return c, "quote_too_short"
	}
	for i, t := range turns {
		if strings.Contains(normQuote(t.Text), q) {
			c.QuoteTurn = i
			break
		}
	}
	if c.QuoteTurn < 0 {
		return c, "quote_not_in_human_turn"
	}
	c.Dir = ix.dirFor(it.Scope)
	if c.Dir == "" {
		return c, "no_memory_dir"
	}
	if it.Action == "add" {
		if ix.exists(c.Dir, it.Name) {
			return c, "name_collision"
		}
		if ix.duplicate(it.Description, "") {
			return c, "duplicate"
		}
		ix.taken[c.Dir+"/"+it.Name] = true
		ix.accepted = append(ix.accepted, it.Description)
		return c, ""
	}
	// update or supersede: the target is looked up, never joined as a path.
	for _, d := range []string{c.Dir, ix.projDir, ix.globDir} {
		if m, ok := ix.mems[d][it.Target]; ok {
			if ix.duplicate(it.Description, m.Path) {
				return c, "duplicate"
			}
			c.Dir, c.TargetPath = d, m.Path
			ix.accepted = append(ix.accepted, it.Description)
			return c, ""
		}
	}
	return c, "unknown_target"
}

// runChunk asks the model about one chunk and validates its answer.
func runChunk(ctx context.Context, env Env, chunk []turn, ix *memIndex, dropped map[string]int) ([]candidate, error) {
	answer, err := callLLM(ctx, env, extractSystemPrompt, ix.prompt(chunk))
	if err != nil {
		return nil, err
	}
	var items []item
	if err := decodeArray(answer, &items); err != nil {
		dropped["bad_json"]++
		return nil, nil
	}
	var out []candidate
	for i, it := range items {
		if i >= maxItems {
			dropped["over_limit"] += len(items) - maxItems
			break
		}
		c, reason := validate(it, chunk, ix)
		if reason != "" {
			dropped[reason]++
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// targetDirs picks where facts go: project facts to the single focus repo's
// memory dir, else the cwd's; global facts to the cwd's. Only memory dirs
// that already exist are used: none is ever created.
func targetDirs(env Env, sessionID, cwd string) (proj, glob, label string) {
	if d := memoryDir(env, cwd); d != "" && isDir(d) {
		glob, proj, label = d, d, filepath.Base(cwd)
	}
	if focus := loadState(env, sessionID).Focus; len(focus) == 1 {
		if d := memoryDir(env, focus[0]); d != "" && isDir(d) {
			proj, label = d, filepath.Base(focus[0])
		}
	}
	return proj, glob, label
}

type extractState struct {
	Offset int64 `json:"offset"`
}

func extractStateFile(env Env, id string) string {
	return filepath.Join(env.StateDir, "extract", unsafeIDRe.ReplaceAllString(id, "_")+".json")
}

// extractLog is one line of extract.jsonl.
type extractLog struct {
	TS        string         `json:"ts"`
	SessionID string         `json:"session_id"`
	Mode      string         `json:"mode"`
	Turns     int            `json:"turns"`
	Chunks    int            `json:"chunks"`
	Proposed  []string       `json:"proposed,omitempty"`
	Written   []string       `json:"written,omitempty"`
	Dropped   map[string]int `json:"dropped,omitempty"`
	Skipped   string         `json:"skipped,omitempty"`
	Error     string         `json:"error,omitempty"`
}

func within(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// extractJob is the built-in extractor: new human turns since the last run
// of this session, chunked, sent to the model, validated, then proposed or
// written. It returns errCapped when the daily cap stops it; the offset then
// covers only the chunks already done.
func extractJob(ctx context.Context, env Env, job Job) error {
	md := mode(env, "CLAUDE_DREAM_EXTRACT")
	if md == "off" || job.SessionID == "" || job.TranscriptPath == "" {
		return nil
	}
	lg := extractLog{TS: env.Now().UTC().Format(time.RFC3339), SessionID: job.SessionID, Mode: md, Dropped: map[string]int{}}
	defer func() { appendJSONL(filepath.Join(env.StateDir, "extract.jsonl"), lg) }()
	if within(job.Cwd, env.StateDir) {
		lg.Skipped = "worker_session"
		return nil
	}
	var st extractState
	if b, err := os.ReadFile(extractStateFile(env, job.SessionID)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	sc, err := scanTranscript(job.TranscriptPath, false)
	if err != nil {
		lg.Error = err.Error()
		return err
	}
	if sc.Headless {
		lg.Skipped = "headless" // claude -p runs: their prompts are machine-written
		return nil
	}
	if sc.Size < st.Offset {
		st.Offset = 0 // a different or rewritten file
	}
	var turns []turn
	for _, t := range sc.Turns {
		if t.End > st.Offset {
			turns = append(turns, t)
		}
	}
	lg.Turns = len(turns)
	if len(turns) < 2 {
		lg.Skipped = "few_turns" // the offset stays, so these turns are not lost
		return nil
	}
	cwd := job.Cwd
	if cwd == "" {
		cwd = sc.Cwd
	}
	proj, glob, label := targetDirs(env, job.SessionID, cwd)
	if proj == "" && glob == "" {
		lg.Skipped = "no_memory_dir"
		return nil
	}
	ix := newMemIndex(env, proj, glob, label)
	chunks := chunkTurns(turns)
	date := env.Now().Local().Format("2006-01-02")
	for i, chunk := range chunks {
		cands, err := runChunk(ctx, env, chunk, ix, lg.Dropped)
		if err != nil {
			lg.Error = err.Error()
			return err
		}
		lg.Chunks++
		for _, c := range cands {
			path, reason := place(env, md, c, job.SessionID, date)
			if reason != "" {
				lg.Dropped[reason]++
			}
			switch {
			case path == "":
			case md == "apply":
				lg.Written = append(lg.Written, path)
			default:
				lg.Proposed = append(lg.Proposed, path)
			}
		}
		st.Offset = chunk[len(chunk)-1].End
		if i == len(chunks)-1 {
			st.Offset = sc.Size
		}
		if b, err := json.Marshal(st); err == nil {
			_ = writeAtomic(extractStateFile(env, job.SessionID), b)
		}
	}
	return nil
}

// yamlValue keeps a frontmatter value on one line and quotes it when plain
// YAML would misread it.
func yamlValue(s string) string {
	s = oneLine(s)
	if s == "" || strings.ContainsAny(s[:1], `!&*[]{}|>'"%@#,?:-`+"`") ||
		strings.Contains(s, ": ") || strings.Contains(s, " #") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

// memoryText renders a memory file in auto memory's frontmatter format.
// A proposal carries proposed_action and proposed_target, to remove when it
// is promoted by hand.
func memoryText(c candidate, stem, sessionID, date string, proposal bool) []byte {
	var b strings.Builder
	b.WriteString("---\nname: " + stem + "\ndescription: " + yamlValue(c.Description) + "\n")
	b.WriteString("metadata:\n  type: " + c.Type + "\n  originSessionId: " + yamlValue(sessionID) + "\n  modified: " + date + "\n")
	if proposal {
		b.WriteString("proposed_action: " + c.Action + "\n")
		if c.TargetPath != "" {
			b.WriteString("proposed_target: " + filepath.Base(c.TargetPath) + "\n")
		}
	}
	b.WriteString("---\n\n" + strings.TrimSpace(c.Rule) + "\n")
	if w := strings.TrimSpace(c.Why); w != "" {
		b.WriteString("\n**Why:** " + w + "\n")
	}
	if h := strings.TrimSpace(c.HowToApply); h != "" {
		b.WriteString("\n**How to apply:** " + h + "\n")
	}
	if c.TargetPath != "" {
		b.WriteString("\nReplaces: `" + filepath.Base(c.TargetPath) + "`\n")
	}
	fmt.Fprintf(&b, "\nEvidence: %q (session %s, %s)\n", oneLine(c.EvidenceQuote), sessionID, date)
	return []byte(b.String())
}

// newStem names the file for c: its own name for add, or the target's name
// with the next free -vN suffix for update and supersede. It gives up on an
// unreadable dir or after maxNameTries names.
func newStem(c candidate) (string, error) {
	if c.TargetPath == "" {
		return c.Name, nil
	}
	base := versionRe.ReplaceAllString(strings.TrimSuffix(filepath.Base(c.TargetPath), ".md"), "")
	for n := 2; n < 2+maxNameTries; n++ {
		stem := fmt.Sprintf("%s-v%d", base, n)
		_, err := os.Lstat(filepath.Join(c.Dir, stem+".md"))
		switch {
		case errors.Is(err, os.ErrNotExist):
			if !proposalExists(c.Dir, stem) {
				return stem, nil
			}
		case err != nil:
			return "", err
		}
	}
	return "", errors.New("no free -vN name")
}

// place proposes or writes one candidate. It returns the file written, or
// a drop reason. Writes refuse to replace any existing file.
func place(env Env, md string, c candidate, sessionID, date string) (string, string) {
	stem, err := newStem(c)
	if err != nil {
		return "", "no_free_name"
	}
	if md != "apply" {
		path := filepath.Join(c.Dir, ".proposed", date+"-"+stem+".md")
		if err := writeMemory(path, memoryText(c, stem, sessionID, date, true), time.Time{}); err != nil {
			return "", writeReason(err)
		}
		return path, ""
	}
	path := filepath.Join(c.Dir, stem+".md")
	if err := writeMemory(path, memoryText(c, stem, sessionID, date, false), time.Time{}); err != nil {
		return "", writeReason(err)
	}
	if c.TargetPath != "" {
		if err := supersede(c.TargetPath, stem, env.Now()); err != nil {
			return path, "supersede_failed" // the new file stands; the old one is still live
		}
	}
	_ = appendPointer(c.Dir, stem, c.Description)
	return path, ""
}

func writeReason(err error) string {
	if errors.Is(err, ErrConflict) {
		return "exists"
	}
	return "write_failed"
}

// pointerTitle turns a file stem into a MEMORY.md link title.
func pointerTitle(stem string) string {
	t := strings.ReplaceAll(strings.ReplaceAll(stem, "-", " "), "_", " ")
	if t == "" {
		return t
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

// appendPointer adds "- [Title](stem.md) — hook" to the dir's MEMORY.md,
// through the conflict-checked writer. It does nothing if the file is
// already linked.
func appendPointer(dir, stem, hook string) error {
	path := filepath.Join(dir, "MEMORY.md")
	var readAt time.Time
	var text string
	if st, err := os.Stat(path); err == nil {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		readAt, text = st.ModTime(), string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(text, "]("+stem+".md)") {
		return nil
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += "- [" + pointerTitle(stem) + "](" + stem + ".md) — " + oneLine(hook) + "\n"
	return writeMemory(path, []byte(text), readAt)
}

// appendJSONL appends v as one line; errors are ignored (logs only).
func appendJSONL(path string, v any) {
	b, err := json.Marshal(v)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}
