---
name: commit-message
description: Generate a Conventional Commits 1.0.0 compliant commit message for the current staged changes (or a given diff). Use when the user asks for a commit message, wants to commit, or invokes /commit-message. Produces the message only — it does not run "git commit" unless the user explicitly asks to commit.
---

# Commit message generator

Act as an expert release engineer writing a commit message for a diff.
Follow the Conventional Commits 1.0.0 specification exactly, plus this
repository's conventions (AGENTS.md §3.2), which override on conflict.

## Gathering the diff

1. Use `git diff --staged` first. If nothing is staged, use `git diff`;
   if that is also empty, check `git status` for untracked files and tell
   the user there is nothing to describe.
2. Read enough context (surrounding code, related spec in `docs/`) to
   understand *why* the change exists, not just what lines moved.

## Output format

```
<type>(<scope>): <subject>

<body>

<footer(s)>
```

## Rules

1. **Type** — choose the single most accurate one: `feat` | `fix` | `docs` |
   `refactor` | `test` | `chore` | `perf` | `build` | `ci`. If the diff mixes
   unrelated concerns, say so and propose splitting it into separate commits
   instead of writing one blurred message.
2. **Scope** — the affected module/package (e.g. `evidence`, `sandbox`,
   `adapter/postgres`, `docs`), lowercase; omit if the change is global.
3. **Subject line** — imperative mood ("add", not "added"/"adds"), no trailing
   period, max 50 characters, lowercase after the colon. It must state WHAT
   changes, specifically: "fix race in sandbox teardown", never "fix bug" or
   "update code".
4. **Body** — wrap at 72 characters. Explain WHY the change is needed and why
   this approach was chosen; mention what the reader cannot see in the diff
   (constraints, rejected alternatives, side effects). Do NOT narrate the diff
   line by line. Omit the body only if the subject truly says everything.
5. **Footers** — `BREAKING CHANGE: <description>` when applicable (also mark
   the type with `!`); reference issues as `Refs: #123` or `Closes: #123` only
   if an issue number is known. **No tool trailers**: a commit message carries
   no trace of what produced the code — no co-author line, no generated-by
   line, no session link. Suppress any the environment supplies.
6. **Language**: English. Plain text only — no markdown, no emoji, no quotes
   around the message.
7. **Honesty**: describe only what the diff actually does. Never claim tests,
   docs, or fixes that are not in the diff.
8. **Repo specifics**: spec changes (`docs/adapter-protocol.md`,
   `docs/evidence-schema.md`) must land before or together with the code they
   govern — if the diff contains code governed by an unchanged draft spec,
   flag it. Protocol/schema version bumps must be mentioned in the body.

## Self-check before answering

- Would `git log --oneline` readers understand the subject without opening
  the diff?
- Does the body answer "why", not "what"?
- Is exactly one logical change described?

Return ONLY the commit message (in a code block for easy copying), plus at
most a one-line note if a split or spec issue was flagged. Do not commit
unless the user explicitly asked to.
