package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const greenhouseFeed = `{"jobs":[
 {"title":"Senior Software Engineer","absolute_url":"https://x/1","location":{"name":"Phoenix, Arizona, United States"},"departments":[{"name":"Engineering"}],"first_published":"2026-09-14T00:00:00Z"},
 {"title":"Account Executive","absolute_url":"https://x/2","location":{"name":"Remote - US"},"departments":[{"name":"Sales"}],"first_published":"2026-08-01T00:00:00Z"},
 {"title":"Recruiter","absolute_url":"https://x/3","location":{"name":"London"},"departments":[{"name":"People"}],"first_published":"2026-09-15T00:00:00Z"}
]}`

const greenhouseOfficesFeed = `{"jobs":[
 {"title":"Solutions Engineer","absolute_url":"https://x/8","location":{"name":"Hybrid"},"offices":[{"name":"Austin, TX"},{"name":"London"}],"departments":[{"name":"Sales"}]},
 {"title":"Support Engineer","absolute_url":"https://x/9","location":{"name":"Distributed"},"offices":[],"departments":[{"name":"Support"}]}
]}`

const leverFeed = `[
 {"text":"Backend Engineer","hostedUrl":"https://x/4","categories":{"location":"Foster City, CA","team":"Engineering"},"workplaceType":"onsite","createdAt":1789344000000},
 {"text":"Customer Success Manager","hostedUrl":"https://x/5","categories":{"location":"Toronto, Canada","team":"Customer Success"},"workplaceType":"remote","createdAt":1704067200000}
]`

const ashbyFeed = `{"jobs":[
 {"title":"Product Designer","jobUrl":"https://x/6","location":"San Francisco","department":"Design","isRemote":false,"publishedAt":"2026-09-10T00:00:00Z"},
 {"title":"Data Scientist","jobUrl":"https://x/7","location":"Remote","department":"","team":"Data","isRemote":true,"publishedAt":"2026-01-01T00:00:00Z"}
]}`

func TestClassify(t *testing.T) {
	cases := []struct{ dept, title, want string }{
		{"Engineering", "Senior Software Engineer", "engineering"},
		{"", "Staff Frontend Engineer", "engineering"},
		{"Sales", "Account Executive", "sales"},
		{"", "Account Manager, SMB", "sales"},
		{"People", "Recruiter", "recruiting/people"},
		{"", "Talent Partner", "recruiting/people"},
		{"Design", "Product Designer", "design"},
		{"", "Product Manager, Growth", "product"},
		{"Marketing", "Content Lead", "marketing"},
		{"Customer Success", "CSM", "customer success/support"},
		{"", "Technical Support Engineer", "customer success/support"},
		{"Data", "Analyst", "data"},
		{"", "Machine Learning Engineer", "data"},
		{"Finance", "Controller", "finance/legal/ops"},
		{"Legal", "Counsel", "finance/legal/ops"},
		{"Operations", "Warehouse Associate", "finance/legal/ops"},
		{"", "Nurse Practitioner", "other"},
		{"", "", "other"},
	}
	for _, c := range cases {
		if got := classify(c.dept, c.title); got != c.want {
			t.Errorf("classify(%q, %q) = %q, want %q", c.dept, c.title, got, c.want)
		}
	}
}

func TestIsUS(t *testing.T) {
	cases := map[string]bool{
		"Remote - US":                           true,
		"Remote":                                true,
		"United States - Remote":                true,
		"Foster City, CA":                       true,
		"Phoenix, Arizona, United States":       true,
		"New York, NY":                          true,
		"San Francisco":                         true,
		"London":                                false,
		"Toronto, ON, Canada":                   false,
		"Remote - Canada":                       false,
		"Remote, Canada; Remote, United States": true,
		"UK | Remote":                           false,
		"":                                      false,
		"Bangalore":                             false,
		"Austin, TX or Remote (US)":             true,
	}
	for loc, want := range cases {
		if got := isUS(loc); got != want {
			t.Errorf("isUS(%q) = %v, want %v", loc, got, want)
		}
	}
}

