# Retire hook injection and LLM extraction by default

Status: Proposed (2026-11-20)

## Context

After one day live on a single user's setup, the hooks were assessed against that user's local transcripts. The figures below come from a transcript replay, which is a proxy and not a live measurement. The live sample was one session with injection on.

- **Injection.** In the replay, memories selected by repo focus were about 90% off-task: same-repo neighbours unrelated to what the session was doing. They added about 2 points of recall, at roughly 1K characters per session. The replay's fidelity to the real hook was imperfect.
- **Extraction.** The extractor made 6 proposals and none was a clean keep. The main agent had already written durable memories in-session, and some facts went straight to repo docs.
- **Unrelated cause.** In this one user's layout, repo AGENTS.md files were not loading because of a specific Claude Code loading setting. That is particular to the setup and is not a general fix.

These are small samples from one user and one day. They are enough to doubt the design, not to prove it wrong in general.

## Decision

Propose disabling all five hook registrations by default: the injection hooks (`SessionStart`, `UserPromptSubmit`, `PostToolUse`) and the two queue hooks (`PostCompact`, `SessionEnd`).

The queue hooks also trigger the worker that runs the daily dream pass, so disabling them stops hook-triggered dream runs as well as extraction. The manual `claude-dream dream` and `report` commands remain available. Claude Code's native auto memory is unaffected.

Durable facts belong in versioned owner files: a repo's AGENTS.md or docs, a global AGENTS.md, and a home-repo map. A fact reaches those files only through a reviewed diff.

After two weeks, decide whether to archive the repository.

## Consequences

- Sessions start with less injected context, and no hook runs on every tool call.
- Durable knowledge is reviewed and versioned in one place instead of spread across per-project memory dirs.
- Native auto memory still loads same-repo memory, and facts remain distributed across owners.
- Memory-dir hygiene (index drift, duplicates, dead paths) is no longer run automatically.
- Facts that exist only in auto memory may stop reaching other sessions through this tool. If that matters, it shows up as the evidence below.

## What would reopen this

- Sessions that restate or rediscover facts that already existed in memory.
- A labelled eval, with relevance judged per memory, showing injection recall gains that outweigh the context cost.
- A larger or longer sample across several users, where extraction proposals are mostly clean keeps the main agent did not already write.
