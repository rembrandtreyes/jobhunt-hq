# AGENTS.md — jobhunt-hq

Instructions for any coding agent working in this repo (Claude Code, Codex, Cursor, Copilot, Gemini CLI, or a human). `CLAUDE.md` imports this file and adds only Claude-specific notes; put anything general here.

## What this is

A personal job-search command center: a single Go binary serving one HTML page, with everything stored in a local SQLite file. Built for one user on localhost. Keep it that way: simple, single-file, no auth, no frameworks. Free and open source (MIT).

Four tabs in `web/index.html`:
- **Today** — day/week counter, weekly stats (applied vs goal, pipeline, interviewing, follow-ups due), the day block by block, and the current week's study focus. All derived client-side from the data below. The schedule and the focus each have an Edit button; what the user saves lives in `settings.schedule` / `settings.focus` and overrides the track (see "Per-user customization").
- **Applications** — the tracker. Pipeline statuses: `saved, applied, screen, technical, onsite, offer, rejected, withdrawn`. Every application should carry a `nextDate` + `nextAction`; the Today tab surfaces them when due.
- **Study plan** — 8-week tracks loaded from `tracks/*.json`, one file per track: `general` (any role, ids `g1-…`) and `engineering` (ids `w1-resume`, `w3-case`, …). The user picks a track in settings (`settings.track`, a track id, default general). Item ids are the keys in `study_progress` and must stay stable and unique across all tracks; never rename, only add. Each track file also carries its schedule, afternoon blocks, weekend, resources, groups, focus labels, rules, and weighting text (see "Study tracks").
- **Companies** — target list with A/B/C priority, why-it-fits, careers link, hiring signal. Seeded from `seed.json` on first run.

## Layout

```
main.go                        HTTP server, SQLite schema, REST API, first-run seeding (Go 1.22+ mux patterns)
tracks.go                      loads + validates tracks/*.json at startup, serves GET /api/tracks, injects them into the page
customize.go                   validation + storage of the per-user schedule and focus settings
main_test.go, tracks_test.go, customize_test.go
                               httptest suite against a temp-file SQLite db; testing/quick property tests for the validators
web/index.html                 the entire UI (inline CSS + JS, no build step), embedded via go:embed; has a __TRACKS__ placeholder
tracks/*.json                  the study tracks, one file each, compiled into the binary (contributions go here)
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
| GET | `/api/state` | `{apps, companies, done, settings}` — the page's only read after boot |
| GET | `/api/tracks` | the loaded tracks keyed by id, exactly the bytes injected into the page's `<script id="tracks-data">` |
| GET | `/api/export` | same, as a download |
| POST | `/api/import` | the export shape; upserts apps + companies (events on status change), adds study items, applies settings; all-or-nothing on validation; never deletes |
| PUT | `/api/applications/{id}` | upsert; body = Application without id |
| DELETE | `/api/applications/{id}` | also deletes its events |
| PUT | `/api/companies/{id}` | upsert |
| DELETE | `/api/companies/{id}` | |
| PUT | `/api/study` | `{"done": {id: true}}` replaces the set; existing `done_at` preserved |
| PUT | `/api/settings` | `{startDate, weeklyGoal, track, schedule, focus}`; zero values of the first three are ignored; `track` must be a loaded track id; `schedule` and `focus` are validated (`customize.go`), stored as canonical JSON, `null` deletes the row, absent leaves it alone |

The page re-fetches `/api/state` after every write except study toggles (those patch the DOM locally).

## Seed and job search

- `seed.json` companies are employers whose job boards expose a public JSON feed; each feed lists every open role in every function. `careersUrl` is the human page; `sourceUrl` is the feed: Greenhouse `boards-api.greenhouse.io/v1/boards/{slug}/jobs`, Lever `api.lever.co/v0/postings/{slug}?mode=json`, Ashby `api.ashbyhq.com/posting-api/job-board/{slug}`. `why` (total roles + top functions), `signal` (total roles, remote, date), `priority` (A ≥300 / B ≥75 / C by total roles), and `location` (top US/remote) are generated from the feed on the seed date; they are snapshots, not live. The top-level `_note` explains this to users; keep it.
- To add a company, confirm its feed returns JSON with engineering roles, then add a row with both URLs. Never add a company whose feed you did not fetch.
- **Job search procedure:** `.claude/skills/find-jobs/SKILL.md` is written as a Claude Code skill but is plain markdown any agent can follow: read `/api/state`, fetch each `sourceUrl`, filter by title/location/seniority, dedupe by URL against existing apps, show the user a table, and only after confirmation `PUT` each chosen posting as `status: "saved"` with `source`, `url`, `nextAction`, `nextDate`. Never fabricate a posting. Never change an existing application's status.
- The page's default `startDate` is empty so Day 1 is the day the user opens it; the seed sets no start date.

## Study tracks

- A track is one file, `tracks/<id>.json`, compiled in with `go:embed` and loaded by `loadTracks` (`tracks.go`) at startup. Fields the page reads: `id`, `label`, `sub`, `weighting`, `rulesTitle`, `rules`, `focusLabels`, `groups`, `resources` (`{n, u, d}`), `schedule` (`{t, d, w, h, c, vary?}`; the `vary` block takes `afternoon[dow]`), `afternoon` (`{"1".."5": {w, h}}`), `weekend` (`{"0"|"6": [block]}`), `plan` (weeks with `n`, `theme`, `targets`, `focus`, `items` `{id, g, t}`). Unknown fields pass through to the page untouched.
- Startup validation, also run by `go test`: `id` equals the filename stem; label, at least one week and one schedule block; weeks numbered 1..N in order, each with a theme, items, and as many `focus` lines as `focusLabels`; every item id matches the id regex and is unique across all tracks (they are `study_progress` keys). A bad file stops the server with a message naming the file and the problem.
- To add a track: copy `tracks/general.json`, pick an id prefix for your item ids (`s1-…`), fill it in, run `go test ./...`. The page lists tracks in the settings popover and on the Study tab, General first, the rest by label. Never edit ids of an existing track.
- The server replaces the single `__TRACKS__` placeholder in `web/index.html` with the tracks JSON (HTML-escaped by `encoding/json`) inside `<script id="tracks-data" type="application/json">`, so the page renders without a second request. The same bytes are served at `GET /api/tracks`.

## Per-user customization

- `settings.schedule` — `{"weekday"|"mon"|"tue"|"wed"|"thu"|"fri"|"sat"|"sun": [{t, d, w, h, c}]}`. A day key is a complete list for that day; `weekday` covers Mon–Fri when the day has no key of its own; a day with neither uses the track. Caps: 40 blocks per day, `w` required, `t` ≤ 16, `d` ≤ 24, `w` ≤ 80, `h` ≤ 300 characters, `c` in `""|deep|apply|rest`. The page derives `c` from the title when parsing the editor text.
- `settings.focus` — `{"track": "general", "labels": [..], "weeks": {"1": [..], ..}}`. `track` (optional on the API, always set by the page) names the track it was written for, and the page applies the override only while that track is selected; switching tracks hides it, switching back shows it. Labels replace `focusLabels`; a week's lines replace that week's `focus` on Today and on the Study tab; anything missing comes from the track. Caps: 6 labels of 1–40 characters, weeks 1–12, 6 lines of ≤ 300 characters each; `track` must be a loaded id. The schedule is deliberately not track-scoped: one person, one day.
- Both are rows in the settings table holding canonical JSON (`validateSchedule` / `validateFocus` in `customize.go`). On PUT and import, `null` deletes the row and an absent key leaves it alone. Export carries them.
- The page's editors are plain textareas with a text format (`time | length | title | what to do` under `# Weekdays` / `# Monday` … headings; `# Labels` + `# Week N` sections for the focus). The pure helpers (`resolveSchedule`, `scheduleToText`, `parseScheduleText`, `resolveFocus`, `focusToText`, `parseFocusText`) sit between the `customization: pure helpers` markers in the page script and take no DOM or state, so they can be evaluated headlessly. On save the page drops days/weeks/labels that equal the track's, so untouched parts keep following the track.

