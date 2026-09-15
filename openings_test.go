package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"
)

// boardStub serves the three feed shapes and counts requests. The server's
// allow-list is pointed at it, so the seeded (real) board URLs are refused
// and no test ever reaches the network.
type boardStub struct {
	srv   *httptest.Server
	hits  atomic.Int64
	fail  atomic.Bool
	extra int // how many rows /gh returns on top of the fixed ones
}

func newBoardStub(t *testing.T) *boardStub {
	t.Helper()
	b := &boardStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/gh", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		if b.fail.Load() {
			w.WriteHeader(500)
			return
		}
		jobs := []map[string]any{
			{"title": "\t Senior Backend Engineer ", "location": map[string]string{"name": " Remote - US"}, "absolute_url": "https://gh.example/1", "updated_at": "2026-09-10T12:00:00-04:00"}, // boards ship stray whitespace; it must be trimmed
			{"title": "Account Executive", "location": map[string]string{"name": "Phoenix, AZ"}, "absolute_url": "https://gh.example/2", "updated_at": "2026-09-11T12:00:00-04:00"},
		}
		for i := 0; i < b.extra; i++ {
			jobs = append(jobs, map[string]any{"title": fmt.Sprintf("Engineer %03d", i), "location": map[string]string{"name": "Remote"}, "absolute_url": fmt.Sprintf("https://gh.example/x%d", i)})
		}
		json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
	})
	mux.HandleFunc("/lever", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		json.NewEncoder(w).Encode([]map[string]any{
			{"text": "Customer Success Manager", "hostedUrl": "https://lever.example/1", "createdAt": 1789430400000, "categories": map[string]string{"location": "Remote"}},
			{"text": "Staff Engineer, Platform", "hostedUrl": "https://lever.example/2", "createdAt": 0, "categories": map[string]string{"location": "New York"}},
		})
	})
	mux.HandleFunc("/ashby", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
			{"title": "Product Designer", "location": "Phoenix", "jobUrl": "https://ashby.example/1", "publishedAt": "2026-09-01T00:00:00Z"},
		}})
	})
	mux.HandleFunc("/bad", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		w.WriteHeader(503)
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		w.Write([]byte("<html>not json</html>"))
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// openingsServer builds a server whose feed client trusts only the stub,
// with four stub-backed companies added next to the seeded ones.
func openingsServer(t *testing.T) (*server, http.Handler, *boardStub) {
	t.Helper()
	stub := newBoardStub(t)
	s, h := newTestServer(t)
	stubHost := strings.TrimPrefix(stub.srv.URL, "http://")
	s.feeds.client = stub.srv.Client()
	s.feeds.allow = func(u *url.URL) bool { return u.Host == stubHost }
	for _, c := range []Company{
		{ID: "ghco", Name: "Alpha (GH)", SourceURL: stub.srv.URL + "/gh"},
		{ID: "leverco", Name: "Beta (Lever)", SourceURL: stub.srv.URL + "/lever"},
		{ID: "ashbyco", Name: "Gamma (Ashby)", SourceURL: stub.srv.URL + "/ashby"},
		{ID: "badco", Name: "Delta (down)", SourceURL: stub.srv.URL + "/bad"},
	} {
		if rec := do(t, h, "PUT", "/api/companies/"+c.ID, c); rec.Code != 200 {
			t.Fatalf("add %s: %d %s", c.ID, rec.Code, rec.Body)
		}
	}
	return s, h, stub
}

type openingsResp struct {
	Openings  []opening     `json:"openings"`
	Boards    int           `json:"boards"`
	Failed    []failedBoard `json:"failed"`
	CachedAt  string        `json:"cachedAt"`
	Truncated bool          `json:"truncated"`
}

func search(t *testing.T, h http.Handler, query string) openingsResp {
	t.Helper()
	rec := do(t, h, "GET", "/api/openings?"+query, nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/openings?%s: %d %s", query, rec.Code, rec.Body)
	}
	var r openingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func titles(rows []opening) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Title
	}
	return out
}