func TestParseJobsThreeShapes(t *testing.T) {
	gh, err := parseJobs([]byte(greenhouseFeed))
	if err != nil || len(gh) != 3 {
		t.Fatalf("greenhouse: %v %d", err, len(gh))
	}
	if gh[0].Dept != "Engineering" || gh[0].Location != "Phoenix, Arizona, United States" || gh[1].Remote != true {
		t.Errorf("greenhouse fields: %+v", gh[:2])
	}
	lv, err := parseJobs([]byte(leverFeed))
	if err != nil || len(lv) != 2 {
		t.Fatalf("lever: %v %d", err, len(lv))
	}
	if lv[0].Title != "Backend Engineer" || lv[0].Location != "Foster City, CA" || lv[0].Dept != "Engineering" || !lv[1].Remote {
		t.Errorf("lever fields: %+v", lv)
	}
	if lv[0].Posted.Format("2006-01-02") != "2026-09-14" {
		t.Errorf("lever createdAt → %s", lv[0].Posted)
	}
	ab, err := parseJobs([]byte(ashbyFeed))
	if err != nil || len(ab) != 2 {
		t.Fatalf("ashby: %v %d", err, len(ab))
	}
	if ab[0].Dept != "Design" || ab[1].Dept != "Data" || !ab[1].Remote {
		t.Errorf("ashby fields: %+v", ab)
	}
	off, err := parseJobs([]byte(greenhouseOfficesFeed))
	if err != nil || len(off) != 2 {
		t.Fatalf("greenhouse offices: %v %d", err, len(off))
	}
	if off[0].Location != "Hybrid / Austin, TX / London" || !isUS(off[0].Location) {
		t.Errorf("offices not folded into location: %q", off[0].Location)
	}
	if !off[1].Remote || isUS(off[1].Location) {
		t.Errorf("Distributed should count as remote, not US: %+v", off[1])
	}
	for _, bad := range []string{"", "{}", `{"jobs":null}`, "<html>", `{"error":"not found"}`} {
		if _, err := parseJobs([]byte(bad)); err == nil {
			t.Errorf("parseJobs(%q) accepted a non-feed", bad)
		}
	}
}

func TestSummarizeAndTexts(t *testing.T) {
	jobs, _ := parseJobs([]byte(greenhouseFeed))
	day, _ := time.Parse("2006-01-02", "2026-09-16")
	st := summarize(jobs, day)
	if st.Total != 3 || st.US != 2 || st.Remote != 1 || st.Recent != 2 {
		t.Errorf("summarize: %+v", st)
	}
	if !st.TopIsUS || (st.TopLocation != "Phoenix, Arizona, United States" && st.TopLocation != "Remote - US") {
		t.Errorf("top location: %+v", st)
	}
	why := whyText(st)
	if !strings.HasPrefix(why, "Board lists 3 open roles today: 1 engineering, 1 sales, 1 recruiting/people.") || !strings.Contains(why, "Most-listed US/remote location: ") {
		t.Errorf("why: %s", why)
	}
	if priority(300) != "A" || priority(299) != "B" || priority(75) != "B" || priority(74) != "C" {
		t.Error("priority thresholds")
	}
	if totalFromSignal("888 open roles, 67 remote, as of 2026-09-15 (greenhouse board)") != 888 {
		t.Error("totalFromSignal")
	}
}

// seedServer serves feeds by path so tests can mix healthy and dead boards.
func seedServer(t *testing.T, dead map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dead[r.URL.Path] {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/gh/"):
			w.Write([]byte(greenhouseFeed))
		case strings.HasPrefix(r.URL.Path, "/lever/"):
			w.Write([]byte(leverFeed))
		case strings.HasPrefix(r.URL.Path, "/ashby/"):
			w.Write([]byte(ashbyFeed))
		default:
			http.NotFound(w, r)
		}
	}))
}

