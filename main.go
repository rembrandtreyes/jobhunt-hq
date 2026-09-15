// Job Hunt HQ — local edition.
//
// A single Go binary that serves the HQ page and stores everything in a
// SQLite file next to it. No cgo: the SQLite driver is pure Go (WASM).
//
//	go mod tidy && go run .          # http://127.0.0.1:8787
//	go build -o hq . && ./hq -db ~/hq.db -addr 127.0.0.1:8787
package main

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed seed.json
var seedJSON []byte

// ---------- types (field names match the page's JSON) ----------

type Application struct {
	ID         string `json:"id"`
	Company    string `json:"company"`
	Role       string `json:"role"`
	URL        string `json:"url"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	Location   string `json:"location"`
	Salary     string `json:"salary"`
	AppliedAt  string `json:"appliedAt"`
	NextDate   string `json:"nextDate"`
	NextAction string `json:"nextAction"`
	Contact    string `json:"contact"`
	Notes      string `json:"notes"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

type Company struct {
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

type Settings struct {
	StartDate  string          `json:"startDate,omitempty"`
	WeeklyGoal int             `json:"weeklyGoal,omitempty"`
	Track      string          `json:"track,omitempty"`    // study track id, one of tracks/*.json
	Schedule   json.RawMessage `json:"schedule,omitempty"` // the user's own day, see customize.go; null clears
	Focus      json.RawMessage `json:"focus,omitempty"`    // the user's week focus labels/lines, see customize.go; null clears
}

type State struct {
	Apps      []Application   `json:"apps"`
	Companies []Company       `json:"companies"`
	Done      map[string]bool `json:"done"`
	Settings  Settings        `json:"settings"`
}

var (
	idRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	statuses = map[string]bool{"saved": true, "applied": true, "screen": true, "technical": true, "onsite": true, "offer": true, "rejected": true, "withdrawn": true}
)

const schema = `
CREATE TABLE IF NOT EXISTS applications (
  id          TEXT PRIMARY KEY,
  company     TEXT NOT NULL,
  role        TEXT NOT NULL,
  url         TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'applied',
  source      TEXT NOT NULL DEFAULT '',
  location    TEXT NOT NULL DEFAULT '',
  salary      TEXT NOT NULL DEFAULT '',
  applied_at  TEXT NOT NULL DEFAULT '',
  next_date   TEXT NOT NULL DEFAULT '',
  next_action TEXT NOT NULL DEFAULT '',
  contact     TEXT NOT NULL DEFAULT '',
  notes       TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS application_events (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  application_id TEXT NOT NULL,
  status         TEXT NOT NULL,
  at             TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_app ON application_events(application_id);
CREATE TABLE IF NOT EXISTS companies (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  priority    TEXT NOT NULL DEFAULT 'B',
  location    TEXT NOT NULL DEFAULT '',
  careers_url TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'Researching',
  why         TEXT NOT NULL DEFAULT '',
  notes       TEXT NOT NULL DEFAULT '',
  signal      TEXT NOT NULL DEFAULT '',
  source_url  TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS study_progress (
  item_id TEXT PRIMARY KEY,
  done_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`

// ---------- server ----------

type server struct {
	db     *sql.DB
	tracks *trackSet // study tracks from tracks/*.json, validated at startup
	page   []byte    // web/index.html with the tracks JSON injected
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "address to listen on")
	dbPath := flag.String("db", "hq.db", "path to the SQLite database file (created on first run)")
	flag.Parse()

	s, err := newServer(*dbPath)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Job Hunt HQ  →  http://%s   (database: %s)", *addr, *dbPath)
	log.Fatal(http.ListenAndServe(*addr, logRequests(s.routes())))
}

// newServer opens (creating if needed) the SQLite database at dbPath,
// applies the schema, and seeds it on first run. The caller owns s.db.
func newServer(dbPath string) (*server, error) { return newServerFS(dbPath, trackFiles) }

// newServerFS is newServer with the study tracks read from fsys (tests pass
// a synthetic one to try out a contributed track).
func newServerFS(dbPath string, fsys fs.FS) (*server, error) {
	tracks, err := loadTracks(fsys, "tracks")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer, no contention; plenty for one person
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &server{db: db, tracks: tracks, page: renderPage(indexHTML, tracks)}
	if err := s.seedIfEmpty(); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed: %w", err)
	}
	return s, nil
}