func TestOpeningsParsesAllThreeShapesAndReportsFailures(t *testing.T) {
	s, h, _ := openingsServer(t)
	before := count(t, s, `SELECT COUNT(*) FROM companies`) + count(t, s, `SELECT COUNT(*) FROM applications`)
	r := search(t, h, "")
	if r.Boards != 3 {
		t.Fatalf("boards searched = %d, want 3 (the stub-backed ones)", r.Boards)
	}
	want := []string{"Customer Success Manager", "Account Executive", "Senior Backend Engineer", "Product Designer", "Staff Engineer, Platform"} // newest first, undated last
	if got := titles(r.Openings); !reflect.DeepEqual(got, want) {
		t.Fatalf("openings = %v, want %v", got, want)
	}
	byURL := map[string]opening{}
	for _, o := range r.Openings {
		byURL[o.URL] = o
	}
	if o := byURL["https://gh.example/1"]; o.Company != "Alpha (GH)" || o.CompanyID != "ghco" || o.Location != "Remote - US" || o.Posted != "2026-09-10" {
		t.Fatalf("greenhouse row = %+v", o)
	}
	if o := byURL["https://lever.example/1"]; o.Location != "Remote" || o.Posted != "2026-09-15" {
		t.Fatalf("lever row = %+v", o)
	}
	if o := byURL["https://lever.example/2"]; o.Posted != "" {
		t.Fatalf("lever row without createdAt should have no date: %+v", o)
	}
	if o := byURL["https://ashby.example/1"]; o.Location != "Phoenix" || o.Posted != "2026-09-01" {
		t.Fatalf("ashby row = %+v", o)
	}
	// The down board and every seeded (real, disallowed) board are reported, not fatal.
	reasons := map[string]string{}
	for _, f := range r.Failed {
		reasons[f.CompanyID] = f.Reason
	}
	if reasons["badco"] != "HTTP 503" {
		t.Fatalf("down board reason = %q", reasons["badco"])
	}
	seeded := len(getState(t, h).Companies) - 4
	notKnown := 0
	for _, f := range r.Failed {
		if f.Reason == "not a known board" {
			notKnown++
		}
	}
	if notKnown != seeded {
		t.Fatalf("%d seeded boards refused, want %d", notKnown, seeded)
	}
	if r.CachedAt == "" {
		t.Fatal("cachedAt missing")
	}
	// Anti: the endpoint writes nothing.
	if after := count(t, s, `SELECT COUNT(*) FROM companies`) + count(t, s, `SELECT COUNT(*) FROM applications`); after != before {
		t.Fatalf("search changed row counts %d → %d", before, after)
	}
}

func TestOpeningsFiltersByTitleWordsAndLocation(t *testing.T) {
	_, h, _ := openingsServer(t)
	cases := []struct {
		query string
		want  []string
	}{
		{"q=engineer", []string{"Senior Backend Engineer", "Staff Engineer, Platform"}},
		{"q=ENGINEER+backend", []string{"Senior Backend Engineer"}},                                         // every word must match, any case
		{"q=engineer&loc=remote", []string{"Senior Backend Engineer"}},                                      // "Remote - US" contains remote
		{"loc=phoenix+york", []string{"Account Executive", "Product Designer", "Staff Engineer, Platform"}}, // any location word; newest first
		{"q=customer+success", []string{"Customer Success Manager"}},
		{"q=nothing+like+this", []string{}},
	}
	for _, c := range cases {
		got := titles(search(t, h, c.query).Openings)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.query, got, c.want)
		}
	}
}

func TestOpeningsCacheAndRefresh(t *testing.T) {
	_, h, stub := openingsServer(t)
	search(t, h, "q=engineer")
	first := stub.hits.Load()
	if first != 4 {
		t.Fatalf("first search made %d requests, want 4", first)
	}
	search(t, h, "q=designer")
	if n := stub.hits.Load(); n != first {
		t.Fatalf("second search within the hour made %d more requests", n-first)
	}
	search(t, h, "q=designer&refresh=1")
	if n := stub.hits.Load(); n != first+4 {
		t.Fatalf("refresh made %d requests, want 4", n-first)
	}
	// A failed board is retried after failTTL, a good one is not.
	stub.fail.Store(true)
	search(t, h, "refresh=1")
	base := stub.hits.Load()
	search(t, h, "")
	if n := stub.hits.Load(); n != base {
		t.Fatalf("a fresh failure was retried immediately (%d extra requests)", n-base)
	}
}