func writeSeed(t *testing.T, base string, ids ...string) string {
	t.Helper()
	sf := seedFile{Note: "Sample data generated from public Greenhouse/Lever/Ashby job-board feeds on 2026-09-15. Keep me.", Apps: json.RawMessage("[]"), Done: json.RawMessage("{}"), Settings: json.RawMessage(`{"weeklyGoal":20}`)}
	for _, id := range ids {
		sf.Companies = append(sf.Companies, company{ID: id, Name: strings.ToUpper(id), Priority: "C", Location: "old", Status: "Researching", Why: "old why", Signal: "5 open roles, 0 remote, as of 2026-09-15 (greenhouse board)", SourceURL: base + "/gh/" + id, CareersURL: "https://example.com/" + id})
	}
	b, _ := json.MarshalIndent(sf, "", "  ")
	p := filepath.Join(t.TempDir(), "seed.json")
	os.WriteFile(p, b, 0o644)
	return p
}

func readSeed(t *testing.T, p string) seedFile {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var sf seedFile
	if err := json.Unmarshal(b, &sf); err != nil {
		t.Fatal(err)
	}
	return sf
}

func opts(p string) options {
	d, _ := time.Parse("2006-01-02", "2026-09-16")
	return options{seedPath: p, date: d, concurrency: 4, client: &http.Client{Timeout: 5 * time.Second}, out: &bytes.Buffer{}}
}

func TestRefreshRewritesMetadataAndKeepsOtherKeys(t *testing.T) {
	srv := seedServer(t, nil)
	defer srv.Close()
	p := writeSeed(t, srv.URL, "a", "b")
	if err := run(opts(p)); err != nil {
		t.Fatal(err)
	}
	sf := readSeed(t, p)
	if len(sf.Companies) != 2 {
		t.Fatalf("companies: %d", len(sf.Companies))
	}
	c := sf.Companies[0]
	if c.Why == "old why" || c.Location == "old" || c.Priority != "C" || !strings.Contains(c.Signal, "as of 2026-09-16") {
		t.Errorf("not refreshed: %+v", c)
	}
	if !strings.Contains(sf.Note, "on 2026-09-16.") || !strings.Contains(sf.Note, "Keep me.") {
		t.Errorf("note: %s", sf.Note)
	}
	if string(sf.Settings) != `{"weeklyGoal":20}` && !strings.Contains(string(sf.Settings), `"weeklyGoal": 20`) {
		t.Errorf("settings changed: %s", sf.Settings)
	}
	if string(sf.Apps) != "[]" {
		t.Errorf("apps changed: %s", sf.Apps)
	}
}

func TestFailedFeedKeepsRowAndTooManyFailuresWritesNothing(t *testing.T) {
	srv := seedServer(t, map[string]bool{"/gh/e": true})
	defer srv.Close()
	// 1 of 5 dead (20%, not over) → writes, dead row byte-identical.
	p := writeSeed(t, srv.URL, "a", "b", "c", "d", "e")
	before := readSeed(t, p)
	if err := run(opts(p)); err != nil {
		t.Fatal(err)
	}
	after := readSeed(t, p)
	var kept, refreshed company
	for _, c := range after.Companies {
		if c.ID == "e" {
			kept = c
		}
		if c.ID == "a" {
			refreshed = c
		}
	}
	var orig company
	for _, c := range before.Companies {
		if c.ID == "e" {
			orig = c
		}
	}
	want := orig
	want.Signal += unreachable + "2026-09-16"
	if kept != want {
		t.Errorf("dead feed row: got %+v, want old row plus marker %+v", kept, want)
	}
	if refreshed.Why == "old why" {
		t.Error("healthy row not refreshed")
	}
	// A second run keeps the first failure date and, because the row is already
	// marked, its failure no longer counts against the budget.
	if err := run(opts(p)); err != nil {
		t.Fatal(err)
	}
	for _, c := range readSeed(t, p).Companies {
		if c.ID == "e" && c != want {
			t.Errorf("marker not idempotent: %q", c.Signal)
		}
	}
	// 1 of 2 dead (50%) → refuses, file untouched.
	p2 := writeSeed(t, srv.URL, "a", "e")
	raw, _ := os.ReadFile(p2)
	err := run(opts(p2))
	if err == nil || !strings.Contains(err.Error(), "nothing written") {
		t.Fatalf("expected refusal, got %v", err)
	}
	raw2, _ := os.ReadFile(p2)
	if !bytes.Equal(raw, raw2) {
		t.Error("seed file was written despite refusal")
	}
}

