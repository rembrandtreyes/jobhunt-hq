# Job Hunt HQ

A job-search command center you run on your own machine. One small Go binary serves a single page and keeps everything in a SQLite file you own. No accounts, no cloud, no npm, no cgo.

![Job Hunt HQ — the Today tab](docs/screenshot.png)

## Why

A job search is a pipeline with a deadline. Spreadsheets lose the follow-ups, note apps lose the structure, and hosted trackers own your data. Job Hunt HQ is the middle ground: a focused page that opens every morning, tells you what is due, tracks every application through the pipeline, and keeps the whole thing in a database you can query with `sqlite3`.

## What you get

Four tabs on one page:

- **Today** — day and week counter for your search, weekly stats (applied vs. goal, pipeline, interviewing, follow-ups due), a weekday block schedule, and this week's study focus.
- **Applications** — the tracker. Each application moves through `saved → applied → screen → technical → onsite → offer` (or `rejected` / `withdrawn`), and every status change is logged so you can see which sources actually convert.
- **Study plan** — an 8-week checklist for interview prep (algorithms, system design, behavioral). Progress is saved per item.
- **Companies** — your target list with A/B/C priority, why each one fits, a careers link, and the latest hiring signal.

Everything is stored locally. Export a JSON backup any time.

## Quick start

Requires [Go](https://go.dev/dl/) 1.24 or newer.

```sh
git clone https://github.com/<you>/jobhunt-hq
cd jobhunt-hq
go run .          # → http://127.0.0.1:8787   (creates hq.db in the current directory)
```

Or build once and keep the binary around:

```sh
go build -o hq .
./hq -db ~/jobhunt/hq.db -addr 127.0.0.1:8787
```

Flags:

| flag | default | meaning |
|---|---|---|
| `-db` | `hq.db` | path to the SQLite database file (created on first run) |
| `-addr` | `127.0.0.1:8787` | listen address |

The server has **no authentication** and is meant for one person on one machine. Keep it bound to localhost.

## Make it yours

- **Target companies** — the first run seeds the Companies tab from `seed.json`. Edit that file before your first run (or just delete `hq.db` and re-run) to start from your own list. Seeding never overwrites rows that already exist.
- **Start date and weekly goal** — set them from the gear icon in the top right. The day/week counter and the "applied this week" bar are computed from these.
- **Study plan and daily schedule** — the `PLAN`, `SCHEDULE`, and `WEEKEND` arrays near the top of the script in `web/index.html`. Study item ids are the keys in `study_progress`, so add new ones rather than renaming. Rebuild after editing; the page is embedded into the binary at compile time.

## Your data

`hq.db` is an ordinary SQLite database (WAL mode). Open it with anything:

```sh
sqlite3 hq.db
```

| table | what |
|---|---|
| `applications` | one row per job; `status` is one of saved, applied, screen, technical, onsite, offer, rejected, withdrawn |
| `application_events` | append-only log of every status change (`application_id`, `status`, `at`) — written automatically |
| `companies` | the target list, priority A/B/C |
| `study_progress` | one row per checked study-plan item (`item_id`, `done_at`) |
| `settings` | key/value: `startDate`, `weeklyGoal` |

Queries you will actually want:

```sql
-- pipeline right now
SELECT status, COUNT(*) FROM applications GROUP BY status;

-- which sources convert to a recruiter screen or better
SELECT a.source,
       COUNT(*)                                   AS applied,
       SUM(EXISTS (SELECT 1 FROM application_events e
                   WHERE e.application_id = a.id
                     AND e.status IN ('screen','technical','onsite','offer'))) AS reached_screen
FROM applications a
WHERE a.status <> 'saved'
GROUP BY a.source ORDER BY reached_screen DESC;

-- days from application to first screen
SELECT a.company, a.role,
       julianday(MIN(e.at)) - julianday(a.applied_at) AS days_to_screen
FROM applications a JOIN application_events e ON e.application_id = a.id
WHERE e.status = 'screen' AND a.applied_at <> ''
GROUP BY a.id ORDER BY days_to_screen;

-- follow-ups due
SELECT company, role, next_date, next_action FROM applications
WHERE next_date <> '' AND next_date <= date('now') AND status NOT IN ('rejected','withdrawn')
ORDER BY next_date;
```

### Backup

`cp hq.db hq-$(date +%F).db` while the server is stopped, or `curl -o backup.json localhost:8787/api/export` any time.

## API

All JSON. The page is the only client, but nothing stops a script from using it.

| method | path | body |
|---|---|---|
| GET | `/api/state` | — → `{apps, companies, done, settings}` |
| GET | `/api/export` | same as state, served as a download (backup) |
| PUT | `/api/applications/{id}` | application object (see `Application` in `main.go`); upsert |
| DELETE | `/api/applications/{id}` | — (also removes its events) |
| PUT | `/api/companies/{id}` | company object; upsert |
| DELETE | `/api/companies/{id}` | — |
| PUT | `/api/study` | `{"done": {"w1-resume": true, ...}}` — replaces the full set |
| PUT | `/api/settings` | `{"startDate": "YYYY-MM-DD", "weeklyGoal": 20}` |

Ids are client-generated and match `^[A-Za-z0-9_-]{1,64}$`. Timestamps are server-set RFC3339 UTC; dates you enter are plain `YYYY-MM-DD`.

## How it is built

```
main.go          HTTP server, SQLite schema, REST API, first-run seeding
main_test.go     httptest suite against a temp-file database
web/index.html   the whole UI — inline CSS + vanilla JS, no build step, embedded with go:embed
seed.json        first-run data (never overwrites existing rows)
```

- SQLite via [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3) — SQLite compiled to WebAssembly, so the binary is pure Go and cross-compiles anywhere. No cgo.
- Real columns rather than JSON blobs, so the database stays hand-queryable.
- One connection, WAL mode, `busy_timeout` set. Plenty for one person.

## Development

```sh
go test ./...                    # run the test suite
go vet ./... && gofmt -l .       # both must be clean before committing
go build -o hq .                 # rebuild after editing web/index.html
```

CI (`.github/workflows/ci.yml`) runs `go vet`, `gofmt -l`, `go build`, and `go test` on every push and pull request, using the Go version from `go.mod`.

`hq.db*` is gitignored. Never commit a database.

## Contributing

Issues and pull requests are welcome. Keep changes small and in the spirit of the tool: single binary, single page, local first, no dependencies beyond the standard library and the SQLite driver.
