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
)

const (
	defaultInjectChars = 2000 // small payloads work best
	maxInjectChars     = 9000 // hard cap, under Claude Code's 10,000
	minScore           = 2    // below this a memory is never injected
	perMemoryChars     = 1200
	injectHeader       = "Relevant notes from earlier sessions in this project (auto memory):\n"
)

var (
	wordRe        = regexp.MustCompile(`[a-z0-9]{3,}`)
	nonProjectRe  = regexp.MustCompile(`[^A-Za-z0-9-]`)
	genericTokens = map[string]bool{
		"users": true, "home": true, "code": true, "src": true, "projects": true,
		"documents": true, "work": true, "the": true, "and": true, "for": true,
	}
)

// projectDirName is Claude Code's name for a project dir: the cwd path with
// every character outside [A-Za-z0-9-] replaced by "-".
func projectDirName(cwd string) string { return nonProjectRe.ReplaceAllString(cwd, "-") }

// memoryDir is derived from the full cwd path only, never matched by name.
func memoryDir(env Env, cwd string) string {
	if cwd == "" || env.ProjectsDir == "" {
		return ""
	}
	return filepath.Join(env.ProjectsDir, projectDirName(cwd), "memory")
}

func injectBudget(env Env) int {
	n, err := strconv.Atoi(strings.TrimSpace(env.Getenv("CLAUDE_DREAM_INJECT_CHARS")))
	if err != nil || n <= 0 {
		return defaultInjectChars
	}
	return min(n, maxInjectChars)
}

// scored is a memory with its relevance to the working directory.
type scored struct {
	Memory
	score int
}

// pathTokens returns the basename token and the other distinctive tokens of
// the cwd path.
func pathTokens(cwd string) (base string, others map[string]bool) {
	cwd = filepath.Clean(cwd)
	base = strings.ToLower(filepath.Base(cwd))
	others = map[string]bool{}
	parts := strings.Split(strings.ToLower(cwd), string(filepath.Separator))
	if len(parts) > 2 && (parts[1] == "users" || parts[1] == "home") {
		parts = parts[3:] // drop the user's own name too
	}
	for _, part := range parts {
		for _, w := range wordRe.FindAllString(part, -1) {
			if !genericTokens[w] {
				others[w] = true
			}
		}
	}
	return base, others
}

// score is deterministic: 3 when the repo basename appears in the name or
// description, plus 1 for each other path token found there.
func score(m Memory, base string, others map[string]bool) int {
	text := strings.ToLower(m.Name + " " + m.Description)
	words := map[string]bool{}
	for _, w := range wordRe.FindAllString(text, -1) {
		words[w] = true
	}
	s := 0
	if base != "" && !genericTokens[base] && (words[base] || strings.Contains(text, base)) {
		s += 3
	}
	for w := range others {
		if w != base && words[w] {
			s++
		}
	}
	return s
}

// pickMemories returns the memories to inject, best first, within budget.
func pickMemories(dir, cwd string, budget int) []scored {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	base, others := pathTokens(cwd)
	var cands []scored
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
			continue
		}
		m, err := readMemory(filepath.Join(dir, e.Name()))
		if err != nil || m.SupersededBy != "" {
			continue
		}
		if s := score(m, base, others); s >= minScore {
			cands = append(cands, scored{m, s})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].Name < cands[j].Name
	})
	used := len(injectHeader)
	var out []scored
	for _, c := range cands {
		n := len(formatMemory(c.Memory))
		if used+n > budget {
			continue
		}
		used += n
		out = append(out, c)
	}
	return out
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

// inject answers SessionStart. When no memory is relevant it prints nothing.
// With CLAUDE_DREAM_AB=1 half the sessions get nothing, for evaluation.
func inject(p Payload, stdout io.Writer, env Env) {
	dir := memoryDir(env, p.Cwd)
	if dir == "" {
		return
	}
	picked := pickMemories(dir, p.Cwd, injectBudget(env))
	if len(picked) == 0 {
		return
	}
	arm := "on"
	if env.Getenv("CLAUDE_DREAM_AB") == "1" {
		arm = abArm(p.SessionID)
	}
	var ctx strings.Builder
	ctx.WriteString(injectHeader)
	names := make([]string, len(picked))
	for i, m := range picked {
		ctx.WriteString(formatMemory(m.Memory))
		names[i] = m.Name
	}
	if arm == "off" {
		logInjection(env, p.SessionID, arm, nil, names, 0)
		return
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart", "additionalContext": ctx.String(),
	}}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(stdout, string(b))
	logInjection(env, p.SessionID, arm, names, nil, ctx.Len())
}

// logInjection appends one JSONL line per injection for later evaluation.
func logInjection(env Env, sessionID, arm string, names, withheld []string, chars int) {
	b, err := json.Marshal(map[string]any{
		"ts": env.Now().UTC().Format("2006-01-02T15:04:05Z"), "session_id": sessionID, "arm": arm,
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
