# AGENTS.md — read before acting in this repo

1. Read `~/.config/punkjazz/project-defaults.md` before project work. It is the canonical local source for model routing, email identity, and machine addresses. If absent on a managed laptop, fetch `/Users/example/knowledge-base/cognition/laptop-kit/v2/project-defaults.md` through the `kb` helper before choosing infrastructure.
2. Read docs/PROJECT-AUTONOMY.md first. Gates and protected_paths bind
   every agent, whatever harness you run in.
3. Unattended sessions: an open Flight Recorder exists at
   .hermes-fr.yaml (opened by hermes-session). Before your first
   action, fill does_not_count from the task prompt with near-miss
   outcomes that do not qualify as completion — verification fails if
   it stays empty. Append actions with receipts; commit messages carry
   the FR id.
4. Human-present, human-typed sessions (no agent CLI): commits use the
   `manual:` prefix and judgments go to the cognition inbox via kb-log.
5. Never modify verification (linters, tests config, audit scripts,
   hooks, CI) in the same task whose output it checks. Park the unit
   and escalate instead.
6. Branch work only. Merging to the default branch needs an explicit
   in-session grant.
7. Corrections and dead ends from this session go to the cognition
   inbox (kb-log) so the shared brain compounds.
