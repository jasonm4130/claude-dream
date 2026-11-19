# claude-dream phase 1b: focus-based injection

Problem: the user starts nearly every Claude Code session in a parent folder of all their repos (e.g. `~/Code`), not inside a repo. Claude Code already loads that parent's `MEMORY.md`, but each repo's own memory dir (e.g. `-Users-alex-Code-garden-api/memory`, often 10–30 files) is never loaded. The current SessionStart-by-cwd injection therefore does nothing useful.

## Design

**Roots.** `CLAUDE_DREAM_ROOTS` (colon-separated, default `$HOME/Code`). A *repo* is a direct child directory of a root (skip names starting with `.`). The repo's memory dir is `memoryDir(<root>/<repo>)` (existing projectDirName rule).

**Primary triggers are cwd and tool paths (3 below); prompt names are secondary.**

**State dir.** Durable, not a cache: `$XDG_STATE_HOME/claude-dream` or `~/.local/state/claude-dream` (logs feed a months-long A/B comparison). Move the existing cache-dir usage there.

**Memory dirs.** Compute a repo's dir by encoding `<root>/<repo>` with projectDirName; only read dirs that already exist. Never create memory dirs.

**Focus.** Per-session state file `stateDir/sessions/<session_id>.json` (atomic writes; harmless to lose; prune files older than 7 days opportunistically): `{focus: [repo paths], injected: [memory file paths], chars: N}`. A repo enters focus when:
1. SessionStart: cwd is inside `<root>/<repo>` (cwd == repo or below it).
2. UserPromptSubmit: the prompt names a repo whose dir name contains `-` or a digit (e.g. `garden-api`, `site-v2`), as a whole word, case-insensitive, where `-`/`_` may also match a space. Single plain-word repo names (`mapper`, `kiln`, `lantern`) never match from prompt text: they are mostly false hits in ordinary prose. If one prompt names 2+ repos, treat as ambiguous and add none. Only directories count (not files like AGENTS.md).
3. PostToolUse: any string field in `tool_input` (recursively: file_path, path, notebook_path, command, pattern…) contains `<root>/<repo>/` or equals `<root>/<repo>`. Also expand a leading `~/`.

**Injection.** When one or more repos newly enter focus on an event, inject (once per memory file per session):
- that repo's memory files, ranked by score against the current prompt's words (UserPromptSubmit) or, otherwise, by type priority (feedback > project > reference > user) then mtime newest first;
- on UserPromptSubmit only, additionally memories from the cwd's own memory dir and each focus repo whose name/description share ≥2 non-stopword tokens (len≥4) with the prompt — this is the "relevant to what we're talking about" path. Same threshold rule: nothing below it is injected.
- Skip superseded memories, MEMORY.md, `.archive/`, `.proposed/`, and anything already in `injected`.
- Per-event budget `CLAUDE_DREAM_INJECT_CHARS` (default 2000, hard cap 9000); per-session total cap `CLAUDE_DREAM_SESSION_CHARS` (default 8000). Header line: `claude-dream: memories for <repo> (from ~/.claude/projects/…/memory):` so the agent knows the source and can Read the full file.
- Each memory formatted as now (name, description, body truncated to 1200 chars).

**Output shapes** (verified 2026-11-19 on Claude Code 2.1.328: a headless run showed additionalContext from UserPromptSubmit and PostToolUse reaching the model): SessionStart / UserPromptSubmit / PostToolUse each emit `{"hookSpecificOutput":{"hookEventName":"<event>","additionalContext":"..."}}`. Emit nothing at all when there is nothing to inject.

**SessionStart in a root (cwd == root):** inject nothing (Claude Code already loads that MEMORY.md). SessionStart with `source` == "compact" or "resume": re-inject nothing that was already injected this session — but after compaction the earlier injections are gone from context, so on source=="compact" clear `injected` and re-inject the focus repos' top memories within the event budget.

**A/B.** Keep existing `CLAUDE_DREAM_AB` arm logic; the arm applies to every injection in the session. Log every event that injected or withheld: `{session_id, ts, event, arm, repos, memories, withheld, chars}`.

**Performance.** PostToolUse fires on every tool call: fast path must avoid reading memory dirs unless a new repo entered focus. List root children at most once per invocation. Target <10 ms.

## Tests (synthetic temp dirs only)
- cwd == root: SessionStart emits nothing.
- prompt mentioning "garden api" injects that repo's memories once; a second prompt mentioning it injects nothing new.
- PostToolUse with tool_input.file_path under root/repo injects; PostToolUse with an unrelated path emits nothing and does not read memory dirs (assert via a counter or by making the dir unreadable).
- prompt-keyword path injects a cwd memory whose description matches, and not one below threshold.
- per-session cap respected across events.
- source=compact re-injects.
- A/B off arm withholds on all three events and logs `withheld`.
- plain-word repo names never match from prompt text; a prompt naming two repos adds none.

## Verified payloads (Claude Code 2.1.328)
- SessionStart: cwd, hook_event_name, session_id, source (startup|resume|clear|compact|fork), transcript_path
- UserPromptSubmit: + prompt, prompt_id, permission_mode
- PostToolUse: + tool_name, tool_input, tool_response, tool_use_id, duration_ms
- SessionEnd: + reason
