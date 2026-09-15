package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// embeddedTracksFS copies the shipped track files into a MapFS so a test can
// add a contributed track next to them.
func embeddedTracksFS(t *testing.T) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	for _, name := range []string{"general", "engineering"} {
		b, err := trackFiles.ReadFile("tracks/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		fsys["tracks/"+name+".json"] = &fstest.MapFile{Data: b}
	}
	return fsys
}

// salesTrack is the smallest valid contributed track.
func salesTrack() map[string]any {
	return map[string]any{
		"id": "sales", "label": "Sales", "focusLabels": []string{"Pipeline"},
		"schedule": []map[string]string{{"t": "9:00", "d": "1 hr", "w": "Prospecting", "h": "Twenty cold emails."}},
		"plan": []map[string]any{{
			"n": 1, "theme": "Build the pipeline", "focus": []string{"Twenty target accounts"},
			"items": []map[string]string{{"id": "s1-accounts", "t": "List twenty target accounts"}},
		}},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTracksLoadFromEmbeddedFiles(t *testing.T) {
	set, err := loadTracks(trackFiles, "tracks")
	if err != nil {
		t.Fatal(err)
	}
	if !set.has("general") || !set.has("engineering") || len(set.ids) != 2 {
		t.Fatalf("ids = %v", set.ids)
	}
	count := func(id string) (n int, first string) {
		var s trackShape
		if err := json.Unmarshal(set.raw[id], &s); err != nil {
			t.Fatal(err)
		}
		for _, w := range s.Plan {
			n += len(w.Items)
		}
		return n, s.Plan[0].Items[0].ID
	}
	// The ids are progress keys; these counts and first ids are what the page shipped with.
	if n, first := count("engineering"); n != 58 || first != "w1-resume" {
		t.Fatalf("engineering: %d items, first %q", n, first)
	}
	if n, first := count("general"); n != 43 || !strings.HasPrefix(first, "g1-") {
		t.Fatalf("general: %d items, first %q", n, first)
	}
}

func TestTracksAPIAndPageInjection(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(t, h, "GET", "/api/tracks", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/tracks: %d %s", rec.Code, rec.Body)
	}
	var got map[string]struct {
		Label       string                     `json:"label"`
		FocusLabels []string                   `json:"focusLabels"`
		Plan        []json.RawMessage          `json:"plan"`
		Schedule    []json.RawMessage          `json:"schedule"`
		Weekend     map[string]json.RawMessage `json:"weekend"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"general", "engineering"} {
		tr, ok := got[id]
		if !ok || tr.Label == "" || len(tr.FocusLabels) == 0 || len(tr.Plan) == 0 || len(tr.Schedule) == 0 || len(tr.Weekend) == 0 {
			t.Fatalf("track %s incomplete: %+v", id, tr)
		}
	}

	if n := bytes.Count(indexHTML, []byte(tracksPlaceholder)); n != 1 {
		t.Fatalf("web/index.html has %d %s placeholders, want 1", n, tracksPlaceholder)
	}
	page := do(t, h, "GET", "/", nil)
	if page.Code != 200 || bytes.Contains(page.Body.Bytes(), []byte(tracksPlaceholder)) {
		t.Fatalf("page: %d, placeholder replaced=%v", page.Code, !bytes.Contains(page.Body.Bytes(), []byte(tracksPlaceholder)))
	}
	open := []byte(`<script id="tracks-data" type="application/json">`)
	i := bytes.Index(page.Body.Bytes(), open)
	if i < 0 {
		t.Fatal("page has no tracks data script")
	}
	rest := page.Body.Bytes()[i+len(open):]
	j := bytes.Index(rest, []byte("</script>"))
	var injected map[string]json.RawMessage
	if err := json.Unmarshal(rest[:j], &injected); err != nil || len(injected) != len(got) {
		t.Fatalf("injected JSON: err=%v, %d tracks", err, len(injected))
	}
}

func TestTracksRejectBadContributions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"id differs from filename", func(m map[string]any) { m["id"] = "sale" }, "must equal the filename"},
		{"missing label", func(m map[string]any) { m["label"] = "" }, "label is required"},
		{"empty plan", func(m map[string]any) { m["plan"] = []any{} }, "at least one week"},
		{"empty schedule", func(m map[string]any) { m["schedule"] = []any{} }, "at least one block"},
		{"focus count differs from labels", func(m map[string]any) { m["focusLabels"] = []string{"A", "B"} }, "focus lines"},
		{"week numbered wrong", func(m map[string]any) { m["plan"].([]map[string]any)[0]["n"] = 2 }, "plan[0].n must be 1"},
		{"item id reused from another track", func(m map[string]any) {
			m["plan"].([]map[string]any)[0]["items"] = []map[string]string{{"id": "w1-resume", "t": "dup"}}
		}, "already used by track engineering"},
		{"item without text", func(m map[string]any) {
			m["plan"].([]map[string]any)[0]["items"] = []map[string]string{{"id": "s1-x", "t": ""}}
		}, "without a valid id and text"},
	}
	for _, c := range cases {
		fsys := embeddedTracksFS(t)
		m := salesTrack()
		c.mutate(m)
		fsys["tracks/sales.json"] = &fstest.MapFile{Data: mustJSON(t, m)}
		_, err := loadTracks(fsys, "tracks")
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "tracks/sales.json") {
			t.Errorf("%s: err = %v, want it to name tracks/sales.json and say %q", c.name, err, c.want)
		}
	}
	if _, err := loadTracks(fstest.MapFS{"tracks/x.json": &fstest.MapFile{Data: []byte("{not json")}}, "tracks"); err == nil || !strings.Contains(err.Error(), "tracks/x.json") {
		t.Errorf("bad json: err = %v", err)
	}
	if _, err := loadTracks(fstest.MapFS{"tracks/readme.txt": &fstest.MapFile{Data: []byte("x")}}, "tracks"); err == nil || !strings.Contains(err.Error(), "no track files") {
		t.Errorf("no files: err = %v", err)
	}
}

func TestContributedTrackIsAcceptedEverywhere(t *testing.T) {
	fsys := embeddedTracksFS(t)
	sales := salesTrack()
	sales["label"] = "Sales</script><script>alert(1)</script>" // must not break out of the data script tag
	fsys["tracks/sales.json"] = &fstest.MapFile{Data: mustJSON(t, sales)}
	s, err := newServerFS(filepath.Join(t.TempDir(), "hq.db"), fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	h := s.routes()

	if rec := do(t, h, "GET", "/api/tracks", nil); !bytes.Contains(rec.Body.Bytes(), []byte(`"sales":`)) {
		t.Fatalf("/api/tracks lacks sales: %s", rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/settings", Settings{Track: "sales"}); rec.Code != 204 {
		t.Fatalf("PUT track sales: %d %s", rec.Code, rec.Body)
	}
	if st := getState(t, h); st.Settings.Track != "sales" {
		t.Fatalf("track = %q", st.Settings.Track)
	}
	if rec := do(t, h, "POST", "/api/import", State{Settings: Settings{Track: "sales"}}); rec.Code != 200 {
		t.Fatalf("import track sales: %d %s", rec.Code, rec.Body)
	}
	page := do(t, h, "GET", "/", nil).Body.Bytes()
	if !bytes.Contains(page, []byte(`"id":"sales"`)) {
		t.Fatal("page data lacks the sales track")
	}
	if bytes.Count(page, []byte("</script>")) != bytes.Count(indexHTML, []byte("</script>")) {
		t.Fatal("track text broke out of the data script tag")
	}
}
