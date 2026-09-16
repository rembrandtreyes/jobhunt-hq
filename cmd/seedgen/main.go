// Command seedgen regenerates seed.json from the companies' live job boards.
//
//	go run ./cmd/seedgen                              refresh every company's why, signal, priority, location
//	go run ./cmd/seedgen -add ashby:crusoe=Crusoe     add a company from its board (host:slug or host:slug=Name)
//	go run ./cmd/seedgen -check                       exit 1 if any feed is dead; writes nothing
//	go run ./cmd/seedgen -report                      print today's counts; writes nothing
//
// Standard library only. It never removes a company: a feed that fails leaves
// that row exactly as it was, and if more than 20% of feeds fail nothing is
// written. Hosts are limited to the three boards the app itself will fetch.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// company mirrors the Company struct in the app; the JSON field names must match.
type company struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Priority   string `json:"priority"`
	Location   string `json:"location"`
	CareersURL string `json:"careersUrl"`
	Status     string `json:"status"`
	Why        string `json:"why"`
	Notes      string `json:"notes"`
	Signal     string `json:"signal"`
	SourceURL  string `json:"sourceUrl"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// removed is a tombstone: a company taken out of the seed on purpose, so -add
// and future probes do not quietly put it back.
type removed struct {
	ID      string `json:"id"`
	Removed string `json:"removed"`
	Reason  string `json:"reason"`
}

// seedFile keeps every top-level key; only _note and companies are regenerated.
type seedFile struct {
	Note      string          `json:"_note"`
	Removed   []removed       `json:"_removed,omitempty"`
	Companies []company       `json:"companies"`
	Apps      json.RawMessage `json:"apps"`
	Done      json.RawMessage `json:"done"`
	Settings  json.RawMessage `json:"settings"`
}

type job struct {
	Title    string
	Location string
	Dept     string
	Remote   bool
	Posted   time.Time
}

type stats struct {
	Total, Remote, US, Recent int
	Functions                 map[string]int
	TopLocation               string // most-listed US/remote location, else most-listed overall
	TopIsUS                   bool
}

// boards maps the host keyword of an -add spec to the feed and careers URL shapes.
// Tests override this to point at a local server.
var boards = map[string]struct{ feed, careers string }{
	"greenhouse": {"https://boards-api.greenhouse.io/v1/boards/%s/jobs", "https://job-boards.greenhouse.io/%s"},
	"lever":      {"https://api.lever.co/v0/postings/%s?mode=json", "https://jobs.lever.co/%s"},
	"ashby":      {"https://api.ashbyhq.com/posting-api/job-board/%s", "https://jobs.ashbyhq.com/%s"},
}

var (
	idRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	noteRe  = regexp.MustCompile(`on \d{4}-\d{2}-\d{2}\.`)
	totalRe = regexp.MustCompile(`^(\d+) open roles`)
)

const (
	maxBody      = 16 << 20
	maxFailShare = 0.20 // new feed failures allowed per run, as a share of feeds that were healthy before it
	minKeepShare = 0.70 // refuse to write when the roles total collapses below this share of last time
	zeroGuardMin = 20   // a board that had at least this many roles and now parses to zero is treated as a failure
	maxText      = 160  // runes kept from a feed-supplied location string
	unreachable  = "; feed unreachable since "
)

type options struct {
	seedPath    string
	add         []string
	check       bool
	report      bool
	date        time.Time
	concurrency int
	client      *http.Client
	out         io.Writer
}

func main() {
	var o options
	var add, date string
	var timeout time.Duration
	flag.StringVar(&o.seedPath, "seed", "seed.json", "path to seed.json")
	flag.StringVar(&add, "add", "", "comma-separated host:slug or host:slug=Name to add (greenhouse, lever, ashby)")
	flag.BoolVar(&o.check, "check", false, "only check that every feed answers; exit 1 if one is dead")
	flag.BoolVar(&o.report, "report", false, "only print today's counts")
	flag.StringVar(&date, "date", "", "date to stamp (YYYY-MM-DD, default today UTC)")
	flag.IntVar(&o.concurrency, "concurrency", 8, "feeds fetched at once")
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "per-feed timeout")
	flag.Parse()
	o.date = time.Now().UTC()
	if date != "" {
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			fmt.Fprintln(os.Stderr, "seedgen: -date must be YYYY-MM-DD")
			os.Exit(2)
		}
		o.date = d
	}
	if add != "" {
		o.add = strings.Split(add, ",")
	}
	o.client = &http.Client{Timeout: timeout}
	o.out = os.Stdout
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "seedgen:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	raw, err := os.ReadFile(o.seedPath)
	if err != nil {
		return err
	}
	var sf seedFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return fmt.Errorf("%s: %w", o.seedPath, err)
	}
	seen := map[string]bool{}
	sources := map[string]bool{}
	for _, c := range sf.Companies {
		seen[c.ID] = true
		sources[c.SourceURL] = true
	}
	tomb := map[string]removed{}
	for _, r := range sf.Removed {
		tomb[r.ID] = r
	}
	var added []string
	for _, spec := range o.add {
		c, err := parseAdd(spec)
		if err != nil {
			return err
		}
		if r, ok := tomb[c.ID]; ok {
			return fmt.Errorf("add %s: %q was removed on %s (%s); delete its _removed entry to re-add it", spec, c.ID, r.Removed, r.Reason)
		}
		if seen[c.ID] {
			return fmt.Errorf("add %s: id %q already in the seed", spec, c.ID)
		}
		if sources[c.SourceURL] {
			return fmt.Errorf("add %s: feed %s already in the seed", spec, c.SourceURL)
		}
		seen[c.ID] = true
		sources[c.SourceURL] = true
		sf.Companies = append(sf.Companies, c)
		added = append(added, c.ID)
	}
	results := fetchAll(o.client, sf.Companies, o.concurrency)
	day := o.date.Format("2006-01-02")

	var dead, stale []string
	newFailures, healthyBefore := 0, 0
	prevSum, newSum := 0, 0 // roles last time vs now, over companies that answered both times
	all := stats{Functions: map[string]int{}}
	byID := map[string]stats{}
	for _, c := range sf.Companies {
		r := results[c.ID]
		prev := totalFromSignal(c.Signal)
		wasHealthy := !strings.Contains(c.Signal, unreachable)
		if wasHealthy {
			healthyBefore++
		}
		if r.err == nil && len(r.jobs) == 0 && prev >= zeroGuardMin {
			r.err = fmt.Errorf("feed parsed to zero roles (had %d); treating as a failure", prev)
		}
		if r.err != nil {
			dead = append(dead, fmt.Sprintf("%s: %v", c.ID, r.err))
			if wasHealthy {
				newFailures++
			} else {
				stale = append(stale, c.ID+c.Signal[strings.Index(c.Signal, unreachable)+1:])
			}
			continue
		}
		st := summarize(r.jobs, o.date)
		if prev > 0 && wasHealthy {
			prevSum += prev
			newSum += st.Total
		}
		byID[c.ID] = st
		all.Total += st.Total
		all.Remote += st.Remote
		all.US += st.US
		all.Recent += st.Recent
		for k, v := range st.Functions {
			all.Functions[k] += v
		}
	}
	printReport(o.out, sf.Companies, byID, all, dead, stale, day)

	if o.check {
		if len(dead) > 0 {
			return fmt.Errorf("%d of %d feeds are dead", len(dead), len(sf.Companies))
		}
		return nil
	}
	if o.report {
		return nil
	}
	for _, id := range added {
		if results[id].err != nil {
			return fmt.Errorf("add %s: feed did not answer (%v); nothing written", id, results[id].err)
		}
		if byID[id].Total == 0 {
			return fmt.Errorf("add %s: feed lists no jobs; nothing written", id)
		}
	}
	if healthyBefore > 0 && float64(newFailures) > maxFailShare*float64(healthyBefore) {
		return fmt.Errorf("%d of %d previously healthy feeds failed (over %.0f%%); nothing written", newFailures, healthyBefore, maxFailShare*100)
	}
	if prevSum >= 100 && float64(newSum) < minKeepShare*float64(prevSum) {
		return fmt.Errorf("open roles collapsed from %d to %d across the feeds that answered (below %.0f%% of last time); nothing written", prevSum, newSum, minKeepShare*100)
	}

	totals := map[string]int{}
	for i := range sf.Companies {
		c := &sf.Companies[i]
		st, ok := byID[c.ID]
		if !ok {
			totals[c.ID] = totalFromSignal(c.Signal)
			if !strings.Contains(c.Signal, unreachable) {
				c.Signal += unreachable + day
			}
			continue
		}
		totals[c.ID] = st.Total
		c.Priority = priority(st.Total)
		c.Location = clean(st.TopLocation)
		c.Why = whyText(st)
		c.Signal = fmt.Sprintf("%d open roles, %d remote, as of %s (%s board)", st.Total, st.Remote, day, hostLabel(c.SourceURL))
		if c.Status == "" {
			c.Status = "Researching"
		}
	}
	rank := map[string]int{"A": 0, "B": 1, "C": 2}
	sort.SliceStable(sf.Companies, func(i, j int) bool {
		a, b := sf.Companies[i], sf.Companies[j]
		if rank[a.Priority] != rank[b.Priority] {
			return rank[a.Priority] < rank[b.Priority]
		}
		if totals[a.ID] != totals[b.ID] {
			return totals[a.ID] > totals[b.ID]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	if noteRe.MatchString(sf.Note) {
		sf.Note = noteRe.ReplaceAllString(sf.Note, "on "+day+".")
	} else if sf.Note == "" {
		sf.Note = "Sample data generated from public Greenhouse/Lever/Ashby job-board feeds on " + day + ". It is a starting set of employers hiring across every function, not anyone's application list. Counts go stale; refresh them with the find-jobs skill or edit freely."
	}
	if sf.Apps == nil {
		sf.Apps = json.RawMessage("[]")
	}
	if sf.Done == nil {
		sf.Done = json.RawMessage("{}")
	}
	if sf.Settings == nil {
		sf.Settings = json.RawMessage("{}")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sf); err != nil {
		return err
	}
	if err := os.WriteFile(o.seedPath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(o.out, "wrote %s: %d companies (%d added, %d refreshed, %d kept with their old counts because their feed failed)\n",
		o.seedPath, len(sf.Companies), len(added), len(byID), len(dead))
	return nil
}

// parseAdd turns "host:slug" or "host:slug=Display Name" into a company row.
func parseAdd(spec string) (company, error) {
	spec = strings.TrimSpace(spec)
	name := ""
	if i := strings.Index(spec, "="); i >= 0 {
		name = strings.TrimSpace(spec[i+1:])
		spec = spec[:i]
	}
	host, slug, ok := strings.Cut(spec, ":")
	host = strings.ToLower(strings.TrimSpace(host))
	slug = strings.TrimSpace(slug)
	b, known := boards[host]
	if !ok || !known {
		return company{}, fmt.Errorf("add %q: want host:slug with host one of greenhouse, lever, ashby", spec)
	}
	if !idRe.MatchString(slug) {
		return company{}, fmt.Errorf("add %q: slug must match %s", spec, idRe)
	}
	if name == "" {
		name = strings.ToUpper(slug[:1]) + slug[1:]
	}
	return company{
		ID:         strings.ToLower(slug),
		Name:       name,
		Status:     "Researching",
		CareersURL: fmt.Sprintf(b.careers, url.PathEscape(slug)),
		SourceURL:  fmt.Sprintf(b.feed, url.PathEscape(slug)),
	}, nil
}

type result struct {
	jobs []job
	err  error
}

func fetchAll(client *http.Client, cs []company, concurrency int) map[string]result {
	if concurrency < 1 {
		concurrency = 1
	}
	out := make(map[string]result, len(cs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for _, c := range cs {
		wg.Add(1)
		go func(c company) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			jobs, err := fetchBoard(client, c.SourceURL)
			mu.Lock()
			out[c.ID] = result{jobs, err}
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return out
}

// fetchBoard fetches a feed, retrying once after a pause on a timeout, a 429, or a 5xx.
func fetchBoard(client *http.Client, u string) ([]job, error) {
	jobs, err := fetchOnce(client, u)
	if err != nil && retryable(err) {
		time.Sleep(retryPause)
		jobs, err = fetchOnce(client, u)
	}
	return jobs, err
}

var retryPause = 2 * time.Second

type httpError int

func (e httpError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func retryable(err error) bool {
	var he httpError
	if errors.As(err, &he) {
		return he == 429 || he >= 500
	}
	return !strings.Contains(err.Error(), "not a") && !strings.Contains(err.Error(), "no jobs array") // network errors and timeouts, not parse errors
}

func fetchOnce(client *http.Client, u string) ([]job, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "jobhunt-hq/seedgen")
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, httpError(res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, err
	}
	return parseJobs(body)
}

// anyJob holds the union of the three feed shapes; encoding/json ignores what a feed lacks.
type anyJob struct {
	Title       string          `json:"title"`
	Text        string          `json:"text"`
	Location    json.RawMessage `json:"location"`
	Departments []struct {
		Name string `json:"name"`
	} `json:"departments"`
	Department string `json:"department"`
	Team       string `json:"team"`
	Categories struct {
		Location     string   `json:"location"`
		Team         string   `json:"team"`
		Department   string   `json:"department"`
		AllLocations []string `json:"allLocations"`
	} `json:"categories"`
	WorkplaceType  string `json:"workplaceType"`
	IsRemote       bool   `json:"isRemote"`
	CreatedAt      int64  `json:"createdAt"`
	FirstPublished string `json:"first_published"`
	UpdatedAt      string `json:"updated_at"`
	PublishedAt    string `json:"publishedAt"`
	Offices        []struct {
		Name string `json:"name"`
	} `json:"offices"`
	AbsoluteURL string `json:"absolute_url"`
	JobURL      string `json:"jobUrl"`
	HostedURL   string `json:"hostedUrl"`
}

func parseJobs(body []byte) ([]job, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("empty body")
	}
	var raws []anyJob
	if body[0] == '[' {
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, fmt.Errorf("not a Lever feed: %w", err)
		}
	} else {
		var obj struct {
			Jobs []anyJob `json:"jobs"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			return nil, fmt.Errorf("not a board feed: %w", err)
		}
		if obj.Jobs == nil {
			return nil, errors.New("no jobs array in feed")
		}
		raws = obj.Jobs
	}
	jobs := make([]job, 0, len(raws))
	for _, r := range raws {
		j := job{Title: strings.TrimSpace(firstNonEmpty(r.Title, r.Text))}
		loc := ""
		if len(r.Location) > 0 {
			switch r.Location[0] {
			case '"':
				json.Unmarshal(r.Location, &loc)
			case '{':
				var o struct {
					Name string `json:"name"`
				}
				json.Unmarshal(r.Location, &o)
				loc = o.Name
			}
		}
		j.Location = clean(firstNonEmpty(loc, r.Categories.Location, strings.Join(r.Categories.AllLocations, " / ")))
		// Greenhouse boards sometimes put the work mode in location ("Hybrid",
		// "Distributed") and the real places in offices; keep both.
		for _, o := range r.Offices {
			if name := clean(o.Name); name != "" && !strings.Contains(j.Location, name) {
				j.Location = strings.TrimSpace(j.Location + " / " + name)
			}
		}
		dept := r.Department
		if len(r.Departments) > 0 {
			dept = r.Departments[0].Name
		}
		j.Dept = strings.TrimSpace(firstNonEmpty(dept, r.Team, r.Categories.Department, r.Categories.Team))
		lower := strings.ToLower(j.Location)
		j.Remote = r.IsRemote || strings.EqualFold(r.WorkplaceType, "remote") || strings.Contains(lower, "remote") || strings.Contains(lower, "distributed")
		switch {
		case r.CreatedAt > 0:
			j.Posted = time.UnixMilli(r.CreatedAt).UTC()
		case r.FirstPublished != "":
			j.Posted, _ = time.Parse(time.RFC3339, r.FirstPublished)
		case r.PublishedAt != "":
			j.Posted, _ = time.Parse(time.RFC3339, r.PublishedAt)
		case r.UpdatedAt != "":
			j.Posted, _ = time.Parse(time.RFC3339, r.UpdatedAt)
		}
		if j.Title == "" {
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// clean drops control characters from a feed-supplied string, collapses
// whitespace, and caps its length; feed text is content, never trusted.
func clean(s string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\u2028' || r == '\u2029' {
			r = ' '
		}
		if r == ' ' || r == '\t' || r == '\n' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
		n++
		if n >= maxText {
			break
		}
	}
	return b.String()
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// functions is the fixed set the seed's "why" text uses; order breaks count ties.
var functions = []string{"engineering", "data", "product", "design", "sales", "marketing", "customer success/support", "recruiting/people", "finance/legal/ops"}

// classify maps a department (preferred) or title to one of the fixed functions, or "other".
func classify(dept, title string) string {
	if f := classifyText(dept); f != "" {
		return f
	}
	if f := classifyText(title); f != "" {
		return f
	}
	return "other"
}

var rules = []struct {
	fn   string
	keys []string
}{
	{"product", []string{"product manag", "product management", "product operations", "program manag", "technical program"}},
	{"recruiting/people", []string{"recruit", "talent", "people ops", "people &", "people team", "people operations", "human resources", " hr ", "hr ", "hrbp"}},
	{"design", []string{"design", "ux", "user experience", "creative"}},
	{"marketing", []string{"marketing", "growth", "content", "communications", "brand", "seo", "social media", "demand gen", "events"}},
	{"customer success/support", []string{"customer success", "customer support", "customer experience", "customer service", "support", "success", "implementation", "onboarding", "professional services", "solutions consult", "technical account", "customer engineer", "deployment"}},
	{"sales", []string{"sales", "account executive", "account manager", "account director", "business development", "bdr", "sdr", "revenue", "partnership", "go-to-market", "gtm", "solutions engineer", "solution engineer", "pre-sales", "presales", "channel"}},
	{"data", []string{"data", "analytics", "machine learning", "ml ", " ml", "applied science", "research scientist", "statistic"}},
	{"product", []string{"product manag", "product management", "program manag", "product operations", "technical program", "product"}},
	{"finance/legal/ops", []string{"finance", "financ", "accounting", "legal", "counsel", "compliance", "operations", "ops", "administrat", "office", "procurement", "strategy", "corporate", "facilities", "risk", "audit", "tax", "treasury", "payroll", "executive assistant", "chief of staff", "workplace", "supply chain", "logistics", "fraud", "trust & safety", "trust and safety", "policy"}},
	{"engineering", []string{"engineer", "software", "developer", "infrastructure", "platform", "devops", "sre", "site reliability", "security", "information technology", " it", "it ", "technology", "technical staff", "hardware", "qa", "quality assurance", "architect", "research", "science", "scientist", "systems", "network", "cloud", "mobile", "frontend", "backend", "full stack", "fullstack", "web"}},
}

func classifyText(s string) string {
	s = " " + strings.ToLower(strings.TrimSpace(s)) + " "
	if strings.TrimSpace(s) == "" {
		return ""
	}
	for _, r := range rules {
		for _, k := range r.keys {
			if strings.Contains(s, k) {
				return r.fn
			}
		}
	}
	return ""
}

var (
	usWords  = regexp.MustCompile(`(?i)\b(united states|usa|u\.s\.a?\.?|us|america|anywhere)\b`)
	usPlaces = regexp.MustCompile(`(?i)\b(phoenix|arizona|scottsdale|tempe|chandler|new york|nyc|brooklyn|san francisco|bay area|palo alto|mountain view|menlo park|san jose|sunnyvale|santa clara|redwood city|foster city|san mateo|oakland|berkeley|los angeles|santa monica|irvine|san diego|seattle|bellevue|redmond|portland|austin|dallas|houston|san antonio|denver|boulder|chicago|boston|cambridge|somerville|atlanta|miami|orlando|tampa|nashville|charlotte|raleigh|durham|washington|arlington|reston|mclean|baltimore|philadelphia|pittsburgh|detroit|ann arbor|minneapolis|salt lake|lehi|provo|las vegas|reno|columbus|cincinnati|cleveland|indianapolis|kansas city|st\.? louis|new jersey|jersey city|connecticut|stamford|virginia|maryland|massachusetts|california|texas|colorado|florida|georgia|illinois|michigan|minnesota|missouri|nevada|north carolina|ohio|oregon|pennsylvania|tennessee|utah|wisconsin|madison|milwaukee|omaha|oklahoma|new orleans|louisiana|alabama|kentucky|louisville|indiana|iowa|idaho|boise|montana|new mexico|albuquerque|hawaii|alaska|maine|vermont|new hampshire|rhode island|delaware|south carolina|west virginia|arkansas|mississippi|nebraska|kansas|north dakota|south dakota|wyoming)\b`)
	usState  = regexp.MustCompile(`, (AL|AK|AZ|AR|CA|CO|CT|DE|FL|GA|HI|ID|IL|IN|IA|KS|KY|LA|ME|MD|MA|MI|MN|MS|MO|MT|NE|NV|NH|NJ|NM|NY|NC|ND|OH|OK|OR|PA|RI|SC|SD|TN|TX|UT|VT|VA|WA|WV|WI|WY|DC)\b`)
	nonUS    = regexp.MustCompile(`(?i)\b(canada|toronto|vancouver|montreal|ottawa|calgary|uk|united kingdom|london|england|scotland|europe|emea|eu|germany|berlin|munich|france|paris|spain|madrid|barcelona|portugal|lisbon|netherlands|amsterdam|ireland|dublin|poland|warsaw|krakow|sweden|stockholm|norway|denmark|copenhagen|finland|helsinki|switzerland|zurich|austria|vienna|italy|milan|israel|tel aviv|india|bangalore|bengaluru|hyderabad|pune|mumbai|delhi|gurgaon|chennai|singapore|japan|tokyo|korea|seoul|china|shanghai|beijing|hong kong|taiwan|taipei|australia|sydney|melbourne|new zealand|auckland|brazil|sao paulo|são paulo|mexico|argentina|buenos aires|colombia|bogota|chile|peru|latam|apac|philippines|manila|vietnam|indonesia|jakarta|thailand|bangkok|malaysia|dubai|uae|south africa|nigeria|kenya|egypt|turkey|istanbul|czech|prague|romania|bucharest|hungary|budapest|greece|athens|belgium|brussels|estonia|tallinn|lithuania|ukraine|kyiv|serbia|bulgaria|croatia|slovakia)\b`)
)

// isUS reports whether a posting's location text names the United States, a US
// place, or plain "Remote" with no country.
func isUS(loc string) bool {
	t := strings.TrimSpace(loc)
	if t == "" {
		return false
	}
	if strings.EqualFold(t, "remote") {
		return true
	}
	us := usWords.MatchString(t) || usPlaces.MatchString(t) || usState.MatchString(t)
	if nonUS.MatchString(t) && !us {
		return false
	}
	return us
}

func summarize(jobs []job, day time.Time) stats {
	st := stats{Total: len(jobs), Functions: map[string]int{}}
	usLocs := map[string]int{}
	allLocs := map[string]int{}
	cutoff := day.AddDate(0, 0, -7)
	for _, j := range jobs {
		st.Functions[classify(j.Dept, j.Title)]++
		if j.Remote {
			st.Remote++
		}
		u := isUS(j.Location)
		if u {
			st.US++
		}
		if !j.Posted.IsZero() && !j.Posted.Before(cutoff) {
			st.Recent++
		}
		if j.Location != "" {
			allLocs[j.Location]++
			if u || (j.Remote && !nonUS.MatchString(j.Location)) {
				usLocs[j.Location]++
			}
		}
	}
	if loc := top(usLocs); loc != "" {
		st.TopLocation, st.TopIsUS = loc, true
	} else {
		st.TopLocation = top(allLocs)
	}
	return st
}

func top(m map[string]int) string {
	best, n := "", 0
	for k, v := range m {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best
}

func priority(total int) string {
	switch {
	case total >= 300:
		return "A"
	case total >= 75:
		return "B"
	}
	return "C"
}

func whyText(st stats) string {
	type kv struct {
		k string
		v int
	}
	var parts []kv
	for _, f := range functions {
		if st.Functions[f] > 0 {
			parts = append(parts, kv{f, st.Functions[f]})
		}
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].v > parts[j].v })
	if len(parts) > 4 {
		parts = parts[:4]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Board lists %d open roles today", st.Total)
	if len(parts) > 0 {
		b.WriteString(": ")
		for i, p := range parts {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%d %s", p.v, p.k)
		}
	}
	b.WriteString(".")
	switch {
	case st.TopLocation != "" && st.TopIsUS:
		fmt.Fprintf(&b, " Most-listed US/remote location: %s.", st.TopLocation)
	case st.TopLocation != "":
		fmt.Fprintf(&b, " Most-listed location: %s (no US or remote roles listed).", st.TopLocation)
	}
	return b.String()
}

func hostLabel(sourceURL string) string {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return "job"
	}
	h := strings.ToLower(u.Host)
	for _, k := range []string{"greenhouse", "lever", "ashby"} {
		if strings.Contains(h, k) {
			return k
		}
	}
	return "job"
}

