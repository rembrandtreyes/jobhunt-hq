# CLAUDE.md — jobhunt-hq

@AGENTS.md

Everything about this repo (what it is, layout, run/build/verify, stack decisions, API, seed, front-end conventions, working agreements) lives in `AGENTS.md` above so every agent reads the same instructions. This file adds only what is specific to Claude Code.

## Claude Code specifics

- The `find-jobs` project skill is at `.claude/skills/find-jobs/SKILL.md` and is available automatically when Claude Code runs in this directory. Invoke it for anything like "find backend roles", "what's open at X", "refresh openings". It needs the local server running.
- Use the built-in Read/Grep/Glob tools for repo work; the codebase is a few Go files, one page, and the JSON under `tracks/` and `seed.json`, so no subagents are needed for navigation.
- Before claiming a change works: `go test ./...`, `go vet ./...`, `gofmt -l .`, and for anything visible in the page, run the server and load it in a real browser.
