package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultInjectChars  = 2000 // per event; small payloads work best
	maxInjectChars      = 9000 // hard cap, under Claude Code's 10,000
	defaultSessionChars = 8000 // per session, across events
	minScore            = 2    // shared prompt words needed on the keyword path
	perMemoryChars      = 1200
	sessionKeepDays     = 7
)

var (
	wordRe       = regexp.MustCompile(`[a-z0-9]{4,}`)
	nonProjectRe = regexp.MustCompile(`[^A-Za-z0-9-]`)
	stopwords    = map[string]bool{
		"this": true, "that": true, "with": true, "from": true, "have": true, "what": true,
		"when": true, "where": true, "which": true, "about": true, "into": true, "your": true,
		"does": true, "should": true, "would": true, "could": true, "there": true, "their": true,
		"them": true, "then": true, "than": true, "will": true, "just": true, "like": true,
		"make": true, "need": true, "want": true, "also": true, "some": true, "more": true,
		"here": true, "please": true, "using": true, "work": true, "code": true, "file": true,
		"files": true, "notes": true, "memory": true,
	}
	typePriority = map[string]int{"feedback": 0, "project": 1, "reference": 2, "user": 3}
)

// projectDirName is Claude Code's name for a project dir: the path with
// every character outside [A-Za-z0-9-] replaced by "-".
func projectDirName(cwd string) string { return nonProjectRe.ReplaceAllString(cwd, "-") }

// memoryDir is derived from the full path only, never matched by name.
func memoryDir(env Env, path string) string {
	if path == "" || env.ProjectsDir == "" {
		return ""
	}
	return filepath.Join(env.ProjectsDir, projectDirName(path), "memory")
}

func envInt(env Env, key string, def, limit int) int {
	n, err := strconv.Atoi(strings.TrimSpace(env.Getenv(key)))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, limit)
}

func injectBudget(env Env) int {
	return envInt(env, "CLAUDE_DREAM_INJECT_CHARS", defaultInjectChars, maxInjectChars)
}

func sessionBudget(env Env) int {
	return envInt(env, "CLAUDE_DREAM_SESSION_CHARS", defaultSessionChars, 1<<20)
}

func formatMemory(m Memory) string {
	var b strings.Builder
	b.WriteString("\n## " + m.Name)
	if m.Type != "" {
		b.WriteString(" (" + m.Type + ")")
	}
	b.WriteString("\n")
	if m.Description != "" {
		b.WriteString(m.Description + "\n")
	}
	body := strings.TrimSpace(m.Body)
	if len(body) > perMemoryChars {
		body = body[:perMemoryChars] + "…"
	}
	if body != "" {
		b.WriteString(body + "\n")
	}
	return b.String()
}

// abArm picks "on" or "off" for a session: a deterministic 50/50 hash.
func abArm(sessionID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	if h.Sum32()%2 == 0 {
		return "on"
	}
	return "off"
}

// tokens returns the distinctive words (4+ chars, no stopwords) of text.
func tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		if !stopwords[w] {
			out[w] = true
		}
	}
	return out
}

// overlap counts the words a memory's name and description share with the prompt.
func overlap(m Memory, prompt map[string]bool) int {
	n := 0
	for w := range tokens(m.Name + " " + m.Description) {
		if prompt[w] {
			n++
		}
	}
	return n
}

// memGroup is the memories to inject from one memory dir.
type memGroup struct {
	repo string // label for the header
	dir  string
	mems []Memory
}

// loadMemories reads a memory dir. It skips MEMORY.md, hidden entries
// (.archive, .proposed), superseded files and anything in skip.
func loadMemories(env Env, dir string, skip map[string]bool) []Memory {
	entries, err := env.readDir(dir)
	if err != nil {
		return nil
	}
	var out []Memory
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".md") || n == "MEMORY.md" {
			continue
		}
		p := filepath.Join(dir, n)
		if skip[p] {
			continue
		}
		if m, err := readMemory(p); err == nil && m.SupersededBy == "" {
			out = append(out, m)
		}
	}
	return out
}

// rank orders memories by prompt overlap when there is a prompt, then by
// type (feedback first), then newest first.
func rank(mems []Memory, prompt map[string]bool) {
	sort.SliceStable(mems, func(i, j int) bool {
		if len(prompt) > 0 {
			if a, b := overlap(mems[i], prompt), overlap(mems[j], prompt); a != b {
				return a > b
			}
		}
		pi, ok := typePriority[mems[i].Type]
		if !ok {
			pi = len(typePriority)
		}
		pj, ok := typePriority[mems[j].Type]
		if !ok {
			pj = len(typePriority)
		}
		if pi != pj {
			return pi < pj
		}
		if !mems[i].ModTime.Equal(mems[j].ModTime) {
			return mems[i].ModTime.After(mems[j].ModTime)
		}
		return mems[i].Name < mems[j].Name
	})
}

func groupHeader(g memGroup) string {
	return "claude-dream: memories for " + g.repo + " (from ~/.claude/projects/" +
		filepath.Base(filepath.Dir(g.dir)) + "/memory):\n"
}

