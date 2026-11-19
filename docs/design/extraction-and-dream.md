# claude-dream phase 2 (extraction) and phase 3 (dream pass)

Evidence behind these rules (research notes, 2026-11-19): extract from user turns only with an evidence quote (CONPACT); rules/exclusions are what compaction drops; never rewrite a memory body in place, supersede instead ("Useful Memories Become Faulty", MemFSM); gate writes strictly (Xiong et al.); wrong memory is worse than none.

## Modes (safety default for the first nights)
`CLAUDE_DREAM_EXTRACT` = `propose` (default) | `apply` | `off`. `CLAUDE_DREAM_DREAM` = `propose` (default) | `apply` | `off`.
- propose: never touch live memory files. Write candidates to `<memorydir>/.proposed/<YYYY-MM-DD>-<name>.md` and record them in the report.
- apply: write live files as described below. Even in apply mode: never delete, never rewrite a body in place.

## Phase 2: extraction (worker)
Input job: `{session_id, transcript_path, cwd, event, ...}` from the queue.

1. **Offsets.** Per-session state `stateDir/extract/<session_id>.json` holds the byte offset already processed, so PostCompact then SessionEnd don't re-extract the same turns. Read from offset with `eachLine`.
2. **User turns.** A human turn is a line with `type == "user"` and `origin.kind == "human"` (verified on 2.1.328 transcripts: other kinds seen are `task-notification`, `peer`, and lines with no origin — agent hand-backs ("Another Claude session sent a message"), `<bash-input>`/`<bash-stdout>`, `<command-name>`, `<artifact-content-…>`, all NOT human). If a transcript has no `origin` field on any user line (older versions), fall back to: `isMeta` not true, content is a string or text-only array, and text does not start with `<` or with "Another Claude session" / "This session is". Strip `<system-reminder>…</system-reminder>` blocks inside a turn. Keep the preceding assistant message's last `text` block (≤ 800 chars) as context only — it can never be the evidence source. Never copy real transcript content into the repo or tests.
3. **Skip** when fewer than 2 human turns or the transcript cwd is the worker's own state dir.
4. **Focus.** Load the session's focus state (phase 1b) to know which repos the session worked on. Target dir for `scope: project` facts: the single focus repo's memory dir if exactly one, else the cwd's memory dir. `scope: global` → cwd's memory dir.
5. **Prompt.** Chunk human turns to ≤ 24k chars per call. Pass existing memories of the target dirs as `name | type | description` lines. Ask Haiku for a JSON array only:
   `{"action":"add|update|supersede|noop","scope":"project|global","type":"feedback|user|project|reference","name":"kebab-slug","description":"one line","rule":"the fact","why":"…","how_to_apply":"…","evidence_quote":"verbatim from a USER turn","target":"existing memory name for update/supersede"}`.
   Instruct: only durable facts that would change how a future session behaves — corrections, preferences, constraints ("don't touch X"), decisions with reasons, rejected approaches, pointers (paths/URLs/commands). Not task progress, not code the agent can re-read, not anything the assistant said unless the user confirmed it. Prefer noop. At most 5 items per chunk. Keep the prompt in a `prompt.go` const so it is reviewable.
6. **Validate** each item: evidence_quote (whitespace-normalised, case-insensitive) is a substring of a human turn and ≥ 12 chars; name is kebab-case ≤ 60; type valid; for update/supersede the target exists. Drop invalid items and count them in the log by reason.
7. **Write** (apply mode): add → new file `<name>.md` in the existing frontmatter format (name, description, metadata.type; body = rule + `**Why:**` + `**How to apply:**` + `Evidence: "<quote>" (session <id>, <date>)`), plus one pointer line appended to that dir's MEMORY.md (`- [Title](file.md) — hook`) via the conflict-checked atomic writer. update/supersede → write the new fact as a NEW file (name suffixed `-v2`, `-v3` if needed, body names its source), then `supersede(old, new)`. Name collision with an unrelated existing file → treat as noop and log.
8. **Run** an inert headless call (flags verified on 2.1.328 — this combination fired zero tool calls even when the prompt asked for a shell command): `claude -p --model haiku --output-format json --tools "" --strict-mcp-config --disable-slash-commands --no-session-persistence --system-prompt <extractor system prompt>`, prompt on stdin, env `CLAUDE_DREAM_WORKER=1 CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, cwd = stateDir, timeout 180 s. Parse the envelope's `result` field; it may be wrapped in a ```json fence — strip fences, then take the first JSON array. **Daily cap:** at most `CLAUDE_DREAM_MAX_CALLS` (default 40) LLM calls per local day across extraction and dream, counted in stateDir; over the cap, leave jobs queued and log `capped`. The model call is an interface so tests use a fake; tests never execute `claude`.
9. **Log** `stateDir/extract.jsonl`: `{ts, session_id, mode, turns, chunks, proposed|written:[...], dropped:{reason:count}, error}`.

