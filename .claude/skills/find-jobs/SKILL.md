---
name: find-jobs
description: Search the live job boards of the companies in Job Hunt HQ for open roles in any function — engineering, product, design, data, sales, marketing, customer success, recruiting, finance, operations — that match the titles and locations you give it, then save the ones you pick into the tracker as `saved` applications. USE WHEN find jobs, search jobs, look for roles, what's open, refresh openings, add postings to my tracker.
---

# find-jobs

Job Hunt HQ seeds its Companies tab with employers whose job boards expose a public JSON feed. Each feed lists every open role at the company, in every function, not just engineering. This skill reads those feeds, filters them against what the user is looking for, shows the matches, and (after the user confirms) saves them into the tracker so they show up on the Applications tab with a next action.

The local server must be running: `go run .` in the repo, or `./hq -db ~/jobhunt/hq.db`. Default base URL is `http://127.0.0.1:8787`.

## Inputs to establish first

Ask only for what is missing. Sensible defaults in brackets.

- **Titles / keywords** — required, no default; ask if not given. Any function works: "account executive", "product manager", "recruiter", "customer success manager", "financial analyst", "UX designer", "data analyst", "marketing manager", "backend engineer". Take several at once.
- **Seniority** [any] — e.g. "Senior", "Staff", "no intern/new grad"
- **Location** [Remote, or the user's city] — match against the posting's location text
- **Companies** [all seeded companies] — or a subset by name
- **Limit** [25 matches shown]

## Step 1 — Read the tracker

```sh
curl -s http://127.0.0.1:8787/api/state
```

If this fails, stop and tell the user to start the server. From the JSON keep:

- `companies[]` — each has `name`, `careersUrl` (human page), and `sourceUrl` (the JSON feed this skill reads).
- `apps[]` — existing applications. Their `url` values are the dedupe set: never save a posting whose URL is already present.

## Step 2 — Fetch each feed

`sourceUrl` is one of three shapes. Fetch with `curl -s -A "jobhunt-hq/1.0" <sourceUrl>` and parse:

| Feed host | Shape | Title | Location | Posting URL | Posted |
|---|---|---|---|---|---|
| `boards-api.greenhouse.io` | `{ "jobs": [ … ] }` | `title` | `location.name` | `absolute_url` | `first_published` (else `updated_at`) |
| `api.lever.co` | `[ … ]` (array) | `text` | `categories.location` | `hostedUrl` | `createdAt` (milliseconds since epoch) |
| `api.ashbyhq.com` | `{ "jobs": [ … ] }` | `title` | `location` | `jobUrl` | `publishedAt` |

Reduce the posted value to a `YYYY-MM-DD` date; it is what the tracker shows in its Posted column, so the user can tell a fresh posting from a stale one.

Fetch feeds in parallel where you can (they are independent). A feed that fails or returns non-JSON is skipped and reported at the end, never guessed.

## Step 3 — Filter

A posting matches when its title matches at least one title keyword (case-insensitive) AND its location matches the location preference. Apply the seniority rule to the title. Drop anything whose URL is already in `apps[]`.

Location rules: boards list remote roles per country ("UK | Remote", "Spain (Remote)", "Remote - US"), so "remote" alone over-matches. When the user says remote, use their country (ask once if unknown; default United States) and require the location text to contain "remote" AND that country, or to be exactly "Remote" with no country. A city preference matches when the city name appears in the location text. An empty location is unknown, not a match; list unknowns separately if the user wants them.

## Step 4 — Show, then confirm

Present a compact table: company, title, location, posted date, URL. Newest first within a company; group by company, cap at the limit, say how many more matched. Ask which to save: "all", a list of numbers, or "none". Do not save anything before the user answers.

## Step 5 — Save the chosen postings

For each chosen posting, `PUT /api/applications/{id}` with a client-generated id: lowercase company and title joined by `-`, non `[A-Za-z0-9_-]` characters replaced with `-`, truncated to 64 characters. Body:

```json
{
  "company": "Grafana Labs",
  "role": "Senior Backend Engineer",
  "url": "https://job-boards.greenhouse.io/grafanalabs/jobs/123",
  "status": "saved",
  "source": "find-jobs",
  "location": "Remote, US",
  "postedAt": "<posting date from the feed, YYYY-MM-DD; omit if the feed has none>",
  "nextAction": "Read the posting and decide whether to apply",
  "nextDate": "<today + 2 days, YYYY-MM-DD>",
  "notes": "Found by find-jobs on <today>"
}
```

`status` must be `saved`; the user moves it to `applied` themselves. Report how many were saved and remind the user they are on the Applications tab and that the Today tab will surface them when `nextDate` arrives.

## Step 6 — Update the company signal (optional, cheap)

For each company you fetched, `PUT /api/companies/{id}` with the existing company object and `signal` set to `"<N> open roles, <R> remote, as of <today>"` (all functions). This keeps the Companies tab honest without the user doing anything.

## Beyond the seeded companies

If the user names a company that is not in the tracker, look for its board: try `https://boards-api.greenhouse.io/v1/boards/<slug>/jobs`, `https://api.lever.co/v0/postings/<slug>?mode=json`, and `https://api.ashbyhq.com/posting-api/job-board/<slug>` with the obvious slug (lowercase company name, no spaces). If one answers with JSON, offer to add the company via `PUT /api/companies/{slug}` with `careersUrl` and `sourceUrl` filled in, then include it in the search. If none answer, fall back to a web search for `"<company> careers <title>"` and treat results as unverified until the user opens them.

## Rules

- Never fabricate a posting. Every saved row comes from a fetched feed or a page the user was shown.
- Never change an existing application's status.
- Stay on `127.0.0.1`. The tracker has no auth by design and must never be reached from elsewhere.
