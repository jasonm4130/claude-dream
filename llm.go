package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultMaxCalls = 40 // LLM calls per local day, extraction and dream together
	llmTimeout      = 180 * time.Second
)

// errCapped means the daily LLM call cap is used up. Work stays queued.
var errCapped = errors.New("capped")

// LLM sends one prompt to the model and returns its text answer. Tests
// replace it; they never run claude.
type LLM interface {
	Run(ctx context.Context, system, prompt string) (string, error)
}

// claudeLLM runs claude -p inert: no tools, no MCP, no slash commands, no
// saved session, in the state dir.
type claudeLLM struct{ Dir string }

func (r claudeLLM) Run(ctx context.Context, system, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return "", err
	}
	cmd := claudeCommand(ctx, r.Dir, system)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude: %w: %s", err, firstLine(errb.String()))
	}
	return parseEnvelope(out.Bytes())
}

// claudeCommand builds the headless call. The prompt goes on stdin: --tools
// takes a list and would swallow a positional prompt. The environment stops
// the child's own hooks from queueing more work and keeps it from writing
// auto memory itself.
func claudeCommand(ctx context.Context, dir, system string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--model", "haiku",
		"--output-format", "json",
		"--tools", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--system-prompt", system,
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CLAUDE_DREAM_WORKER=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
	)
	return cmd
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// parseEnvelope reads the --output-format json envelope and returns result.
func parseEnvelope(b []byte) (string, error) {
	var env struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return "", fmt.Errorf("envelope: %w", err)
	}
	if env.IsError {
		return "", fmt.Errorf("claude returned an error (%s)", env.Subtype)
	}
	return env.Result, nil
}

// decodeArray takes the first JSON array out of a model answer, which may be
// wrapped in a code fence or prose. A decoder from the first "[" copes with
// brackets inside strings.
func decodeArray(text string, v any) error {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		text = rest
	}
	i := strings.IndexByte(text, '[')
	if i < 0 {
		return errors.New("no JSON array in answer")
	}
	return json.NewDecoder(strings.NewReader(text[i:])).Decode(v)
}

// callLLM spends one call from the daily budget, then runs the prompt.
func callLLM(ctx context.Context, env Env, system, prompt string) (string, error) {
	if env.LLM == nil {
		return "", errors.New("no llm")
	}
	if !takeCall(env) {
		return "", errCapped
	}
	return env.LLM.Run(ctx, system, prompt)
}

type callCount struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

func callsFile(env Env) string { return filepath.Join(env.StateDir, "llm-calls.json") }

func maxCalls(env Env) int { return envInt(env, "CLAUDE_DREAM_MAX_CALLS", defaultMaxCalls, 10000) }

// takeCall counts one LLM call against today's cap. It reports false, and
// counts nothing, when the cap is reached.
func takeCall(env Env) bool {
	day := env.Now().Local().Format("2006-01-02")
	var c callCount
	if b, err := os.ReadFile(callsFile(env)); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Day != day {
		c = callCount{Day: day}
	}
	if c.Count >= maxCalls(env) {
		return false
	}
	c.Count++
	b, err := json.Marshal(c)
	if err != nil || writeAtomic(callsFile(env), b) != nil {
		return false // an uncounted call could break the cap
	}
	return true
}
