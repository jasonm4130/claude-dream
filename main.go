// Command claude-dream is a Claude Code hook that keeps auto memory tidy.
//
// One binary serves every event: it reads the hook payload on stdin and
// dispatches on hook_event_name.
//
//   - PostCompact and SessionEnd: append a job to a queue and start a
//     detached `claude-dream work` process. The hook never waits.
//   - SessionStart: inject the most relevant auto-memory files for the
//     working directory as additionalContext. No LLM is involved.
//
// Any failure exits 0 with no output, so a bug here never blocks a session.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Payload holds the hook input fields this tool reads. Fields come and go
// between Claude Code versions; every one is optional.
type Payload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Trigger        string `json:"trigger"` // PostCompact: manual|auto
	Reason         string `json:"reason"`  // SessionEnd
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "version":
			fmt.Printf("claude-dream %s %s %s\n", version, commit, date)
			return
		case "work":
			os.Exit(work(defaultEnv()))
		}
	}
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr, defaultEnv()))
}

// run returns the process exit code, which is always 0: hooks fail open.
func run(stdin io.Reader, stdout, _ io.Writer, env Env) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	if env.Getenv("CLAUDE_DREAM_WORKER") == "1" || env.Getenv("CLAUDE_DREAM_OFF") == "1" {
		return 0
	}
	var p Payload
	if err := json.NewDecoder(stdin).Decode(&p); err != nil {
		return 0
	}
	switch p.HookEventName {
	case "PostCompact", "SessionEnd":
		enqueue(p, env)
	case "SessionStart":
		inject(p, stdout, env)
	}
	return 0
}