// inject handles SessionStart, UserPromptSubmit and PostToolUse. It tracks
// the repos a session focuses on and injects each newly focused repo's
// memories once. When nothing qualifies it prints and logs nothing.
func inject(p Payload, stdout io.Writer, env Env) {
	if p.SessionID == "" {
		return
	}
	roots := rootsOf(env)
	st, dirty := loadState(env, p.SessionID), false
	var fresh []string // repos newly in focus on this event
	add := func(repo string) {
		if repo != "" && !contains(st.Focus, repo) {
			st.Focus = append(st.Focus, repo)
			fresh = append(fresh, repo)
			dirty = true
		}
	}
	var prompt map[string]bool
	switch p.HookEventName {
	case "SessionStart":
		pruneSessions(env)
		add(repoOf(roots, p.Cwd))
		if p.Source == "compact" {
			// Earlier injections left the context: start over.
			st.Injected, st.Chars, dirty = nil, 0, true
			fresh = append([]string(nil), st.Focus...)
		}
	case "UserPromptSubmit":
		prompt = tokens(p.Prompt)
		add(repoNamedIn(env, roots, p.Prompt, st.Focus))
	case "PostToolUse":
		for _, s := range toolStrings(p.ToolInput) {
			add(repoInString(env, roots, s))
		}
	}
	if len(fresh) == 0 && len(prompt) == 0 {
		if dirty {
			saveState(env, p.SessionID, st)
		}
		return
	}

	skip := map[string]bool{}
	for _, f := range st.Injected {
		skip[f] = true
	}
	var groups []memGroup
	seen := map[string]bool{}
	for _, repo := range fresh {
		dir := memoryDir(env, repo)
		seen[dir] = true
		g := memGroup{repo: filepath.Base(repo), dir: dir, mems: loadMemories(env, dir, skip)}
		rank(g.mems, prompt)
		groups = append(groups, g)
	}
	if len(prompt) > 0 { // the keyword path: relevant to what is being discussed
		dirs := []struct{ label, path string }{{filepath.Base(p.Cwd), p.Cwd}}
		for _, f := range st.Focus {
			dirs = append(dirs, struct{ label, path string }{filepath.Base(f), f})
		}
		for _, d := range dirs {
			dir := memoryDir(env, d.path)
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			g := memGroup{repo: d.label, dir: dir}
			for _, m := range loadMemories(env, dir, skip) {
				if overlap(m, prompt) >= minScore {
					g.mems = append(g.mems, m)
				}
			}
			rank(g.mems, prompt)
			groups = append(groups, g)
		}
	}

	budget := min(injectBudget(env), sessionBudget(env)-st.Chars)
	var ctx strings.Builder
	var names, paths, repos []string
	for _, g := range groups {
		hdr, used := groupHeader(g), false
		for _, m := range g.mems {
			text := formatMemory(m)
			cost := len(text)
			if !used {
				cost += len(hdr)
			}
			if ctx.Len()+cost > budget {
				continue
			}
			if !used {
				ctx.WriteString(hdr)
				used = true
				repos = append(repos, g.repo)
			}
			ctx.WriteString(text)
			names = append(names, m.Name)
			paths = append(paths, m.Path)
		}
	}
	if len(names) == 0 {
		if dirty {
			saveState(env, p.SessionID, st)
		}
		return
	}
	// Both arms count against the cap as if injected, so they stay comparable.
	st.Injected = append(st.Injected, paths...)
	st.Chars += ctx.Len()
	saveState(env, p.SessionID, st)

	arm := "on"
	if env.Getenv("CLAUDE_DREAM_AB") == "1" {
		arm = abArm(p.SessionID)
	}
	if arm == "off" {
		logInjection(env, p, arm, repos, nil, names, 0)
		return
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": p.HookEventName, "additionalContext": ctx.String(),
	}}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(stdout, string(b))
	logInjection(env, p, arm, repos, names, nil, ctx.Len())
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// logInjection appends one JSONL line per event that injected or withheld.
func logInjection(env Env, p Payload, arm string, repos, names, withheld []string, chars int) {
	b, err := json.Marshal(map[string]any{
		"ts": env.Now().UTC().Format("2006-01-02T15:04:05Z"), "session_id": p.SessionID,
		"event": p.HookEventName, "arm": arm, "repos": repos,
		"memories": names, "withheld": withheld, "chars": chars,
	})
	if err != nil || os.MkdirAll(env.StateDir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(env.StateDir, "injections.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// sessionState is one session's focus, kept so later events inject each
// memory once. Losing it is harmless: the session just re-injects.
type sessionState struct {
	Focus    []string `json:"focus"`
	Injected []string `json:"injected"`
	Chars    int      `json:"chars"`
}

var unsafeIDRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func stateFile(env Env, id string) string {
	return filepath.Join(env.StateDir, "sessions", unsafeIDRe.ReplaceAllString(id, "_")+".json")
}

func loadState(env Env, id string) sessionState {
	var st sessionState
	if b, err := os.ReadFile(stateFile(env, id)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveState(env Env, id string, st sessionState) {
	if b, err := json.Marshal(st); err == nil {
		_ = writeAtomic(stateFile(env, id), b)
	}
}

// pruneSessions removes session files untouched for a week.
func pruneSessions(env Env) {
	dir := filepath.Join(env.StateDir, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := env.Now().Add(-sessionKeepDays * 24 * time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