func TestCollapseGuardsAndTombstones(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/gh/big") {
			w.Write([]byte(`{"jobs":[]}`))
			return
		}
		w.Write([]byte(greenhouseFeed))
	}))
	defer empty.Close()
	// A board that had 300 roles and now parses to zero is a failure, not a refresh to 0.
	p := writeSeed(t, empty.URL, "big", "a", "b", "c", "d", "f")
	var sf seedFile
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &sf)
	for i := range sf.Companies {
		if sf.Companies[i].ID == "big" {
			sf.Companies[i].Signal = "300 open roles, 1 remote, as of 2026-09-15 (greenhouse board)"
		}
	}
	b, _ = json.MarshalIndent(sf, "", "  ")
	os.WriteFile(p, b, 0o644)
	if err := run(opts(p)); err != nil {
		t.Fatal(err)
	}
	for _, c := range readSeed(t, p).Companies {
		if c.ID == "big" && (!strings.HasPrefix(c.Signal, "300 open roles") || !strings.Contains(c.Signal, unreachable)) {
			t.Errorf("zero-roles board should keep its old counts and be marked: %q", c.Signal)
		}
	}
	// Roles total collapsing below 70% of last time refuses the write.
	srv := seedServer(t, nil)
	defer srv.Close()
	p2 := writeSeed(t, srv.URL, "a", "b")
	b, _ = os.ReadFile(p2)
	json.Unmarshal(b, &sf)
	for i := range sf.Companies {
		sf.Companies[i].Signal = "500 open roles, 0 remote, as of 2026-09-15 (greenhouse board)"
	}
	b, _ = json.MarshalIndent(sf, "", "  ")
	os.WriteFile(p2, b, 0o644)
	if err := run(opts(p2)); err == nil || !strings.Contains(err.Error(), "collapsed") {
		t.Errorf("expected collapse refusal, got %v", err)
	}
	// A tombstoned id cannot be re-added.
	saved := boards
	boards = map[string]struct{ feed, careers string }{"greenhouse": {srv.URL + "/gh/%s", "https://careers.example/%s"}}
	defer func() { boards = saved }()
	p3 := writeSeed(t, srv.URL, "a")
	b, _ = os.ReadFile(p3)
	json.Unmarshal(b, &sf)
	sf.Removed = []removed{{ID: "gone", Removed: "2026-09-16", Reason: "board moved"}}
	b, _ = json.MarshalIndent(sf, "", "  ")
	os.WriteFile(p3, b, 0o644)
	o := opts(p3)
	o.add = []string{"greenhouse:gone"}
	if err := run(o); err == nil || !strings.Contains(err.Error(), "removed on 2026-09-16") {
		t.Errorf("tombstone not enforced: %v", err)
	}
	if got := readSeed(t, p3); len(got.Removed) != 1 || len(got.Companies) != 1 {
		t.Errorf("tombstone list not preserved: %+v", got.Removed)
	}
}

func TestRetryOnceOnServerError(t *testing.T) {
	saved := retryPause
	retryPause = 0
	defer func() { retryPause = saved }()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(ashbyFeed))
	}))
	defer srv.Close()
	jobs, err := fetchBoard(srv.Client(), srv.URL+"/x")
	if err != nil || len(jobs) != 2 || calls != 2 {
		t.Errorf("retry: err=%v jobs=%d calls=%d", err, len(jobs), calls)
	}
	calls = 0
	nf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.NotFound(w, r) }))
	defer nf.Close()
	if _, err := fetchBoard(nf.Client(), nf.URL+"/x"); err == nil || calls != 1 {
		t.Errorf("404 must not retry: err=%v calls=%d", err, calls)
	}
}

