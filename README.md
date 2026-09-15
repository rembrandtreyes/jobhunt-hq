# Job Hunt HQ — local edition

The same page as the hosted artifact, served by a single Go binary with everything stored in a SQLite file. No cgo, no npm, no external services. The SQLite driver is pure Go ([ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3), SQLite compiled to WASM).

## Run

Requires Go 1.23 or newer.

```sh
go mod tidy      # first time only: downloads the SQLite driver
go run .         # → http://127.0.0.1:8787   (creates hq.db next to the binary)
```

Or build once and keep it around:

```sh
go build -o hq .
./hq -db ~/jobhunt/hq.db -addr 127.0.0.1:8787
```

The first run seeds the database from `seed.json` — the 28 researched target companies — and never touches existing rows after that. Delete `hq.db` to start over.

Flags: `-db` (path to the database file, default `hq.db`), `-addr` (listen address, default `127.0.0.1:8787` — keep it on localhost; there is no auth).

## Layout

```
main.go          HTTP server + SQLite schema + REST API + seeding
web/index.html   the page (embedded into the binary at build time)
seed.json        first-run data
```

Rebuild after editing `web/index.html`; the file is embedded with `go:embed`.

## Database

`hq.db` is an ordinary SQLite database (WAL mode). Open it with anything:

```sh
sqlite3 hq.db
```

Tables:

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

## API

All JSON. The page is the only client, but nothing stops a script from using it.

| method | path | body |
|---|---|---|
| GET | `/api/state` | — → `{apps, companies, done, settings}` |
| GET | `/api/export` | same as state, served as a download (backup) |
| PUT | `/api/applications/{id}` | application object (see `Application` in main.go); upsert |
| DELETE | `/api/applications/{id}` | — |
| PUT | `/api/companies/{id}` | company object; upsert |
| DELETE | `/api/companies/{id}` | — |
| PUT | `/api/study` | `{"done": {"w1-resume": true, ...}}` — replaces the full set |
| PUT | `/api/settings` | `{"startDate": "YYYY-MM-DD", "weeklyGoal": 20}` |

## Backup

`cp hq.db hq-$(date +%F).db` while the server is stopped, or `curl -o backup.json localhost:8787/api/export` any time. To move data back into the hosted artifact, the export JSON has the same field names the artifact's database uses.
