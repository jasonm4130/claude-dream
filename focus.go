package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// rootsOf returns the folders whose direct children are repos:
// CLAUDE_DREAM_ROOTS (colon-separated), default $HOME/Code.
func rootsOf(env Env) []string {
	var roots []string
	if v := env.Getenv("CLAUDE_DREAM_ROOTS"); v != "" {
		for _, r := range strings.Split(v, ":") {
			if r = strings.TrimSpace(r); r != "" {
				roots = append(roots, filepath.Clean(r))
			}
		}
	} else if env.Home != "" {
		roots = append(roots, filepath.Join(env.Home, "Code"))
	}
	return roots
}

// repoOf returns the repo path that cwd is inside (or is), or "". A cwd that
// is a root itself, or outside every root, is in no repo.
func repoOf(roots []string, cwd string) string {
	if cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	for _, r := range roots {
		rest, ok := strings.CutPrefix(cwd, r+string(filepath.Separator))
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, string(filepath.Separator))
		if name != "" && !strings.HasPrefix(name, ".") {
			return filepath.Join(r, name)
		}
	}
	return ""
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// toolStrings collects every string in a tool_input value, recursively.
func toolStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			out = append(out, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// repoInString finds a repo path inside s: `<root>/<repo>/…` or exactly
// `<root>/<repo>`. A leading `~/` is expanded. Only directories count.
func repoInString(env Env, roots []string, s string) string {
	if env.Home != "" && strings.HasPrefix(s, "~/") {
		s = filepath.Join(env.Home, s[2:])
	}
	for _, r := range roots {
		prefix := r + "/"
		for rest := s; ; {
			i := strings.Index(rest, prefix)
			if i < 0 {
				break
			}
			rest = rest[i+len(prefix):]
			name := rest
			if j := strings.IndexAny(name, "/ \t\n\"'`;&|)<>"); j >= 0 {
				name = name[:j]
			}
			if name != "" && !strings.HasPrefix(name, ".") && isDir(filepath.Join(r, name)) {
				return filepath.Join(r, name)
			}
		}
	}
	return ""
}

// repoNamedIn returns the one repo a prompt names, or "". Only repo dirs whose
// name has a hyphen or a digit can match, as a whole word, case-insensitive,
// with "-" or "_" also matching a space. Plain single-word names are mostly
// false hits and never match. Naming two or more repos is ambiguous.
func repoNamedIn(env Env, roots []string, prompt string, focus []string) string {
	if prompt == "" {
		return ""
	}
	var found []string
	for _, r := range roots {
		entries, err := env.readDir(r)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || !strings.ContainsAny(name, "-0123456789") {
				continue
			}
			if !e.IsDir() && !isDir(filepath.Join(r, name)) {
				continue
			}
			if nameRegexp(name).MatchString(prompt) {
				found = append(found, filepath.Join(r, name))
			}
		}
	}
	if len(found) != 1 || contains(focus, found[0]) {
		return ""
	}
	return found[0]
}

var (
	nameStart = `(?i)(?:^|[^a-z0-9_-])`
	nameSplit = regexp.MustCompile(`[-_]`)
)

func nameRegexp(name string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(nameStart)
	for _, part := range nameSplit.Split(name, -1) {
		if b.Len() > len(nameStart) {
			b.WriteString(`[-_ ]`)
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString(`(?:$|[^a-z0-9_-]|[-_](?:$|[^a-z0-9]))`)
	return regexp.MustCompile(b.String())
}