## Front end conventions

- One file, vanilla JS in an IIFE, no dependencies. Fonts come from Google Fonts with real fallback stacks; everything else is inline.
- Theme tokens are CSS variables defined on `:root` (light), overridden under `prefers-color-scheme: dark` and `[data-theme="dark"]`. Add colors as tokens, never as literals inside components.
- All user content goes through `esc()` before being put in innerHTML.
- The `store` object is the only place that talks to the server. Keep everything outside `store` backend-agnostic so the same page can run against a different store later (browser-local, hosted). `store.saveSettings(patch)` sends the three scalar settings plus whatever `patch` adds, e.g. `{ schedule: null }`.
- `TRACKS` is parsed from the `tracks-data` script tag at boot; `TRACK_IDS` is the display order and `DEFAULT_TRACK` is `general` when present. Never put track content back into the page.
- Editors (schedule, focus) are hidden `.panel-b.editor` siblings of the card body, toggled by `data-action` buttons; `closeEditors()` runs on every tab switch.
- Form controls have stable ids (`a-company`, `c-name`, `set-start`, …); the drawer, settings popover, and toast are the only overlays.

## Working agreements

- Keep changes small and behind the existing shape. Separate commits per concern.
- Add or extend a test in `main_test.go` for any server behavior you change; the suite runs against a temp-file db via `newServer(path)` and `routes()`.
- Do not change the schema, JSON field names, or study item ids without the owner's OK.
- Never commit `hq.db*`, the `hq` binary, or anyone's personal data. The seed is sample data.
- This is a tool people use every morning, not a product. No accounts, no telemetry, no upsells.

## Suggested next work

1. `hq find` subcommand: the job-search procedure above as a Go command, for users without an AI agent.
2. Field-specific tracks (sales, design, data, recruiting, product): each is one file in `tracks/` now, see "Study tracks". Optionally a `-tracks <dir>` flag to load extra track files at runtime without rebuilding.
3. Optional `GET /api/stats` (conversion by source, days-to-screen) from `application_events`, and a small Stats panel on the Today tab.