func totalFromSignal(signal string) int {
	m := totalRe.FindStringSubmatch(signal)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func printReport(w io.Writer, cs []company, byID map[string]stats, all stats, dead, stale []string, day string) {
	type row struct {
		c  company
		st stats
	}
	var rows []row
	for _, c := range cs {
		if st, ok := byID[c.ID]; ok {
			rows = append(rows, row{c, st})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].st.Total > rows[j].st.Total })
	fmt.Fprintf(w, "Open roles as of %s across %d boards (%d answered, %d failed)\n", day, len(cs), len(rows), len(dead))
	fmt.Fprintf(w, "%-28s %6s %6s %6s %6s  %s\n", "company", "total", "us", "remote", "7d", "top functions")
	for _, r := range rows {
		fmt.Fprintf(w, "%-28s %6d %6d %6d %6d  %s\n", trunc(r.c.Name, 28), r.st.Total, r.st.US, r.st.Remote, r.st.Recent, topFunctions(r.st.Functions, 3))
	}
	fmt.Fprintf(w, "%-28s %6d %6d %6d %6d  %s\n", "TOTAL", all.Total, all.US, all.Remote, all.Recent, topFunctions(all.Functions, 10))
	for _, d := range dead {
		fmt.Fprintf(w, "FAILED %s\n", d)
	}
	for _, s := range stale {
		fmt.Fprintf(w, "STALE  %s (remove it, or fix its sourceUrl, and add a _removed tombstone if you drop it)\n", s)
	}
}

func topFunctions(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var parts []kv
	for k, v := range m {
		parts = append(parts, kv{k, v})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].v != parts[j].v {
			return parts[i].v > parts[j].v
		}
		return parts[i].k < parts[j].k
	})
	if len(parts) > n {
		parts = parts[:n]
	}
	ss := make([]string, len(parts))
	for i, p := range parts {
		ss[i] = fmt.Sprintf("%s %d", p.k, p.v)
	}
	return strings.Join(ss, ", ")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