func TestOpeningsRefusesUnknownHostsAndNonJSON(t *testing.T) {
	s, h, stub := openingsServer(t)
	do(t, h, "PUT", "/api/companies/local", Company{Name: "Local service", SourceURL: "http://127.0.0.1:1/secret"})
	do(t, h, "PUT", "/api/companies/htmlco", Company{Name: "HTML board", SourceURL: stub.srv.URL + "/html"})
	r := search(t, h, "")
	reasons := map[string]string{}
	for _, f := range r.Failed {
		reasons[f.CompanyID] = f.Reason
	}
	if reasons["local"] != "not a known board" || reasons["htmlco"] != "feed is not JSON we understand" {
		t.Fatalf("reasons = %v", reasons)
	}
	// The default allow-list, used outside tests, is https on the three ATS hosts only.
	for raw, ok := range map[string]bool{
		"https://boards-api.greenhouse.io/v1/boards/x/jobs": true,
		"https://api.lever.co/v0/postings/x?mode=json":      true,
		"https://api.ashbyhq.com/posting-api/job-board/x":   true,
		"http://boards-api.greenhouse.io/v1/boards/x/jobs":  false,
		"https://boards-api.greenhouse.io.evil.example/":    false,
		"https://127.0.0.1:8787/api/state":                  false,
	} {
		u, _ := url.Parse(raw)
		if knownBoard(u) != ok {
			t.Errorf("knownBoard(%s) = %v, want %v", raw, !ok, ok)
		}
	}
	_ = s
}

func TestOpeningsCapsAt500(t *testing.T) {
	_, h, stub := openingsServer(t)
	stub.extra = 600
	r := search(t, h, "q=engineer&refresh=1")
	if len(r.Openings) != maxOpenings || !r.Truncated {
		t.Fatalf("got %d rows, truncated=%v", len(r.Openings), r.Truncated)
	}
}

func TestOpeningsTimeoutIsBounded(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"jobs":[]}`))
	}))
	defer slow.Close()
	c := newFeedCache(slow.Client())
	c.allow = func(*url.URL) bool { return true }
	c.client.Timeout = 50 * time.Millisecond
	e := c.fetchOne(t.Context(), slow.URL)
	if e.err == "" {
		t.Fatal("a slow board should fail, not hang")
	}
}

// ---------- property ----------

const titleAlphabet = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ,-/()"

type titleAndQuery struct {
	title string
	q     []string // built from substrings of title, so it must match
}

func (titleAndQuery) Generate(r *rand.Rand, _ int) reflect.Value {
	runes := []rune(titleAlphabet)
	n := 1 + r.Intn(40)
	t := make([]rune, n)
	for i := range t {
		t[i] = runes[r.Intn(len(runes))]
	}
	title := string(t)
	var q []string
	for i := r.Intn(4); i > 0; i-- {
		a := r.Intn(n)
		b := a + 1 + r.Intn(n-a)
		w := strings.ToLower(strings.TrimSpace(string(t[a:b])))
		if w != "" && !strings.Contains(w, " ") {
			q = append(q, w)
		}
	}
	return reflect.ValueOf(titleAndQuery{title: title, q: q})
}

func TestMatchTitleProperty(t *testing.T) {
	prop := func(tq titleAndQuery) bool {
		if !matchTitle(tq.title, tq.q) {
			t.Logf("substring query %v did not match %q", tq.q, tq.title)
			return false
		}
		// A word that cannot be in the title (outside its alphabet) never matches.
		return !matchTitle(tq.title, append(append([]string{}, tq.q...), "zzz9"))
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
}
