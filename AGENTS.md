# AGENTS.md

A Claude Code hook that keeps auto memory useful: one Go binary that reads a hook payload on stdin. On `PostCompact` and `SessionEnd` it queues a job and starts a detached worker, which extracts durable facts from the session's human turns and runs a daily dream pass over the memory dirs (docs/design/extraction-and-dream.md). On `SessionStart`, `UserPromptSubmit` and `PostToolUse` it tracks which repos the session is working in and injects each newly focused repo's memories once (docs/design/focus-injection.md). README.md explains the phases and the install.

## Rules that are easy to break

- **Go standard library only.** No modules in `go.mod` beyond the language version.
- **Fail open.** Every error path in a hook exits 0 with nothing on stdout or stderr.
- **Never sleep or wait in a hook.** `SessionEnd` has a 1.5 s budget: append to the queue, spawn the worker detached (own session, stdio on /dev/null) and return. Anything slow belongs to `claude-dream work`.
- **Recursion guard and off switch.** `CLAUDE_DREAM_WORKER=1` or `CLAUDE_DREAM_OFF=1` makes every hook exit 0 at once. The worker's own `claude -p` call must set `CLAUDE_DREAM_WORKER=1` and `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`.
- **Lines can be megabytes.** Read transcripts line by line with a reader that has no line limit (team-guard's `eachLine`), never `bufio.Scanner` with its default.
- **State files** live under `$XDG_STATE_HOME/claude-dream` (default `~/.local/state/claude-dream`), not a cache dir: the injection log feeds a long A/B comparison. Written atomically (temp file and rename). Losing a session file must be harmless.
- **PostToolUse is hot.** It fires on every tool call: unless a new repo enters focus it must read only the small session file, never a memory dir or the root listing.
- **Never rewrite a memory body in place.** The worker puts a change in a new file that names its sources. The old file is superseded (`supersede` adds `superseded_by` and `valid_until` to its frontmatter, body untouched) or archived to `memory/.archive/`. Nothing is deleted. Writes refuse if the file's mtime changed since it was read.
- **Inject less, not more.** A wrong memory is worse than none. Per-event budget 2,000 chars (`CLAUDE_DREAM_INJECT_CHARS`, hard cap 9,000), per-session cap 8,000 (`CLAUDE_DREAM_SESSION_CHARS`), a minimum relevance score on the keyword path, and no output at all when nothing passes. Superseded memories are never injected. A memory dir comes from a full repo path, never a name match across projects. Plain single-word repo names never match from prompt text.
- **Propose by default.** `CLAUDE_DREAM_EXTRACT` and `CLAUDE_DREAM_DREAM` default to `propose`: extraction writes only to `<memory dir>/.proposed/`, the dream pass only writes its report to the state dir. Never change these defaults without being asked. Never create a memory dir; only `.proposed/` inside an existing one.
- **Evidence comes from human turns only.** A human turn is a user line with `origin.kind == "human"` (the no-`origin` fallback applies only when no user line in the transcript has one). Assistant text is context, never evidence. Every extracted item must quote a human turn verbatim, at least 12 characters; validation is in Go, not trusted to the prompt.
- **Model output is untrusted.** Names the model returns are looked up in the dir listing, never joined into a path. Frontmatter values are kept on one line.
- **The LLM call is inert and capped.** The `claude -p` flag set in `llm.go` (`--tools ""`, `--strict-mcp-config`, `--disable-slash-commands`, `--no-session-persistence`, prompt on stdin, cwd the state dir) is verified; don't loosen it. Every call goes through `callLLM`, which counts it against `CLAUDE_DREAM_MAX_CALLS` per local day; over the cap, jobs go back on the queue.
- **Prompts live in `prompt.go`** as constants, so a change to them is reviewable on its own. They decide memory quality.
- **Test fixtures are synthetic.** Never read or commit real transcripts or real memory files from `~/.claude`; use invented names (`garden-api`, `/Users/alex/...`). Tests never spawn a real process or run `claude`; they inject `Env.Spawn`, `Env.Extractor` and `Env.LLM`.

## Layout

- `main.go`: the payload, dispatch (hooks and subcommands), guard env vars.
- `env.go`: `Env` (what tests replace) and the atomic write helper.
- `queue.go`: job queue and the detached spawn.
- `worker.go`: lock file, queue drain (stops and requeues when capped), `Extractor` interface, dream trigger.
- `transcript.go`: `eachLine` and the human-turn filter.
- `extract.go`: phase 2: offsets, chunks, validation, propose or write, `extract.jsonl`.
- `dream.go`: phase 3: index drift, dead anchors, duplicates, pending proposals, the LLM check and the review list.
- `llm.go`: `LLM` (the model interface), the inert `claude -p` command, envelope parsing, the daily call cap.
- `prompt.go`: the extraction and dream system prompts.
- `report.go`: the `report`, `dream` and `extract --dry-run` subcommands.
- `inject.go`: the shared hook flow for the three inject events: focus, ranking, budgets, A/B arm, per-session state file and the injection log.
- `focus.go`: roots, repo detection from cwd, tool_input paths and prompt text.
- `memstore.go`: frontmatter, conflict-checked writes, `supersede`, `archiveMemory`.
- `main_test.go`, `focus_test.go`, `extract_test.go`, `dream_test.go`: tests, run end to end through `run()`, `work()` and `subcommand()` over temp dirs with a fake model.
- `docs/design/focus-injection.md`, `docs/design/extraction-and-dream.md`: the designs.

## Checks

```sh
gofmt -l .                  # must print nothing
go vet ./...
go test -race ./...
golangci-lint run ./...
```

## Releasing

Releases are tag-driven. Push an annotated `vX.Y.Z` tag on `main`, and the Release workflow builds darwin/linux × amd64/arm64 with checksums and SBOMs. To install: download the archive for the platform, check it against `checksums.txt`, then `install -m 755` into `~/.local/bin/`. The hook registration lives in the user's `~/.claude/settings.json`, not here.
