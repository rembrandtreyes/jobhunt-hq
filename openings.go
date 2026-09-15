package main

// Openings: search the seeded companies' job boards from the page.
//
//	GET /api/openings?q=<role words>&loc=<location words>[&refresh=1]
//
// The feeds are the companies' sourceUrl values. Three public ATS feeds are
// understood — Greenhouse, Lever, Ashby — and only their API hosts are
// fetched, so a company row can never point this endpoint at anything
// else. Feeds are cached for an hour in memory; main() warms the cache in
// the background after startup. The endpoint reads the companies table and
// writes nothing.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type opening struct {
	CompanyID string `json:"companyId"`
	Company   string `json:"company"`
	Title     string `json:"title"`
	Location  string `json:"location"`
	URL       string `json:"url"`
	Posted    string `json:"posted,omitempty"` // YYYY-MM-DD when the board gives a date
}

type failedBoard struct {
	CompanyID string `json:"companyId"`
	Company   string `json:"company"`
	Reason    string `json:"reason"`
}

// boardRow is one posting as parsed from a feed, before the company is attached.
type boardRow struct {
	Title, Location, URL, Posted string
}

type feedEntry struct {
	rows []boardRow
	at   time.Time
	err  string
}

// feedCache fetches and remembers board feeds. allow decides which URLs may
// be fetched at all; tests point it at an httptest server.
type feedCache struct {
	client  *http.Client
	allow   func(*url.URL) bool
	ttl     time.Duration // how long a good fetch is reused
	failTTL time.Duration // how long a failure is reused before retrying
	limit   int           // concurrent fetches
	mu      sync.Mutex
	entries map[string]*feedEntry
}

const (
	maxOpenings  = 500
	feedTimeout  = 10 * time.Second
	maxFeedBytes = 8 << 20
)

var feedHosts = map[string]bool{"boards-api.greenhouse.io": true, "api.lever.co": true, "api.ashbyhq.com": true}

func knownBoard(u *url.URL) bool { return u.Scheme == "https" && feedHosts[u.Host] }

func newFeedCache(client *http.Client) *feedCache {
	return &feedCache{client: client, allow: knownBoard, ttl: time.Hour, failTTL: 5 * time.Minute, limit: 16, entries: map[string]*feedEntry{}}
}

func (c *feedCache) stale(e *feedEntry) bool {
	if e == nil {
		return true
	}
	if e.err != "" {
		return time.Since(e.at) > c.failTTL
	}
	return time.Since(e.at) > c.ttl
}

// fetchAll returns an entry for every url, fetching the stale ones
// concurrently (bounded by limit).
func (c *feedCache) fetchAll(ctx context.Context, urls []string, refresh bool) map[string]*feedEntry {
	var need []string
	c.mu.Lock()
	for _, u := range urls {
		if refresh || c.stale(c.entries[u]) {
			need = append(need, u)
		}
	}
	c.mu.Unlock()

	sem := make(chan struct{}, c.limit)
	var wg sync.WaitGroup
	for _, u := range need {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e := c.fetchOne(ctx, u)
			c.mu.Lock()
			c.entries[u] = e
			c.mu.Unlock()
		}(u)
	}
	wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*feedEntry, len(urls))
	for _, u := range urls {
		out[u] = c.entries[u]
	}
	return out
}

func (c *feedCache) fetchOne(ctx context.Context, raw string) *feedEntry {
	now := time.Now()
	u, err := url.Parse(raw)
	if err != nil || !c.allow(u) {
		return &feedEntry{at: now, err: "not a known board"}
	}
	ctx, cancel := context.WithTimeout(ctx, feedTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return &feedEntry{at: now, err: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "jobhunt-hq/"+version)
	// A known board could redirect anywhere; every hop must pass the same allow-list.
	client := *c.client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		if !c.allow(r.URL) {
			return fmt.Errorf("redirect refused")
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		return &feedEntry{at: now, err: trimErr(err)}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return &feedEntry{at: now, err: fmt.Sprintf("HTTP %d", res.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxFeedBytes))
	if err != nil {
		return &feedEntry{at: now, err: trimErr(err)}
	}
	rows, err := parseBoard(body)
	if err != nil {
		return &feedEntry{at: now, err: err.Error()}
	}
	return &feedEntry{rows: rows, at: now}
}

func trimErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i < len(s)-2 {
		s = s[i+2:] // "Get \"https://…\": dial tcp: …" → the last, most useful part
	}
	return s
}

// parseBoard understands the three feed shapes by their structure, not
// their host: Lever is a top-level array; Greenhouse and Ashby are objects
// with a jobs array whose items differ only in field names.
func parseBoard(body []byte) ([]boardRow, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty feed")
	}
	if body[0] == '[' { // Lever
		var jobs []struct {
			Text       string `json:"text"`
			HostedURL  string `json:"hostedUrl"`
			CreatedAt  int64  `json:"createdAt"` // ms since epoch
			Categories struct {
				Location string `json:"location"`
			} `json:"categories"`
		}
		if err := json.Unmarshal(body, &jobs); err != nil {
			return nil, fmt.Errorf("feed is not JSON we understand")
		}
		rows := make([]boardRow, 0, len(jobs))
		for _, j := range jobs {
			title := strings.TrimSpace(j.Text) // boards ship stray tabs and spaces in titles
			u := webURL(j.HostedURL)
			if title == "" || u == "" {
				continue
			}
			posted := ""
			if j.CreatedAt > 0 {
				posted = time.UnixMilli(j.CreatedAt).UTC().Format("2006-01-02")
			}
			rows = append(rows, boardRow{Title: title, Location: strings.TrimSpace(j.Categories.Location), URL: u, Posted: posted})
		}
		return rows, nil
	}
	var feed struct { // Greenhouse and Ashby
		Jobs []struct {
			Title       string          `json:"title"`
			AbsoluteURL string          `json:"absolute_url"` // Greenhouse
			JobURL      string          `json:"jobUrl"`       // Ashby
			UpdatedAt   string          `json:"updated_at"`   // Greenhouse
			PublishedAt string          `json:"publishedAt"`  // Ashby
			Location    json.RawMessage `json:"location"`     // Greenhouse {name}, Ashby string
		} `json:"jobs"`
	}
	if err := json.Unmarshal(body, &feed); err != nil || feed.Jobs == nil {
		return nil, fmt.Errorf("feed is not JSON we understand")
	}
	rows := make([]boardRow, 0, len(feed.Jobs))
	for _, j := range feed.Jobs {
		u := webURL(j.AbsoluteURL)
		if u == "" {
			u = webURL(j.JobURL)
		}
		title := strings.TrimSpace(j.Title) // boards ship stray tabs and spaces in titles
		if title == "" || u == "" {
			continue
		}
		loc := ""
		if len(j.Location) > 0 {
			if j.Location[0] == '"' {
				json.Unmarshal(j.Location, &loc)
			} else {
				var named struct {
					Name string `json:"name"`
				}
				json.Unmarshal(j.Location, &named)
				loc = named.Name
			}
		}
		posted := j.UpdatedAt
		if posted == "" {
			posted = j.PublishedAt
		}
		if len(posted) >= 10 {
			posted = posted[:10]
		}
		rows = append(rows, boardRow{Title: title, Location: strings.TrimSpace(loc), URL: u, Posted: posted})
	}
	return rows, nil
}

