package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard) // silence "seeded N companies" and friends
	os.Exit(m.Run())
}

// newTestServer builds a server on a fresh temp-file database and returns
// its handler. The database is closed when the test ends.
func newTestServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	s := openTestServer(t, filepath.Join(t.TempDir(), "hq.db"))
	return s, s.routes()
}

func openTestServer(t *testing.T, path string) *server {
	t.Helper()
	s, err := newServer(path)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getState(t *testing.T, h http.Handler) State {
	t.Helper()
	rec := do(t, h, "GET", "/api/state", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/state: %d %s", rec.Code, rec.Body)
	}
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return st
}

func count(t *testing.T, s *server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func seedCompanyCount(t *testing.T) int {
	t.Helper()
	var seed State
	if err := json.Unmarshal(seedJSON, &seed); err != nil {
		t.Fatalf("seed.json: %v", err)
	}
	if len(seed.Companies) == 0 {
		t.Fatal("seed.json has no companies")
	}
	return len(seed.Companies)
}

func app(company, role, status string) Application {
	return Application{Company: company, Role: role, Status: status}
}

// ---------- seeding ----------

func TestSeedRunsOnEmptyDB(t *testing.T) {
	_, h := newTestServer(t)
	st := getState(t, h)
	if want := seedCompanyCount(t); len(st.Companies) != want {
		t.Fatalf("companies after first run = %d, want %d", len(st.Companies), want)
	}
	if len(st.Apps) != 0 {
		t.Fatalf("apps after first run = %d, want 0", len(st.Apps))
	}
}

func TestSeedSkipsPopulatedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hq.db")
	s := openTestServer(t, path)
	keep := getState(t, s.routes()).Companies[0].ID
	if _, err := s.db.Exec(`DELETE FROM companies WHERE id <> ?`, keep); err != nil {
		t.Fatal(err)
	}
	s.db.Close()

	s2 := openTestServer(t, path) // second start against the same file
	st := getState(t, s2.routes())
	if len(st.Companies) != 1 || st.Companies[0].ID != keep {
		t.Fatalf("seed re-ran on a populated db: got %d companies", len(st.Companies))
	}
}

// ---------- applications ----------

func TestPutApplicationWritesEventOnlyOnStatusChange(t *testing.T) {
	s, h := newTestServer(t)
	const id = "acme-swe"
	events := func() int {
		return count(t, s, `SELECT COUNT(*) FROM application_events WHERE application_id = ?`, id)
	}

	if rec := do(t, h, "PUT", "/api/applications/"+id, app("Acme", "SWE", "applied")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM applications WHERE id = ?`, id); n != 1 {
		t.Fatalf("applications rows = %d, want 1", n)
	}
	if n := events(); n != 1 {
		t.Fatalf("events after create = %d, want 1", n)
	}

	// Same status, different notes: an edit, not a transition.
	a := app("Acme", "SWE", "applied")
	a.Notes = "edited"
	if rec := do(t, h, "PUT", "/api/applications/"+id, a); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	if n := events(); n != 1 {
		t.Fatalf("events after same-status edit = %d, want 1", n)
	}

	if rec := do(t, h, "PUT", "/api/applications/"+id, app("Acme", "SWE", "screen")); rec.Code != 200 {
		t.Fatalf("transition: %d %s", rec.Code, rec.Body)
	}
	if n := events(); n != 2 {
		t.Fatalf("events after status change = %d, want 2", n)
	}
	var last string
	if err := s.db.QueryRow(`SELECT status FROM application_events WHERE application_id = ? ORDER BY id DESC LIMIT 1`, id).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != "screen" {
		t.Fatalf("latest event status = %q, want screen", last)
	}

	st := getState(t, h)
	if len(st.Apps) != 1 || st.Apps[0].Status != "screen" || st.Apps[0].Notes != "" {
		t.Fatalf("state after transition = %+v", st.Apps)
	}
}

func TestPutApplicationValidation(t *testing.T) {
	_, h := newTestServer(t)
	cases := []struct {
		name string
		id   string
		body Application
		want string
	}{
		{"bad id (dot)", "not.valid", app("Acme", "SWE", "applied"), "bad id"},
		{"bad id (too long)", strings.Repeat("a", 65), app("Acme", "SWE", "applied"), "bad id"},
		{"missing company", "ok", app("", "SWE", "applied"), "company and role are required"},
		{"missing role", "ok", app("Acme", "", "applied"), "company and role are required"},
		{"unknown status", "ok", app("Acme", "SWE", "ghosted"), "unknown status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "PUT", "/api/applications/"+tc.id, tc.body)
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %s, want it to mention %q", rec.Body, tc.want)
			}
		})
	}
	if st := getState(t, h); len(st.Apps) != 0 {
		t.Fatalf("rejected writes leaked into state: %+v", st.Apps)
	}
}

func TestDeleteApplicationRemovesEvents(t *testing.T) {
	s, h := newTestServer(t)
	do(t, h, "PUT", "/api/applications/x", app("Acme", "SWE", "applied"))
	do(t, h, "PUT", "/api/applications/x", app("Acme", "SWE", "screen"))
	if rec := do(t, h, "DELETE", "/api/applications/x", nil); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM application_events WHERE application_id = 'x'`); n != 0 {
		t.Fatalf("events left behind = %d", n)
	}
}

// ---------- study plan ----------

func TestPutStudyReplacesSetAndPreservesDoneAt(t *testing.T) {
	s, h := newTestServer(t)
	put := func(done map[string]bool) {
		t.Helper()
		if rec := do(t, h, "PUT", "/api/study", map[string]any{"done": done}); rec.Code != 204 {
			t.Fatalf("PUT /api/study: %d %s", rec.Code, rec.Body)
		}
	}
	put(map[string]bool{"w1-resume": true, "w1-linkedin": true})

	// Backdate one row so a preserved timestamp is distinguishable from a rewrite.
	const old = "2020-01-01T00:00:00Z"
	if _, err := s.db.Exec(`UPDATE study_progress SET done_at = ? WHERE item_id = 'w1-resume'`, old); err != nil {
		t.Fatal(err)
	}

	put(map[string]bool{"w1-resume": true, "w2-projects": true, "w1-linkedin": false})

	st := getState(t, h)
	want := map[string]bool{"w1-resume": true, "w2-projects": true}
	if len(st.Done) != len(want) {
		t.Fatalf("done = %v, want %v", st.Done, want)
	}
	for id := range want {
		if !st.Done[id] {
			t.Fatalf("done = %v, want %v", st.Done, want)
		}
	}
	var got string
	if err := s.db.QueryRow(`SELECT done_at FROM study_progress WHERE item_id = 'w1-resume'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != old {
		t.Fatalf("done_at for a still-checked item was rewritten: %s", got)
	}
}

// ---------- settings ----------

func TestSettingsRoundTrip(t *testing.T) {
	_, h := newTestServer(t)
	if rec := do(t, h, "PUT", "/api/settings", Settings{StartDate: "2026-09-01", WeeklyGoal: 15}); rec.Code != 204 {
		t.Fatalf("PUT /api/settings: %d %s", rec.Code, rec.Body)
	}
	if st := getState(t, h); st.Settings != (Settings{StartDate: "2026-09-01", WeeklyGoal: 15}) {
		t.Fatalf("settings = %+v", st.Settings)
	}

	// Zero values are ignored, not written.
	if rec := do(t, h, "PUT", "/api/settings", Settings{WeeklyGoal: 7}); rec.Code != 204 {
		t.Fatalf("partial PUT: %d %s", rec.Code, rec.Body)
	}
	if st := getState(t, h); st.Settings != (Settings{StartDate: "2026-09-01", WeeklyGoal: 7}) {
		t.Fatalf("settings after partial update = %+v", st.Settings)
	}

	rec := do(t, h, "PUT", "/api/settings", map[string]any{"startDate": "09/01/2026"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "YYYY-MM-DD") {
		t.Fatalf("malformed startDate: %d %s", rec.Code, rec.Body)
	}
	if st := getState(t, h); st.Settings.StartDate != "2026-09-01" {
		t.Fatalf("rejected startDate was stored: %+v", st.Settings)
	}
}
