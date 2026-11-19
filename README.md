# claude-dream

A [Claude Code](https://claude.com/claude-code) hook that tidies auto memory between sessions. It is a single Go binary with no dependencies. It reads the hook payload on stdin and always fails open.

## Phases

1. **Plumbing and injection (built).** `PostCompact` and `SessionEnd` queue a job and start a detached worker, which for now drains the queue and does nothing with it. `SessionStart` injects the memories that match the working directory.
2. **Extraction (planned).** The worker reads the session transcript and asks `claude -p --model haiku` for new facts, written as new memory files that name their sources.
3. **Consolidation (planned).** Duplicate or stale memories are superseded or archived. Nothing is rewritten in place or deleted.

## What injection does

On `SessionStart` it finds `~/.claude/projects/<cwd with non-alphanumerics as ->/memory/`, skips `MEMORY.md`, `.archive/` and superseded files, and scores each memory by how many words of its name and description match the repo name and path. Memories below the minimum score are never injected. If none pass, it prints nothing. The default budget is 2,000 characters; set `CLAUDE_DREAM_INJECT_CHARS` to change it (hard cap 9,000). There is no LLM involved. Each injection is logged as JSONL under the cache dir (`claude-dream/injections.jsonl`).

Set `CLAUDE_DREAM_AB=1` to withhold injection from a deterministic half of sessions (by session id) and log both arms, for evaluation.

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
  "PostCompact": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-dream" } ] }
  ],
  "SessionEnd": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-dream" } ] }
  ]
}
```

Check the install:

```sh
claude-dream --version
echo '{"hook_event_name":"SessionStart"}' | claude-dream; echo "exit $?"   # exit 0, no output
```

## How it works

- **One binary for every event.** It dispatches on `hook_event_name` and ignores any event it doesn't handle.
- **It never waits.** Hooks append one line to `queue.jsonl` and spawn `claude-dream work` in its own session.
- **One worker at a time.** The worker holds `worker.lock` (its PID inside; a dead PID makes the lock stale) and drains the queue.
- **State** lives under your user cache directory (`claude-dream/`) and is written atomically. Deleting it is harmless.
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