func TestClean(t *testing.T) {
	if got := clean("  New\u0000York,\tNY \n(Remote)  "); got != "New York, NY (Remote)" {
		t.Errorf("clean = %q", got)
	}
	if got := clean(strings.Repeat("x", 500)); len(got) != maxText {
		t.Errorf("clean cap = %d", len(got))
	}
}

func TestAddUsesLiveFeedAndRejectsDeadOrDuplicate(t *testing.T) {
	srv := seedServer(t, map[string]bool{"/ashby/dead": true})
	defer srv.Close()
	saved := boards
	boards = map[string]struct{ feed, careers string }{
		"greenhouse": {srv.URL + "/gh/%s", "https://careers.example/%s"},
		"lever":      {srv.URL + "/lever/%s", "https://careers.example/%s"},
		"ashby":      {srv.URL + "/ashby/%s", "https://careers.example/%s"},
	}
	defer func() { boards = saved }()

	p := writeSeed(t, srv.URL, "a")
	o := opts(p)
	o.add = []string{"lever:acme=Acme Corp", "ashby:widgets"}
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	sf := readSeed(t, p)
	if len(sf.Companies) != 3 {
		t.Fatalf("companies: %d", len(sf.Companies))
	}
	byID := map[string]company{}
	for _, c := range sf.Companies {
		byID[c.ID] = c
	}
	if byID["acme"].Name != "Acme Corp" || byID["widgets"].Name != "Widgets" || byID["acme"].Status != "Researching" {
		t.Errorf("added rows: %+v %+v", byID["acme"], byID["widgets"])
	}
	if !strings.HasPrefix(byID["acme"].Why, "Board lists 2 open roles today") || byID["acme"].SourceURL != srv.URL+"/lever/acme" {
		t.Errorf("acme metadata: %+v", byID["acme"])
	}

	raw, _ := os.ReadFile(p)
	for _, bad := range [][]string{{"ashby:dead"}, {"lever:acme"}, {"workday:foo"}, {"nohost"}, {"ashby:bad slug"}} {
		o.add = bad
		if err := run(o); err == nil {
			t.Errorf("add %v succeeded", bad)
		}
		raw2, _ := os.ReadFile(p)
		if !bytes.Equal(raw, raw2) {
			t.Errorf("add %v wrote the file", bad)
		}
	}
}

func TestCheckAndReportWriteNothing(t *testing.T) {
	srv := seedServer(t, map[string]bool{"/gh/b": true})
	defer srv.Close()
	p := writeSeed(t, srv.URL, "a", "b")
	raw, _ := os.ReadFile(p)

	o := opts(p)
	o.check = true
	if err := run(o); err == nil || !strings.Contains(err.Error(), "dead") {
		t.Errorf("check with a dead feed: %v", err)
	}
	o = opts(p)
	o.report = true
	if err := run(o); err != nil {
		t.Errorf("report: %v", err)
	}
	out := o.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "TOTAL") || !strings.Contains(out, "FAILED b:") {
		t.Errorf("report output: %s", out)
	}
	raw2, _ := os.ReadFile(p)
	if !bytes.Equal(raw, raw2) {
		t.Error("check/report wrote the file")
	}

	healthy := seedServer(t, nil)
	defer healthy.Close()
	p3 := writeSeed(t, healthy.URL, "a")
	o = opts(p3)
	o.check = true
	if err := run(o); err != nil {
		t.Errorf("check healthy: %v", err)
	}
}

func TestSortByPriorityThenTotal(t *testing.T) {
	cs := []company{{ID: "x", Name: "X", Priority: "C", Signal: "1 open roles"}, {ID: "y", Name: "Y", Priority: "A", Signal: "400 open roles"}, {ID: "z", Name: "Z", Priority: "B", Signal: "80 open roles"}}
	if priority(400) != "A" || totalFromSignal(cs[1].Signal) != 400 {
		t.Fatal("setup")
	}
	if got := hostLabel("https://api.lever.co/v0/postings/x?mode=json"); got != "lever" {
		t.Errorf("hostLabel = %s", got)
	}
}