// routes returns the page + API handler (without request logging).
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(s.page)
	})
	mux.HandleFunc("GET /api/tracks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.tracks.json)
	})
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/export", s.handleState)
	mux.HandleFunc("PUT /api/applications/{id}", s.handlePutApplication)
	mux.HandleFunc("DELETE /api/applications/{id}", s.handleDeleteApplication)
	mux.HandleFunc("PUT /api/companies/{id}", s.handlePutCompany)
	mux.HandleFunc("DELETE /api/companies/{id}", s.handleDeleteCompany)
	mux.HandleFunc("PUT /api/study", s.handlePutStudy)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("POST /api/import", s.handleImport)
	return mux
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------- reads ----------

func (s *server) loadState() (*State, error) {
	st := &State{Apps: []Application{}, Companies: []Company{}, Done: map[string]bool{}}

	rows, err := s.db.Query(`SELECT id, company, role, url, status, source, location, salary, applied_at, next_date, next_action, contact, notes, created_at, updated_at FROM applications ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Application
		if err := rows.Scan(&a.ID, &a.Company, &a.Role, &a.URL, &a.Status, &a.Source, &a.Location, &a.Salary, &a.AppliedAt, &a.NextDate, &a.NextAction, &a.Contact, &a.Notes, &a.CreatedAt, &a.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		st.Apps = append(st.Apps, a)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT id, name, priority, location, careers_url, status, why, notes, signal, source_url, created_at, updated_at FROM companies ORDER BY priority, name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Company
		if err := rows.Scan(&c.ID, &c.Name, &c.Priority, &c.Location, &c.CareersURL, &c.Status, &c.Why, &c.Notes, &c.Signal, &c.SourceURL, &c.CreatedAt, &c.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		st.Companies = append(st.Companies, c)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT item_id FROM study_progress`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		st.Done[id] = true
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		switch k {
		case "startDate":
			st.Settings.StartDate = v
		case "weeklyGoal":
			st.Settings.WeeklyGoal, _ = strconv.Atoi(v)
		case "track":
			st.Settings.Track = v
		case "schedule":
			st.Settings.Schedule = json.RawMessage(v)
		case "focus":
			st.Settings.Focus = json.RawMessage(v)
		}
	}
	rows.Close()
	return st, nil
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadState()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if r.URL.Path == "/api/export" {
		w.Header().Set("Content-Disposition", `attachment; filename="jobhunt-hq-export.json"`)
	}
	writeJSON(w, 200, st)
}

// ---------- writes ----------

func (s *server) handlePutApplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		httpError(w, 400, "bad id")
		return
	}
	var a Application
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&a); err != nil {
		httpError(w, 400, "bad json: "+err.Error())
		return
	}
	a.ID = id
	if a.Company == "" || a.Role == "" {
		httpError(w, 400, "company and role are required")
		return
	}
	if !statuses[a.Status] {
		httpError(w, 400, "unknown status "+a.Status)
		return
	}
	ts := now()
	if a.CreatedAt == "" {
		a.CreatedAt = ts
	}
	a.UpdatedAt = ts

	tx, err := s.db.Begin()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	if err := upsertApplication(tx, a, ts); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, a)
}

