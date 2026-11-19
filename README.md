# claude-dream

A [Claude Code](https://claude.com/claude-code) hook that tidies auto memory between sessions. It is a single Go binary with no dependencies. It reads the hook payload on stdin and always fails open.

## Phases

1. **Plumbing and injection (built).** `PostCompact` and `SessionEnd` queue a job and start a detached worker, which for now drains the queue and does nothing with it. `SessionStart`, `UserPromptSubmit` and `PostToolUse` inject memories for the repos a session is working in.
2. **Extraction (planned).** The worker reads the session transcript and asks `claude -p --model haiku` for new facts, written as new memory files that name their sources.
3. **Consolidation (planned).** Duplicate or stale memories are superseded or archived. Nothing is rewritten in place or deleted.

## What injection does

Sessions usually start in a folder that holds all your repos, not inside one, so the working directory says little. Instead each session tracks its **focus**: the repos it is actually working in. A repo is a direct child directory of a root (`CLAUDE_DREAM_ROOTS`, colon-separated, default `~/Code`). A repo enters focus when:

1. `SessionStart`: the cwd is inside it. A cwd that is a root itself injects nothing.
2. `PostToolUse`: any string in `tool_input` (file path, command, pattern) contains `<root>/<repo>/` or equals `<root>/<repo>`, with a leading `~/` expanded.
3. `UserPromptSubmit`: the prompt names a repo whose directory name has a hyphen or a digit (`garden-api`, `site-v2`), as a whole word, case-insensitive, with `-` or `_` also matching a space. Plain one-word names never match from prompt text, and a prompt naming two repos is ambiguous and adds none.

When a repo newly enters focus, its memory files from `~/.claude/projects/<repo path with non-alphanumerics as ->/memory/` are injected once per session, ranked by relevance to the prompt or, otherwise, by type (feedback, project, reference, user) and then newest first. On `UserPromptSubmit` it also injects memories from the cwd's memory dir and from each focus repo whose name and description share at least two distinctive words with the prompt. `MEMORY.md`, `.archive/`, `.proposed/` and superseded files are skipped, and a memory dir is never created. There is no LLM involved, and nothing is printed when nothing qualifies.

Budgets: 2,000 characters per event (`CLAUDE_DREAM_INJECT_CHARS`, hard cap 9,000) and 8,000 per session (`CLAUDE_DREAM_SESSION_CHARS`). After a compaction (`SessionStart` with source `compact`) the earlier injections are gone, so the focus repos' top memories are injected again.

Each event that injects or withholds is logged as JSONL in the state dir (`injections.jsonl`: session, event, arm, repos, memories, chars). Set `CLAUDE_DREAM_AB=1` to withhold injection from a deterministic half of sessions (by session id) and log both arms, for evaluation. The design is in [docs/design/focus-injection.md](docs/design/focus-injection.md).

## Switches

- `CLAUDE_DREAM_OFF=1`: every hook exits 0 at once.
- `CLAUDE_DREAM_WORKER=1`: the same, used as the recursion guard for the worker's own `claude` calls.

## Install

Build it and copy the binary:

```sh
go build -o claude-dream . && install -m 755 claude-dream ~/.local/bin/
```

Then register the hooks in `~/.claude/settings.json`:

```json
"hooks": {
  "SessionStart": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-dream" } ] }
  ],
  "UserPromptSubmit": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-dream" } ] }
  ],
  "PostToolUse": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-dream" } ] }
  ],
  "PostCompact": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-dream" } ] }
  ],
  "SessionEnd": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-dream" } ] }
  ]
}
```

`PostToolUse` needs no matcher (or the matcher `.*`): the tool name does not matter, only the paths in `tool_input`. It runs on every tool call, so it exits quickly unless a new repo enters focus.

Check the install:

```sh
claude-dream --version
echo '{"hook_event_name":"SessionStart"}' | claude-dream; echo "exit $?"   # exit 0, no output
```

## How it works

- **One binary for every event.** It dispatches on `hook_event_name` and ignores any event it doesn't handle.
- **It never waits.** Hooks append one line to `queue.jsonl` and spawn `claude-dream work` in its own session.
- **One worker at a time.** The worker holds `worker.lock` (its PID inside; a dead PID makes the lock stale) and drains the queue.
- **State** lives under `$XDG_STATE_HOME/claude-dream` (default `~/.local/state/claude-dream`) and is written atomically: the queue, the lock, per-session focus files (pruned after 7 days) and the injection log. Deleting it is harmless apart from losing the log.
- **Memory files are never deleted.** Superseding adds frontmatter fields; archiving moves a file to `.archive/`.

## Development

```sh
gofmt -l .                  # must print nothing
go vet ./...
go test -race ./...
golangci-lint run ./...
```

The tests drive `run()` end to end over synthetic memory files in temp directories. Releases are built by GoReleaser from `vX.Y.Z` tags.

## License

[MIT](LICENSE)
