package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxNameTries bounds every loop that looks for a free file name.
const maxNameTries = 50

// ErrConflict means the file changed since it was read.
var ErrConflict = errors.New("memory file changed since it was read")

// Memory is one auto-memory file: YAML frontmatter plus a markdown body.
type Memory struct {
	Path         string
	Name         string
	Description  string
	Type         string // metadata.type
	SupersededBy string // frontmatter superseded_by
	Body         string
	ModTime      time.Time // at read time; pass back to writeMemory
}

// parseMemory reads the minimal frontmatter subset auto memory uses: top-level
// `name` and `description`, and `type` nested under `metadata`.
func parseMemory(data string) Memory {
	var m Memory
	rest, ok := strings.CutPrefix(strings.ReplaceAll(data, "\r\n", "\n"), "---\n")
	if !ok {
		m.Body = data
		return m
	}
	fm, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		m.Body = data
		return m
	}
	m.Body = strings.TrimPrefix(strings.TrimPrefix(body, "\n"), "\n")
	inMeta := false
	for line := range strings.SplitSeq(fm, "\n") {
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		key, val, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		switch {
		case !indented:
			inMeta = key == "metadata"
			switch key {
			case "name":
				m.Name = val
			case "description":
				m.Description = val
			case "superseded_by":
				m.SupersededBy = val
			}
		case inMeta && key == "type":
			m.Type = val
		}
	}
	return m
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// readMemory loads and parses a memory file, recording its mtime.
func readMemory(path string) (Memory, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Memory{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Memory{}, err
	}
	m := parseMemory(string(b))
	m.Path, m.ModTime = path, st.ModTime()
	if m.Name == "" {
		m.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	return m, nil
}

// writeMemory replaces path atomically, but only if its mtime still equals
// readAt (the zero time means the file must not exist yet). A narrow window
// remains between the check and the rename; losing it is harmless because
// nothing here deletes.
func writeMemory(path string, data []byte, readAt time.Time) error {
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !readAt.IsZero() {
			return ErrConflict
		}
	case err != nil:
		return err
	case readAt.IsZero() || !st.ModTime().Equal(readAt):
		return ErrConflict
	}
	return writeAtomic(path, data)
}

// archiveMemory moves a memory file into <dir>/.archive/. It never deletes,
// and never overwrites an earlier archived file of the same name.
func archiveMemory(path string) (string, error) {
	dir := filepath.Join(filepath.Dir(path), ".archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(path))
	for i := 1; ; i++ {
		_, err := os.Lstat(dst)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err // unreadable: never guess past it
		}
		if i > maxNameTries {
			return "", errors.New("archive: no free name")
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s.%d", filepath.Base(path), i))
	}
	return dst, os.Rename(path, dst)
}

// supersede marks a memory as replaced by newName. It only adds the
// frontmatter fields superseded_by and valid_until; the body is never
// touched. It refuses if the file changed since it was read, and when the
// file has no frontmatter to extend.
func supersede(path, newName string, now time.Time) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok || strings.ContainsAny(newName, "\r\n") {
		return errors.New("cannot supersede: no frontmatter")
	}
	if parseMemory(text).SupersededBy != "" {
		return nil // already superseded; keep the first record
	}
	add := "superseded_by: " + newName + "\nvalid_until: " + now.UTC().Format(time.RFC3339) + "\n"
	return writeMemory(path, []byte("---\n"+add+rest), st.ModTime())
}