## Phase 3: dream pass
Subcommand `claude-dream dream [--dry-run]`; the worker also runs it after draining the queue if the last dream (mtime of `stateDir/dream.last`) is > 20 h old. Same lock as the worker.

Scope: for each root, encode the root itself and each direct child dir with projectDirName and use the memory dirs that exist. Never decode dir names back to paths.

Deterministic checks (no LLM):
- **index drift:** memory files missing from MEMORY.md; MEMORY.md pointers to missing files.
- **dead anchors:** paths in backticks that start with `~/` or `$HOME` and don't exist on disk → stale candidate. Ignore every other path (memories often describe remote hosts). `[[links]]` to no memory are NOT flagged (they're intentional per the user's convention).
- **duplicates:** pairs in one dir whose description token sets have Jaccard ≥ 0.6, or identical `name`.
- **promote pending proposals:** list `.proposed/` files older than 7 days still unreviewed.

LLM check (one Haiku call per dir with ≥ 4 memories AND any memory file changed since that dir's last LLM check; same model interface and daily cap): find contradictions and merge candidates; output `{kind:"contradiction|merge|stale", files:[...], reason, suggested_text}`. Validate that every file named exists.

Output: `stateDir/dream/<YYYY-MM-DD>.md`, a human-readable review list grouped by dir, each item with the action it would take. Apply mode: fix index drift (append missing pointers; remove dead pointer lines only from MEMORY.md, which is an index not a memory), move confirmed-stale files to `.archive/` (never delete), write merges as a new file + supersede the sources. Propose mode: report only.

## Report subcommand
`claude-dream report [--days N]` (default 1): prints injections (count, sessions by arm, top memories), extractions (proposed/written/dropped by reason), latest dream report path, and the `.proposed/` files awaiting review with their paths. This is what the user reads first.

## Tests
Fake model returning canned JSON; synthetic transcripts covering every user-turn filter case; evidence-quote validation (pass, fail, too short); offsets prevent re-extraction; propose vs apply vs off; update writes -v2 and supersedes old without touching its body; MEMORY.md pointer append; dream: index drift, dead anchor, duplicate, archive in apply mode, nothing touched in propose mode; worker triggers dream only after 20 h.

**State dir:** durable `$XDG_STATE_HOME/claude-dream` or `~/.local/state/claude-dream` (phase 1b moves it there).

## Acceptance (run locally, never commit output)
Dry-run extraction (`claude-dream extract --dry-run <transcript>` subcommand, fake-free, real Haiku) over one real transcript that contains agent hand-backs; confirm in the printed result that no candidate's evidence quote comes from a non-human turn. Report counts only.

## Decisions made while building it
- **Confirmed-stale** means two independent signals: the LLM check calls the file stale *and* it names a home path that no longer exists. Either alone is only reported.
- **`dream --dry-run`** makes no LLM call and writes nothing (no report file, no `dream.last`, no check state). It lists the dirs a real run would send to the model as `llm_pending`.
- **Offsets** advance only past complete lines, after each chunk that succeeds; never on an error, the cap, a skip or a dry run. A file shorter than its stored offset starts again from 0. The no-`origin` fallback is decided over the whole transcript, not only the part after the offset.
- **Memory dirs are never created.** Extraction uses a target dir only if it already exists (the focus repo's, else the cwd's); it creates only `.proposed/` inside one.
- **Proposals** are full memory files with two extra frontmatter lines, `proposed_action` and `proposed_target`, removed when promoted by hand.
- **The model's names are looked up, never joined into a path:** an update target or a dream file must match a file in the dir listing.
- **A capped job** goes back on the queue with every job after it, and the worker stops for the day (no dream either).
- **Review follow-ups:**
  - Headless transcripts (`entrypoint: "sdk-cli"`) are skipped, logged as `headless`.
  - The no-`origin` fallback applies only to transcripts whose `version` is below 2.1.300.
  - `[Request interrupted` lines and `<pasted_content>` blocks are never evidence.
  - Pending proposals are shown to the model, and a candidate whose description overlaps a live memory, a proposal or an earlier item (Jaccard ≥ 0.6) is dropped as `duplicate`.
  - The worker lock is an `flock` held for the worker's lifetime. Claimed queue files are adopted only from dead PIDs. A worker stops after 30 minutes, and a stuck holder is logged, never robbed of its lock.