// upsertApplication writes the row and, when the row is new or its status
// changed, exactly one application_events entry — inside the caller's
// transaction. Shared by PUT /api/applications/{id} and POST /api/import.
func upsertApplication(tx *sql.Tx, a Application, ts string) error {
	var prevStatus string
	err := tx.QueryRow(`SELECT status FROM applications WHERE id = ?`, a.ID).Scan(&prevStatus)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO applications (id, company, role, url, status, source, location, salary, applied_at, next_date, next_action, contact, notes, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET company=excluded.company, role=excluded.role, url=excluded.url, status=excluded.status, source=excluded.source,
		  location=excluded.location, salary=excluded.salary, applied_at=excluded.applied_at, next_date=excluded.next_date, next_action=excluded.next_action,
		  contact=excluded.contact, notes=excluded.notes, updated_at=excluded.updated_at`,
		a.ID, a.Company, a.Role, a.URL, a.Status, a.Source, a.Location, a.Salary, a.AppliedAt, a.NextDate, a.NextAction, a.Contact, a.Notes, a.CreatedAt, a.UpdatedAt); err != nil {
		return err
	}
	if isNew || prevStatus != a.Status {
		if _, err := tx.Exec(`INSERT INTO application_events (application_id, status, at) VALUES (?,?,?)`, a.ID, a.Status, ts); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) handleDeleteApplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.Exec(`DELETE FROM applications WHERE id = ?`, id); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.db.Exec(`DELETE FROM application_events WHERE application_id = ?`, id)
	w.WriteHeader(204)
}

func (s *server) handlePutCompany(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		httpError(w, 400, "bad id")
		return
	}
	var c Company
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
		httpError(w, 400, "bad json: "+err.Error())
		return
	}
	c.ID = id
	if c.Name == "" {
		httpError(w, 400, "name is required")
		return
	}
	if c.Priority == "" {
		c.Priority = "B"
	}
	if c.Status == "" {
		c.Status = "Researching"
	}
	ts := now()
	if c.CreatedAt == "" {
		c.CreatedAt = ts
	}
	c.UpdatedAt = ts
	if err := upsertCompany(s.db, c); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c)
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertCompany(db execer, c Company) error {
	_, err := db.Exec(`INSERT INTO companies (id, name, priority, location, careers_url, status, why, notes, signal, source_url, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, priority=excluded.priority, location=excluded.location, careers_url=excluded.careers_url,
		  status=excluded.status, why=excluded.why, notes=excluded.notes, signal=excluded.signal, source_url=excluded.source_url, updated_at=excluded.updated_at`,
		c.ID, c.Name, c.Priority, c.Location, c.CareersURL, c.Status, c.Why, c.Notes, c.Signal, c.SourceURL, c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *server) handleDeleteCompany(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(`DELETE FROM companies WHERE id = ?`, r.PathValue("id")); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *server) handlePutStudy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Done map[string]bool `json:"done"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, 400, "bad json: "+err.Error())
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	// Keep original done_at for items that are still checked; add new ones; drop unchecked.
	existing := map[string]bool{}
	rows, err := tx.Query(`SELECT item_id FROM study_progress`)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		existing[id] = true
	}
	rows.Close()
	ts := now()
	for id, on := range body.Done {
		if on && !existing[id] {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO study_progress (item_id, done_at) VALUES (?,?)`, id, ts); err != nil {
				httpError(w, 500, err.Error())
				return
			}
		}
	}
	for id := range existing {
		if !body.Done[id] {
			if _, err := tx.Exec(`DELETE FROM study_progress WHERE item_id = ?`, id); err != nil {
				httpError(w, 500, err.Error())
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		httpError(w, 400, "bad json: "+err.Error())
		return
	}
	if in.StartDate != "" {
		if _, err := time.Parse("2006-01-02", in.StartDate); err != nil {
			httpError(w, 400, "startDate must be YYYY-MM-DD")
			return
		}
	}
	if in.WeeklyGoal < 0 || in.WeeklyGoal > 1000 {
		httpError(w, 400, "weeklyGoal out of range")
		return
	}
	if in.Track != "" && !s.tracks.has(in.Track) {
		httpError(w, 400, "unknown track "+in.Track)
		return
	}
	custom, err := validateCustom(in, s.tracks.has)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	up := func(k, v string) error {
		_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
		return err
	}
	if in.StartDate != "" {
		if err := up("startDate", in.StartDate); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if in.WeeklyGoal > 0 {
		if err := up("weeklyGoal", strconv.Itoa(in.WeeklyGoal)); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if in.Track != "" {
		if err := up("track", in.Track); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if err := applyCustom(tx, custom); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// handleImport merges an export — the /api/state shape — into the database.
// Applications and companies upsert by id (a status change is logged as an
// event, exactly like a PUT), study items are added, settings apply when set.
// Nothing is deleted. All-or-nothing: one invalid record rejects the body.
func (s *server) handleImport(w http.ResponseWriter, r *http.Request) {
	var in State
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&in); err != nil {
		httpError(w, 400, "bad json: "+err.Error())
		return
	}
	for i, a := range in.Apps {
		switch {
		case !idRe.MatchString(a.ID):
			httpError(w, 400, fmt.Sprintf("apps[%d]: bad id", i))
			return
		case a.Company == "" || a.Role == "":
			httpError(w, 400, fmt.Sprintf("apps[%d]: company and role are required", i))
			return
		case !statuses[a.Status]:
			httpError(w, 400, fmt.Sprintf("apps[%d]: unknown status %s", i, a.Status))
			return
		}
	}
	for i, c := range in.Companies {
		if !idRe.MatchString(c.ID) || c.Name == "" {
			httpError(w, 400, fmt.Sprintf("companies[%d]: id and name are required", i))
			return
		}
	}
	if in.Settings.StartDate != "" {
		if _, err := time.Parse("2006-01-02", in.Settings.StartDate); err != nil {
			httpError(w, 400, "settings.startDate must be YYYY-MM-DD")
			return
		}
	}
	if in.Settings.WeeklyGoal < 0 || in.Settings.WeeklyGoal > 1000 {
		httpError(w, 400, "settings.weeklyGoal out of range")
		return
	}
	if in.Settings.Track != "" && !s.tracks.has(in.Settings.Track) {
		httpError(w, 400, "settings.track: unknown track "+in.Settings.Track)
		return
	}
	custom, err := validateCustom(in.Settings, s.tracks.has)
	if err != nil {
		httpError(w, 400, "settings."+err.Error())
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	ts := now()
	for _, a := range in.Apps {
		if a.CreatedAt == "" {
			a.CreatedAt = ts
		}
		a.UpdatedAt = ts
		if err := upsertApplication(tx, a, ts); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	for _, c := range in.Companies {
		if c.Priority == "" {
			c.Priority = "B"
		}
		if c.Status == "" {
			c.Status = "Researching"
		}
		if c.CreatedAt == "" {
			c.CreatedAt = ts
		}
		c.UpdatedAt = ts
		if err := upsertCompany(tx, c); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	added := 0
	for id, on := range in.Done {
		if !on {
			continue
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO study_progress (item_id, done_at) VALUES (?,?)`, id, ts)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	up := func(k, v string) error {
		_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
		return err
	}
	if in.Settings.StartDate != "" {
		if err := up("startDate", in.Settings.StartDate); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if in.Settings.WeeklyGoal > 0 {
		if err := up("weeklyGoal", strconv.Itoa(in.Settings.WeeklyGoal)); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if in.Settings.Track != "" {
		if err := up("track", in.Settings.Track); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if err := applyCustom(tx, custom); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"apps": len(in.Apps), "companies": len(in.Companies), "done": added})
}

// ---------- seed ----------

// seedIfEmpty loads seed.json (the researched company list, plus anything
// else exported from the hosted version) the first time the database is
// created. It never overwrites existing rows.
func (s *server) seedIfEmpty() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM companies`).Scan(&n); err != nil {
		return err
	}
	var seed State
	if err := json.Unmarshal(seedJSON, &seed); err != nil {
		return fmt.Errorf("seed.json: %w", err)
	}
	if n > 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	for _, c := range seed.Companies {
		if !idRe.MatchString(c.ID) || c.Name == "" {
			continue
		}
		if c.CreatedAt == "" {
			c.CreatedAt = ts
		}
		if c.UpdatedAt == "" {
			c.UpdatedAt = ts
		}
		if err := upsertCompany(tx, c); err != nil {
			return err
		}
	}
	for _, a := range seed.Apps {
		if !idRe.MatchString(a.ID) || a.Company == "" || !statuses[a.Status] {
			continue
		}
		if a.CreatedAt == "" {
			a.CreatedAt = ts
		}
		if a.UpdatedAt == "" {
			a.UpdatedAt = ts
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO applications (id, company, role, url, status, source, location, salary, applied_at, next_date, next_action, contact, notes, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.ID, a.Company, a.Role, a.URL, a.Status, a.Source, a.Location, a.Salary, a.AppliedAt, a.NextDate, a.NextAction, a.Contact, a.Notes, a.CreatedAt, a.UpdatedAt); err != nil {
			return err
		}
	}
	for id, on := range seed.Done {
		if on {
			tx.Exec(`INSERT OR IGNORE INTO study_progress (item_id, done_at) VALUES (?,?)`, id, ts)
		}
	}
	if seed.Settings.StartDate != "" {
		tx.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES ('startDate', ?)`, seed.Settings.StartDate)
	}
	if seed.Settings.WeeklyGoal > 0 {
		tx.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES ('weeklyGoal', ?)`, strconv.Itoa(seed.Settings.WeeklyGoal))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("seeded %d companies from seed.json", len(seed.Companies))
	return nil
}
