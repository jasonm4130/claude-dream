# Retire hook injection and LLM extraction by default

Status: Proposed (2026-11-20)

## Context

After one day live on a single user's setup, the hooks were measured against that user's local transcripts.

- **Injection.** Memories selected by repo focus were about 90% off-task: they were same-repo neighbours that had nothing to do with what the session was doing. They added about 2 points of recall, at a cost of roughly 1K characters per session.
- **Extraction.** The LLM extractor made 6 proposals and none of them was a clean keep. The main agent had already written every durable memory in-session.
- **Unrelated cause.** Repo AGENTS.md files were not loading for a separate reason, which a Claude Code setting fixes.

These are small samples from one user, one day and one machine. They are enough to doubt the design. They are not enough to prove it wrong in general.

## Decision

Propose disabling the injection hooks (`SessionStart`, `UserPromptSubmit`, `PostToolUse`) and the extraction hooks (`PostCompact`, `SessionEnd`) by default.

The code stays for two weeks so the change can be rolled back. After that, remove it if nothing reopens the question.

Durable facts belong in versioned owner files: a repo's AGENTS.md or docs, a global AGENTS.md, and a home-repo map. A fact reaches those files only through a reviewed diff. Auto memory stays the agent's own scratch space and is not re-injected by this tool.

## Consequences

- Sessions start with less injected context, and no hook runs on every tool call.
- Durable knowledge is reviewed, versioned and visible in one place, not spread across per-project memory dirs.
- The dream pass (index drift, duplicates, dead paths) is untouched by this ADR. Whether it stays is a separate question.
- Facts that exist only in auto memory stop reaching other sessions in the same repo. If that turns out to matter, it will show up as the evidence below.

## What would reopen this

- Sessions that restate or rediscover facts that already existed in memory.
- A labelled eval, with relevance judged per memory, showing injection recall gains that outweigh the context cost.
- A larger or longer sample, across several users, where extraction proposals are mostly clean keeps the main agent did not already write.
