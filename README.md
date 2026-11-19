# claude-dream

A [Claude Code](https://claude.com/claude-code) hook that tidies auto memory between sessions. It is a single Go binary with no dependencies. It reads the hook payload on stdin and always fails open.

## Phases

1. **Plumbing and injection.** `PostCompact` and `SessionEnd` queue a job and start a detached worker. `SessionStart`, `UserPromptSubmit` and `PostToolUse` inject memories for the repos a session is working in.
2. **Extraction.** The worker reads the session's human turns and asks Haiku for durable facts, each backed by a verbatim quote of the user. By default it only proposes them.
3. **Dream pass.** Once a day the worker checks the memory dirs for index drift, dead paths, duplicates, contradictions and merges. By default it only reports.

Nothing is ever rewritten in place or deleted. The designs are in [docs/design/focus-injection.md](docs/design/focus-injection.md) and [docs/design/extraction-and-dream.md](docs/design/extraction-and-dream.md).

## What injection does

Sessions usually start in a folder that holds all your repos, not inside one, so the working directory says little. Instead each session tracks its **focus**: the repos it is actually working in. A repo is a direct child directory of a root (`CLAUDE_DREAM_ROOTS`, colon-separated, default `~/Code`). A repo enters focus when:

1. `SessionStart`: the cwd is inside it. A cwd that is a root itself injects nothing.
2. `PostToolUse`: any string in `tool_input` (file path, command, pattern) contains `<root>/<repo>/` or equals `<root>/<repo>`, with a leading `~/` expanded.
3. `UserPromptSubmit`: the prompt names a repo whose directory name has a hyphen or a digit (`garden-api`, `site-v2`), as a whole word, case-insensitive, with `-` or `_` also matching a space. Plain one-word names never match from prompt text, and a prompt naming two repos is ambiguous and adds none.

When a repo newly enters focus, its memory files from `~/.claude/projects/<repo path with non-alphanumerics as ->/memory/` are injected once per session, ranked by relevance to the prompt or, otherwise, by type (feedback, project, reference, user) and then newest first. On `UserPromptSubmit` it also injects memories from the cwd's memory dir and from each focus repo whose name and description share at least two distinctive words with the prompt. `MEMORY.md`, `.archive/`, `.proposed/` and superseded files are skipped, and a memory dir is never created. There is no LLM involved, and nothing is printed when nothing qualifies.

Budgets: 2,000 characters per event (`CLAUDE_DREAM_INJECT_CHARS`, hard cap 9,000) and 8,000 per session (`CLAUDE_DREAM_SESSION_CHARS`). After a compaction (`SessionStart` with source `compact`) the earlier injections are gone, so the focus repos' top memories are injected again.

Each event that injects or withholds is logged as JSONL in the state dir (`injections.jsonl`: session, event, arm, repos, memories, chars). Set `CLAUDE_DREAM_AB=1` to withhold injection from a deterministic half of sessions (by session id) and log both arms, for evaluation.

## What extraction does

When a session compacts or ends, the worker reads the transcript from where it last stopped. It keeps only **human turns**: user lines with `origin.kind == "human"`. Agent hand-backs, task notifications, peer messages, tool results, shell and slash-command echoes, and `[Request interrupted` lines are not human turns. Transcripts written before `origin` existed (versions before 2.1.300) fall back to text-only lines that are not meta and don't start with `<` or with hand-back wording. Headless transcripts (`entrypoint: "sdk-cli"`, from `claude -p` runs) are skipped, since their prompts are machine-written. System reminders and `<pasted_content>` blocks are stripped: pasted text is not the user's own words. The assistant's last words before each turn go along as context, but they are never evidence.

Sessions with fewer than two new human turns are skipped. The turns go to Haiku in chunks of up to 24,000 characters, with the existing memories listed as `name | type | description` (proposals awaiting review are listed too, marked `pending review`). The prompts are in [prompt.go](prompt.go). The model is told to prefer saving nothing and to return at most five items per chunk. Each item must quote the user word for word. The worker drops an item when:

- the quote is shorter than 12 characters or isn't found in a human turn (case and whitespace are ignored);
- the name isn't kebab-case of at most 60 characters, or the type is unknown;
- an update names a memory that doesn't exist;
- an add reuses a name that is taken;
- its description's words overlap (Jaccard ≥ 0.6) a live memory, a pending proposal or an item already accepted in this run (`duplicate`).

The log records every drop by reason.

The call is inert: `claude -p --model haiku --output-format json --tools "" --strict-mcp-config --disable-slash-commands --no-session-persistence`, run in the state dir with `CLAUDE_DREAM_WORKER=1` and `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, with a 180 s timeout.

Project facts go to the memory dir of the session's single focus repo, otherwise to the cwd's. Global facts go to the cwd's. A memory dir is used only if it already exists.

## What the dream pass does

`claude-dream dream` (and the worker, when the last pass is more than 20 hours old) looks at the memory dir of each root and of each repo directly under a root. It checks, without an LLM:

- **index drift:** memory files that `MEMORY.md` doesn't link, and links to files that are gone;
- **dead paths:** paths in backticks that start with `~/` or `$HOME` and no longer exist (other paths are ignored, since memories often describe remote hosts);
- **duplicates:** two files with the same name, or descriptions whose words overlap by a Jaccard index of 0.6 or more;
- **pending proposals:** files in `.proposed/` unreviewed for more than seven days.

A dir with at least four memories, where some file changed since its last check, also gets one Haiku call. That call looks for contradictions, merges and stale files, and every file it names must exist. The review list goes to `<state dir>/dream/<date>.md`, grouped by dir, each item with the action it would take.

In apply mode the pass appends missing pointers to `MEMORY.md` and removes pointer lines to missing files (`MEMORY.md` is an index, not a memory). It writes a merge as a new file and supersedes the sources. It moves a file to `.archive/` only when the LLM calls it stale and it also names a dead path.

## Modes and switches

| Variable | Default | Meaning |
| --- | --- | --- |
| `CLAUDE_DREAM_EXTRACT` | `propose` | `propose` writes candidates to `<memory dir>/.proposed/<date>-<name>.md` and never touches a live memory file. `apply` writes them as live files with a `MEMORY.md` pointer. `off` skips extraction. |
| `CLAUDE_DREAM_DREAM` | `propose` | `propose` only writes the review list. `apply` also fixes the index, writes merges and archives confirmed-stale files. `off` stops the worker from dreaming. |
| `CLAUDE_DREAM_MAX_CALLS` | `40` | LLM calls per local day, extraction and dream together. Over the cap, jobs stay queued for the next day. |
| `CLAUDE_DREAM_ROOTS` | `~/Code` | Colon-separated folders whose direct children are repos. |
| `CLAUDE_DREAM_INJECT_CHARS` | `2000` | Injection budget per event (hard cap 9,000). |
| `CLAUDE_DREAM_SESSION_CHARS` | `8000` | Injection budget per session. |
| `CLAUDE_DREAM_AB` | unset | `1` withholds injection from half of all sessions, for evaluation. |
| `CLAUDE_DREAM_OFF` | unset | `1` makes every hook exit 0 at once. |
| `CLAUDE_DREAM_WORKER` | unset | The same; the recursion guard for the worker's own `claude` calls. |

Even in apply mode, nothing is deleted and no memory body is rewritten. A change is a new file that names its source, and the old file gets `superseded_by` and `valid_until` in its frontmatter.

## Commands

```sh
claude-dream report [--days N]                  # start here: injections, extractions, latest dream, proposals to review
claude-dream dream [--dry-run]                  # run the dream pass now; --dry-run prints the list, writes nothing, calls no LLM
claude-dream extract --dry-run <transcript>     # real Haiku extraction over one transcript, printed as JSON; writes nothing
```

`report` covers the last day by default. It lists injections (events, sessions per A/B arm, the most injected memories) and extractions (proposed, written, dropped by reason, skipped, errors, LLM calls today). It also gives the latest dream report's path and every `.proposed/` file waiting for review.

`extract --dry-run` counts against the daily cap, but it saves no offset and writes no file. For each candidate it also says whether the quote appears in non-human text too.

## Reviewing proposals

A proposal is a complete memory file with two extra frontmatter lines: `proposed_action` (`add`, `update` or `supersede`) and, for updates, `proposed_target` (the file it replaces). Its body ends with the evidence quote and the session id. To promote one by hand:

1. Read it, and edit the text if needed.
2. Move it into the memory dir without the date prefix: `mv .proposed/2026-11-19-no-laptop-migrations.md no-laptop-migrations.md`.
3. Delete the `proposed_action` and `proposed_target` lines.
4. Add a pointer to `MEMORY.md`: `- [No laptop migrations](no-laptop-migrations.md) — <its description>`.
5. For an update or supersede, add `superseded_by: <new name>` and `valid_until: <today>` to the top of the target's frontmatter, leaving its body alone.

To reject a proposal, delete the file. The proposal is yours to delete; the tool never deletes one. The dream pass lists any proposal older than seven days.

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
- **One worker at a time.** The worker holds an `flock` on `worker.lock` for its lifetime (the kernel releases it if the process dies; the file holds its PID and start time for people). It adopts a leftover claimed queue file only if the PID that claimed it is dead. After 30 minutes it stops taking jobs and leaves the rest queued. A worker that finds the lock held exits; if the holder has run over 30 minutes it logs `holder_stuck` to `worker.jsonl` but never steals the lock.
- **State** lives under `$XDG_STATE_HOME/claude-dream` (default `~/.local/state/claude-dream`) and is written atomically. It holds the queue, the lock, per-session focus files (pruned after 7 days), the injection log and `extract.jsonl`. It also holds per-session extraction offsets (`extract/`), the daily call count, dream reports (`dream/`) and `dream.last`. Deleting it is harmless apart from losing the logs, though extraction then reads old sessions again.
- **Memory files are never deleted.** Superseding adds frontmatter fields; archiving moves a file to `.archive/`.

## Development

```sh
gofmt -l .                  # must print nothing
go vet ./...
go test -race ./...
golangci-lint run ./...
```

The tests drive `run()`, `work()` and the subcommands end to end, over synthetic transcripts and memory files in temp directories, with a fake model. They never run `claude`. Releases are built by GoReleaser from `vX.Y.Z` tags.

## License

[MIT](LICENSE)
