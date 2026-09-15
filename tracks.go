package main

// Study tracks live in tracks/*.json — one file per track — and are compiled
// into the binary. The server validates them at startup, serves them at
// GET /api/tracks, and injects them into the page so it renders without a
// second request. To add a track, add a file; see AGENTS.md.

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed tracks/*.json
var trackFiles embed.FS

// trackSet is the loaded tracks: the raw JSON per id (served and injected
// as-is, so a field the server does not know about still reaches the page)
// plus the ids the settings API accepts.
type trackSet struct {
	ids  []string
	raw  map[string]json.RawMessage
	json []byte // {"engineering": {...}, "general": {...}}
}

func (t *trackSet) has(id string) bool { _, ok := t.raw[id]; return ok }

// trackShape is the part of a track file the server checks. The page reads
// more (schedule, afternoon, weekend, resources, groups, sub, weighting,
// rulesTitle, rules); those are passed through untouched.
type trackShape struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	FocusLabels []string `json:"focusLabels"`
	Schedule    []struct {
		W string `json:"w"`
	} `json:"schedule"`
	Plan []struct {
		N     int      `json:"n"`
		Theme string   `json:"theme"`
		Focus []string `json:"focus"`
		Items []struct {
			ID string `json:"id"`
			T  string `json:"t"`
		} `json:"items"`
	} `json:"plan"`
}

// loadTracks reads every *.json in dir, validates each file, and rejects the
// whole set on the first problem so a bad contribution fails loudly at
// startup (and in `go test`) instead of rendering a broken page.
func loadTracks(fsys fs.FS, dir string) (*trackSet, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	set := &trackSet{raw: map[string]json.RawMessage{}}
	owner := map[string]string{} // study item id -> track id, ids must be unique across tracks
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, err
		}
		var t trackShape
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("tracks/%s: %w", name, err)
		}
		stem := strings.TrimSuffix(name, ".json")
		switch {
		case t.ID != stem:
			return nil, fmt.Errorf("tracks/%s: id %q must equal the filename", name, t.ID)
		case !idRe.MatchString(t.ID):
			return nil, fmt.Errorf("tracks/%s: id may only contain letters, digits, - and _", name)
		case t.Label == "":
			return nil, fmt.Errorf("tracks/%s: label is required", name)
		case len(t.Plan) == 0:
			return nil, fmt.Errorf("tracks/%s: plan needs at least one week", name)
		case len(t.Schedule) == 0:
			return nil, fmt.Errorf("tracks/%s: schedule needs at least one block", name)
		}
		for i, w := range t.Plan {
			switch {
			case w.N != i+1:
				return nil, fmt.Errorf("tracks/%s: plan[%d].n must be %d", name, i, i+1)
			case w.Theme == "":
				return nil, fmt.Errorf("tracks/%s: week %d needs a theme", name, w.N)
			case len(w.Focus) != len(t.FocusLabels):
				return nil, fmt.Errorf("tracks/%s: week %d has %d focus lines, focusLabels has %d", name, w.N, len(w.Focus), len(t.FocusLabels))
			case len(w.Items) == 0:
				return nil, fmt.Errorf("tracks/%s: week %d needs at least one item", name, w.N)
			}
			for _, it := range w.Items {
				if !idRe.MatchString(it.ID) || it.T == "" {
					return nil, fmt.Errorf("tracks/%s: week %d has an item without a valid id and text", name, w.N)
				}
				if prev, dup := owner[it.ID]; dup {
					return nil, fmt.Errorf("tracks/%s: item id %q is already used by track %s (ids are progress keys and must be unique)", name, it.ID, prev)
				}
				owner[it.ID] = t.ID
			}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, b); err != nil {
			return nil, fmt.Errorf("tracks/%s: %w", name, err)
		}
		set.raw[t.ID] = json.RawMessage(compact.Bytes())
		set.ids = append(set.ids, t.ID)
	}
	if len(set.ids) == 0 {
		return nil, fmt.Errorf("tracks/: no track files found")
	}
	// Marshal escapes <, > and & as < etc., so the result is safe inside
	// the page's <script type="application/json"> tag as well as on the API.
	set.json, err = json.Marshal(set.raw)
	if err != nil {
		return nil, err
	}
	return set, nil
}

const tracksPlaceholder = "__TRACKS__"

// renderPage puts the tracks JSON into the page's data script tag.
func renderPage(page []byte, tracks *trackSet) []byte {
	return bytes.Replace(page, []byte(tracksPlaceholder), tracks.json, 1)
}
