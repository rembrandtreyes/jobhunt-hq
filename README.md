# Job Hunt HQ

A job-search command center you run on your own machine. Free and open source (MIT). One small Go binary serves a single page and keeps everything in a SQLite file you own. No accounts, no cloud, no npm, no cgo.

I built it for my own search and I'm sharing it so you can run yours. If it helps, tell a friend who's looking.

![Job Hunt HQ — the Today tab](docs/screenshot.png)

## What you get

Four tabs on one page:

- **Today** — day and week counter for your search, weekly stats (applied vs. goal, pipeline, interviewing, follow-ups due), a weekday block schedule, and this week's study focus.
- **Applications** — the tracker. Each application moves through `saved → applied → screen → technical → onsite → offer` (or `rejected` / `withdrawn`). Every status change is logged, so you can see which sources actually convert.
- **Study plan** — an 8-week interview-prep checklist for software engineers (algorithms, system design, behavioral, applications, outreach, mocks). Progress is saved per item.
- **Companies** — 41 engineering employers to start from, each with a live job board. See below.

Everything is stored locally. Export a JSON backup any time and import it on another machine.

## Quick start

Requires [Go](https://go.dev/dl/) 1.24 or newer.

```sh
git clone https://github.com/rembrandtreyes/jobhunt-hq
cd jobhunt-hq
go run .          # → http://127.0.0.1:8787   (creates hq.db in the current directory)
```

Open the page, click the gear icon, set your **start date** and **weekly goal**. That's the whole setup.

To keep a binary around:

```sh
go build -o hq .
./hq -db ~/jobhunt/hq.db -addr 127.0.0.1:8787
```

| flag | default | meaning |
|---|---|---|
| `-db` | `hq.db` | path to the SQLite database file (created on first run) |
| `-addr` | `127.0.0.1:8787` | listen address |

The server has **no authentication** and is meant for one person on one machine. Keep it on localhost.

## What's seeded on first run

The first run loads `seed.json`: **41 companies that are hiring software, backend, frontend, and DevOps/SRE engineers**, chosen because their job boards (Greenhouse, Lever, or Ashby) expose a public feed. For each one:

- **Priority** A / B / C is set by how many engineering roles were open on 2026-09-15 (A = 100 or more, B = 20 or more, C = fewer). Re-rank them for yourself; it's your list.
- **Why** and **Signal** summarize that day's board: how many engineering roles, how many remote, the most-listed location.
- **Careers link** opens the board. The hidden `sourceUrl` on each row is the board's JSON feed, which is what the job search below reads.

Seeding never overwrites rows that already exist. To start from your own list, edit `seed.json` before the first run, or delete `hq.db` and run again. The Companies tab lets you add, edit, and delete freely.

## Find jobs

**With Claude Code.** The repo ships a project skill at `.claude/skills/find-jobs/`. With the server running, open Claude Code in the repo and say something like:

> find senior backend and devops roles, remote or Phoenix

It reads every company's live feed, filters by your titles and location, skips postings you already have, shows you a table, and saves the ones you pick as `saved` applications with a follow-up date. Name a company that isn't in your list and it will look for that company's board and offer to add it.

**Without Claude.** Every company's careers link opens its board. Each row's feed URL is also plain JSON you can read from a terminal; for a Greenhouse board:

```sh
curl -s https://boards-api.greenhouse.io/v1/boards/grafanalabs/jobs \
  | python3 -c 'import json,sys; [print(j["title"], "—", j["location"]["name"], "—", j["absolute_url"]) for j in json.load(sys.stdin)["jobs"] if "engineer" in j["title"].lower()]'
```

Lever feeds are `https://api.lever.co/v0/postings/<slug>?mode=json` (fields `text`, `categories.location`, `hostedUrl`) and Ashby feeds are `https://api.ashbyhq.com/posting-api/job-board/<slug>` (fields `title`, `location`, `jobUrl`).

## Every morning

1. Open the Today tab. Clear the follow-ups that are due.
2. Log every application the moment you send it: **+ Log an application**, set the status, and always fill **next action** and **next date**. That's what the Today tab surfaces later.
3. When something moves (screen booked, rejection, offer), change the status. The history is kept automatically.
4. Tick off the study plan as you go. The week's focus is on the Today tab.

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

### Backup, and moving between machines

`cp hq.db hq-$(date +%F).db` while the server is stopped, or `curl -o backup.json localhost:8787/api/export` any time. To load a backup into another install:

```sh
curl -X POST localhost:8787/api/import -H 'Content-Type: application/json' --data-binary @backup.json
```

Import merges: it adds and updates, never deletes.

## API

All JSON. The page is the only client, but nothing stops a script from using it.

| method | path | body |
|---|---|---|
| GET | `/api/state` | — → `{apps, companies, done, settings}` |
| GET | `/api/export` | same as state, served as a download (backup) |
| POST | `/api/import` | the export shape; upserts apps and companies, adds study items, applies settings |
| PUT | `/api/applications/{id}` | application object (see `Application` in `main.go`); upsert |
| DELETE | `/api/applications/{id}` | — (also removes its events) |
| PUT | `/api/companies/{id}` | company object; upsert |
| DELETE | `/api/companies/{id}` | — |
| PUT | `/api/study` | `{"done": {"w1-resume": true, ...}}` — replaces the full set |
| PUT | `/api/settings` | `{"startDate": "YYYY-MM-DD", "weeklyGoal": 20}` |

Ids are client-generated and match `^[A-Za-z0-9_-]{1,64}$`. Timestamps are server-set RFC3339 UTC; dates you enter are plain `YYYY-MM-DD`.

## How it's built

```
main.go                        HTTP server, SQLite schema, REST API, first-run seeding
main_test.go                   httptest suite against a temp-file database
web/index.html                 the whole UI — inline CSS + vanilla JS, no build step, embedded with go:embed
seed.json                      first-run data (never overwrites existing rows)
.claude/skills/find-jobs/      the Claude Code skill that searches the job boards
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

Issues and pull requests are welcome. Good first contributions: more companies with public board feeds in `seed.json`, a study plan for a different role, and fixes to the page. Keep the spirit of the tool: single binary, single page, local first, no dependencies beyond the standard library and the SQLite driver.

## License

[MIT](LICENSE). Use it, fork it, share it.