// webURL returns a trimmed http(s) URL, or "" for anything else — a feed is
// external content, and a javascript: or data: link must never reach an href.
func webURL(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}

// words lower-cases and splits a query; empty input means "no filter".
func words(s string) []string {
	return strings.Fields(strings.ToLower(strings.TrimSpace(s)))
}

// matchTitle: every query word must appear in the title.
func matchTitle(title string, q []string) bool {
	t := strings.ToLower(title)
	for _, w := range q {
		if !strings.Contains(t, w) {
			return false
		}
	}
	return true
}

// matchLocation: any location word may appear; no words means any location.
func matchLocation(location string, want []string) bool {
	if len(want) == 0 {
		return true
	}
	l := strings.ToLower(location)
	for _, w := range want {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

type board struct{ id, name, url string }

func (s *server) boards() ([]board, error) {
	rows, err := s.db.Query(`SELECT id, name, source_url FROM companies WHERE source_url != '' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []board
	for rows.Next() {
		var b board
		if err := rows.Scan(&b.id, &b.name, &b.url); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *server) handleOpenings(w http.ResponseWriter, r *http.Request) {
	q := words(r.URL.Query().Get("q"))
	loc := words(r.URL.Query().Get("loc"))
	refresh := r.URL.Query().Get("refresh") == "1"
	boards, err := s.boards()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	urls := make([]string, 0, len(boards))
	for _, b := range boards {
		urls = append(urls, b.url)
	}
	entries := s.feeds.fetchAll(r.Context(), urls, refresh)

	out := []opening{}
	failed := []failedBoard{}
	searched := 0
	var oldest time.Time
	for _, b := range boards {
		e := entries[b.url]
		if e == nil || e.err != "" {
			reason := "no result"
			if e != nil {
				reason = e.err
			}
			failed = append(failed, failedBoard{CompanyID: b.id, Company: b.name, Reason: reason})
			continue
		}
		searched++
		if oldest.IsZero() || e.at.Before(oldest) {
			oldest = e.at
		}
		for _, row := range e.rows {
			if matchTitle(row.Title, q) && matchLocation(row.Location, loc) {
				out = append(out, opening{CompanyID: b.id, Company: b.name, Title: row.Title, Location: row.Location, URL: row.URL, Posted: row.Posted})
			}
		}
	}
	// Newest first (boards without dates last), then company, then title —
	// so the cap keeps the freshest roles and one company cannot crowd the top.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Posted != out[j].Posted {
			return out[i].Posted > out[j].Posted
		}
		if out[i].Company != out[j].Company {
			return out[i].Company < out[j].Company
		}
		return out[i].Title < out[j].Title
	})
	truncated := false
	if len(out) > maxOpenings {
		out = out[:maxOpenings]
		truncated = true
	}
	cachedAt := ""
	if !oldest.IsZero() {
		cachedAt = oldest.UTC().Format(time.RFC3339)
	}
	writeJSON(w, 200, map[string]any{"openings": out, "boards": searched, "failed": failed, "cachedAt": cachedAt, "truncated": truncated})
}

// warm fetches every board once so the first search from the page is
// answered from the cache. Called from main() in a goroutine; never from
// newServer, so tests stay offline.
func (s *server) warm(db *sql.DB) {
	boards, err := s.boards()
	if err != nil {
		log.Printf("openings: could not list boards: %v", err)
		return
	}
	urls := make([]string, 0, len(boards))
	for _, b := range boards {
		urls = append(urls, b.url)
	}
	start := time.Now()
	entries := s.feeds.fetchAll(context.Background(), urls, false)
	failed := 0
	for _, e := range entries {
		if e == nil || e.err != "" {
			failed++
		}
	}
	log.Printf("openings: prefetched %d boards in %s (%d failed)", len(urls)-failed, time.Since(start).Round(time.Millisecond), failed)
}
