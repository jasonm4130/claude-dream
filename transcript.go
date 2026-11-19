package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const contextChars = 800 // assistant text kept before a human turn

// eachLine calls fn for every complete line of a JSONL file, with the byte
// offset just past that line. Lines can be megabytes (tool results), so it
// reads with no line-length limit. A final line without a newline may still
// be being written; it is left for next time.
func eachLine(path string, fn func(line []byte, end int64)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	var pos int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			pos += int64(len(line))
			fn(line, pos)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// turn is one human message from a transcript.
type turn struct {
	Text    string // the user's words, system reminders removed
	Context string // the assistant's last text before it; never evidence
	End     int64  // byte offset just past the line
}

// transcriptLine is the subset of a transcript record read here.
type transcriptLine struct {
	Type        string `json:"type"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	Cwd         string `json:"cwd"`
	Entrypoint  string `json:"entrypoint"` // "sdk-cli" for headless claude -p runs
	Version     string `json:"version"`    // Claude Code version that wrote the line
	Origin      *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var (
	reminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	// Pasted text is not the user's own words, so it can't be evidence.
	pastedRe = regexp.MustCompile(`(?s)<pasted_content\b[^>]*>.*?</pasted_content>`)
)

// originSince is the first Claude Code version known to write origin on
// user lines; transcripts from it on never use the no-origin fallback.
var originSince = [3]int{2, 1, 300}

// contentText returns a message's text and whether it was text only (a
// string, or an array of text blocks).
func contentText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	textOnly := len(blocks) > 0
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else {
			textOnly = false
		}
	}
	return strings.Join(parts, "\n"), textOnly
}

func cleanTurn(s string) string {
	s = reminderRe.ReplaceAllString(s, "")
	return strings.TrimSpace(pastedRe.ReplaceAllString(s, ""))
}

// parseVersion reads "2.1.328" (ignoring any suffix); ok is false otherwise.
func parseVersion(v string) (out [3]int, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		j := 0
		for j < len(p) && p[j] >= '0' && p[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(p[:j])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// transcriptScan is what one pass over a transcript finds.
type transcriptScan struct {
	Turns     []turn   // human turns, all of them
	Others    []string // every string in non-human messages; kept only on request
	Cwd       string   // the first cwd seen
	Size      int64    // bytes of complete lines
	Headless  bool     // a user line has entrypoint sdk-cli: prompts are machine-written
	sawOrigin bool
	modern    bool // written by a version that always records origin
}

// scanTranscript finds the human turns. A human turn is a user line whose
// origin.kind is "human". Only transcripts from versions before origin
// existed fall back to a heuristic, decided over the whole file: not meta,
// text only, and not starting with "<" or with the wording of an agent
// hand-back. "[Request interrupted" lines are never human. A headless
// transcript (entrypoint sdk-cli) is flagged; its caller skips it.
func scanTranscript(path string, keepOthers bool) (transcriptScan, error) {
	var sc transcriptScan
	type cand struct {
		t        turn
		origin   string   // "" when absent
		fallback bool     // passes the no-origin heuristic
		all      []string // every string in the content, for keepOthers
	}
	var cands []cand
	lastText := ""
	err := eachLine(path, func(line []byte, end int64) {
		sc.Size = end
		var l transcriptLine
		if json.Unmarshal(line, &l) != nil || l.IsSidechain {
			return
		}
		if sc.Cwd == "" && l.Cwd != "" {
			sc.Cwd = l.Cwd
		}
		if v, ok := parseVersion(l.Version); ok && !versionLess(v, originSince) {
			sc.modern = true
		}
		switch l.Type {
		case "assistant":
			var blocks []contentBlock
			if json.Unmarshal(l.Message.Content, &blocks) != nil {
				return
			}
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					lastText = tail(strings.TrimSpace(b.Text), contextChars)
					if keepOthers {
						sc.Others = append(sc.Others, b.Text)
					}
				}
			}
		case "user":
			if l.Entrypoint == "sdk-cli" {
				sc.Headless = true
			}
			text, textOnly := contentText(l.Message.Content)
			c := cand{t: turn{Text: cleanTurn(text), Context: lastText, End: end}}
			if keepOthers {
				c.all = toolStrings(l.Message.Content)
			}
			if l.Origin != nil {
				sc.sawOrigin = true
				c.origin = l.Origin.Kind
			}
			if strings.HasPrefix(c.t.Text, "[Request interrupted") {
				c.origin = "interrupted" // not "human", and not a fallback candidate
			}
			c.fallback = !l.IsMeta && textOnly && c.t.Text != "" &&
				!strings.HasPrefix(c.t.Text, "<") &&
				!strings.HasPrefix(c.t.Text, "Another Claude session") &&
				!strings.HasPrefix(c.t.Text, "This session is")
			cands = append(cands, c)
			if c.origin == "human" || (c.origin == "" && c.fallback) {
				lastText = "" // context belongs to the next human turn only
			}
		}
	})
	for _, c := range cands {
		human := c.origin == "human" && c.t.Text != ""
		if !sc.sawOrigin && !sc.modern {
			human = c.fallback && c.origin == ""
		}
		if human {
			sc.Turns = append(sc.Turns, c.t)
		} else {
			sc.Others = append(sc.Others, c.all...)
		}
	}
	return sc, err
}

// tail keeps the last n bytes of s, cut at a rune boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
