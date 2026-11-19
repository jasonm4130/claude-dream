# AGENTS.md

A Claude Code hook that keeps auto memory useful: one Go binary that reads a hook payload on stdin. On `PostCompact` and `SessionEnd` it queues a job and starts a detached worker. On `SessionStart` it injects the few memories that match the working directory. README.md explains the phases and the install.

## Rules that are easy to break

- **Go standard library only.** No modules in `go.mod` beyond the language version.
- **Fail open.** Every error path in a hook exits 0 with nothing on stdout or stderr.
- **Never sleep or wait in a hook.** `SessionEnd` has a 1.5 s budget: append to the queue, spawn the worker detached (own session, stdio on /dev/null) and return. Anything slow belongs to `claude-dream work`.
- **Recursion guard and off switch.** `CLAUDE_DREAM_WORKER=1` or `CLAUDE_DREAM_OFF=1` makes every hook exit 0 at once. The worker's own `claude -p` call must set `CLAUDE_DREAM_WORKER=1` and `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`.
- **Lines can be megabytes.** Read transcripts line by line with a reader that has no line limit (team-guard's `eachLine`), never `bufio.Scanner` with its default.
- **State files** live under `os.UserCacheDir()/claude-dream`, written atomically (temp file and rename). Losing one must be harmless.
- **Never rewrite a memory body in place.** The worker puts a change in a new file that names its sources. The old file is superseded (`supersede` adds `superseded_by` and `valid_until` to its frontmatter, body untouched) or archived to `memory/.archive/`. Nothing is deleted. Writes refuse if the file's mtime changed since it was read.
- **Inject less, not more.** A wrong memory is worse than none. Default budget 2,000 chars (`CLAUDE_DREAM_INJECT_CHARS`, hard cap 9,000), a minimum relevance score, and no output at all when nothing passes. Superseded memories are never injected. The memory dir comes from the full cwd path, never a name match across projects.
- **Test fixtures are synthetic.** Never read or commit real transcripts or real memory files from `~/.claude`. Tests never spawn a real process or run `claude`; they inject `Env.Spawn` and `Env.Extractor`.

## Layout

- `main.go`: the payload, dispatch, guard env vars.
- `env.go`: `Env` (what tests replace) and the atomic write helper.
- `queue.go`: job queue and the detached spawn.
- `worker.go`: lock file, queue drain, `Extractor` interface, `claude -p` command builder.
- `inject.go`: SessionStart scoring, budget, A/B arm and the injection log.
- `memstore.go`: frontmatter, conflict-checked writes, `supersede`, `archiveMemory`.
- `main_test.go`: tests, run end to end through `run()`.

## Checks

```sh
gofmt -l .                  # must print nothing
go vet ./...
go test -race ./...
golangci-lint run ./...
```

## Releasing

Releases are tag-driven. Push an annotated `vX.Y.Z` tag on `main`, and the Release workflow builds darwin/linux × amd64/arm64 with checksums and SBOMs. To install: download the archive for the platform, check it against `checksums.txt`, then `install -m 755` into `~/.local/bin/`. The hook registration lives in the user's `~/.claude/settings.json`, not here.
