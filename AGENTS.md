# AGENTS.md — jobhunt-hq

Instructions for any coding agent working in this repo (Claude Code, Codex, Cursor, Copilot, Gemini CLI, or a human). `CLAUDE.md` imports this file and adds only Claude-specific notes; put anything general here.

## What this is

A personal job-search command center: a single Go binary serving one HTML page, with everything stored in a local SQLite file. Built for one user on localhost. Keep it that way: simple, single-file, no auth, no frameworks. Free and open source (MIT).

Four tabs in `web/index.html`:
- **Today** — day/week counter, weekly stats (applied vs goal, pipeline, interviewing, follow-ups due), a weekday block schedule, and the current week's study focus. All derived client-side from the data below.
- **Applications** — the tracker. Pipeline statuses: `saved, applied, screen, technical, onsite, offer, rejected, withdrawn`. Every application should carry a `nextDate` + `nextAction`; the Today tab surfaces them when due.
- **Study plan** — two 8-week tracks in the page script: `GENERAL_PLAN` (any role, ids `g1-resume`, `g4-exercise`, …) and `ENGINEERING_PLAN` (ids `w1-resume`, `w3-case`, …). The user picks a track in settings (`settings.track`, `general` | `engineering`, default general). Item ids are the keys in `study_progress` and must stay stable across both tracks; never rename, only add. Each track object in `TRACKS` also carries its schedule, afternoon blocks, resources, focus labels, and Weighting text.
- **Companies** — target list with A/B/C priority, why-it-fits, careers link, hiring signal. Seeded from `seed.json` on first run.

## Layout

```
main.go                        HTTP server, SQLite schema, REST API, first-run seeding (Go 1.22+ mux patterns)
main_test.go                   httptest suite against a temp-file SQLite db
web/index.html                 the entire UI (inline CSS + JS, no build step), embedded via go:embed
seed.json                      first-run data — never overwrites existing rows
.claude/skills/find-jobs/      job-search procedure (readable by any agent, see below)
.github/workflows/ci.yml       go vet, gofmt -l, go build, go test on push and PR
README.md                      user-facing docs incl. useful SQL queries
```

## Run / build / verify

```sh
go run .                       # http://127.0.0.1:8787, creates hq.db
go build -o hq . && ./hq -db ~/jobhunt/hq.db
go test ./...                  # must pass
go vet ./... && gofmt -l .     # must be clean before committing
```

Rebuild after editing `web/index.html` (it is embedded at compile time). `hq.db*` is gitignored — never commit a database.

## Stack decisions (keep)

- SQLite driver is `github.com/ncruces/go-sqlite3` (SQLite compiled to WASM, pure Go). **No cgo.** Don't swap to mattn/go-sqlite3.
- `db.SetMaxOpenConns(1)`; WAL mode; `busy_timeout` set via `_pragma` in the DSN.
- Real columns, not JSON blobs, so `sqlite3 hq.db` is queryable by hand. Column names are snake_case; JSON field names are camelCase and must match what the page sends (see the `Application` / `Company` structs).
- `application_events` is an append-only status history, written inside the same transaction as the upsert whenever a row is new or its status changes (`upsertApplication`, shared by PUT and import). Conversion analytics depend on it — don't bypass it.
- Timestamps are server-set RFC3339 UTC; `created_at` is preserved on update. Dates the user enters (`applied_at`, `next_date`) are plain `YYYY-MM-DD` strings and compared as strings.
- Ids match `^[A-Za-z0-9_-]{1,64}$`; the page generates them client-side. Status is validated against the fixed set.
- Listens on `127.0.0.1` by default and has no authentication. Do not add features that assume it is reachable from elsewhere.
- No dependencies beyond the standard library and what is already in `go.mod` without asking the owner.

## API (all JSON)

| method | path | notes |
|---|---|---|
| GET | `/api/state` | `{apps, companies, done, settings}` — the page's only read |
| GET | `/api/export` | same, as a download |
| POST | `/api/import` | the export shape; upserts apps + companies (events on status change), adds study items, applies settings; all-or-nothing on validation; never deletes |
| PUT | `/api/applications/{id}` | upsert; body = Application without id |
| DELETE | `/api/applications/{id}` | also deletes its events |
| PUT | `/api/companies/{id}` | upsert |
| DELETE | `/api/companies/{id}` | |
| PUT | `/api/study` | `{"done": {id: true}}` replaces the set; existing `done_at` preserved |
| PUT | `/api/settings` | `{startDate, weeklyGoal, track}`; zero values are ignored; `track` must be `general` or `engineering` |

The page re-fetches `/api/state` after every write except study toggles (those patch the DOM locally).

## Seed and job search

- `seed.json` companies are employers whose job boards expose a public JSON feed; each feed lists every open role in every function. `careersUrl` is the human page; `sourceUrl` is the feed: Greenhouse `boards-api.greenhouse.io/v1/boards/{slug}/jobs`, Lever `api.lever.co/v0/postings/{slug}?mode=json`, Ashby `api.ashbyhq.com/posting-api/job-board/{slug}`. `why` (total roles + top functions), `signal` (total roles, remote, date), `priority` (A ≥300 / B ≥75 / C by total roles), and `location` (top US/remote) are generated from the feed on the seed date; they are snapshots, not live. The top-level `_note` explains this to users; keep it.
- To add a company, confirm its feed returns JSON with engineering roles, then add a row with both URLs. Never add a company whose feed you did not fetch.
- **Job search procedure:** `.claude/skills/find-jobs/SKILL.md` is written as a Claude Code skill but is plain markdown any agent can follow: read `/api/state`, fetch each `sourceUrl`, filter by title/location/seniority, dedupe by URL against existing apps, show the user a table, and only after confirmation `PUT` each chosen posting as `status: "saved"` with `source`, `url`, `nextAction`, `nextDate`. Never fabricate a posting. Never change an existing application's status.
- The page's default `startDate` is empty so Day 1 is the day the user opens it; the seed sets no start date.

## Front end conventions

- One file, vanilla JS in an IIFE, no dependencies. Fonts come from Google Fonts with real fallback stacks; everything else is inline.
- Theme tokens are CSS variables defined on `:root` (light), overridden under `prefers-color-scheme: dark` and `[data-theme="dark"]`. Add colors as tokens, never as literals inside components.
- All user content goes through `esc()` before being put in innerHTML.
- The `store` object is the only place that talks to the server. Keep everything outside `store` backend-agnostic so the same page can run against a different store later (browser-local, hosted).
- Form controls have stable ids (`a-company`, `c-name`, `set-start`, …); the drawer, settings popover, and toast are the only overlays.

## Working agreements

- Keep changes small and behind the existing shape. Separate commits per concern.
- Add or extend a test in `main_test.go` for any server behavior you change; the suite runs against a temp-file db via `newServer(path)` and `routes()`.
- Do not change the schema, JSON field names, or study item ids without the owner's OK.
- Never commit `hq.db*`, the `hq` binary, or anyone's personal data. The seed is sample data.
- This is a tool people use every morning, not a product. No accounts, no telemetry, no upsells.

## Suggested next work

1. `hq find` subcommand: the job-search procedure above as a Go command, for users without an AI agent.
2. Move the `TRACKS` data out of `web/index.html` into data files so a study track for a specific field (sales, design, data, recruiting) is a data contribution.
3. Optional `GET /api/stats` (conversion by source, days-to-screen) from `application_events`, and a small Stats panel on the Today tab.
