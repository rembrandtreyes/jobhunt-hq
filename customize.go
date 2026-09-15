package main

// Per-user customization. Each piece is one row in the settings table,
// stored as canonical JSON under its key:
//
//	schedule  {"weekday"|"mon".."sun": [{t, d, w, h, c}]}   the day, block by block
//	focus     {"labels": [..], "weeks": {"1": [..], ..}}   the week focus labels and lines
//
// On PUT /api/settings and POST /api/import the field is optional: absent
// leaves the row alone, JSON null deletes it (the page's "Reset to track
// default"), anything else must validate. The page decides what to show:
// its own blocks when a row exists, the track's otherwise.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// block is one row of the schedule card. Field names match the track files.
type block struct {
	T string `json:"t"`           // time label: "8:30", or "—"
	D string `json:"d"`           // length: "30 min"
	W string `json:"w"`           // title, required
	H string `json:"h"`           // what to do
	C string `json:"c,omitempty"` // "", deep, apply, rest — the marker style
}

var (
	scheduleDays = map[string]bool{"weekday": true, "mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true}
	blockStyles  = map[string]bool{"": true, "deep": true, "apply": true, "rest": true}
)

const (
	maxCustomBytes = 32 << 10
	maxBlocks      = 40
	maxTimeRunes   = 16
	maxLengthRunes = 24
	maxTitleRunes  = 80
	maxNoteRunes   = 300
)

// isNull reports whether a raw value is the JSON literal null.
func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func tooLong(s string, max int) bool { return utf8.RuneCountInString(s) > max }

// validateSchedule checks a user schedule and returns it in canonical form
// (sorted keys, no whitespace), which is what gets stored and served.
func validateSchedule(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > maxCustomBytes {
		return nil, fmt.Errorf("schedule: too large")
	}
	var days map[string][]block
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&days); err != nil {
		return nil, fmt.Errorf("schedule: %v", err)
	}
	if len(days) == 0 {
		return nil, fmt.Errorf("schedule: no days; send null to go back to the track default")
	}
	for day, blocks := range days {
		if !scheduleDays[day] {
			return nil, fmt.Errorf("schedule: unknown day %q (use weekday, mon, tue, wed, thu, fri, sat, sun)", day)
		}
		if len(blocks) > maxBlocks {
			return nil, fmt.Errorf("schedule: %s has more than %d blocks", day, maxBlocks)
		}
		for i, b := range blocks {
			switch {
			case b.W == "":
				return nil, fmt.Errorf("schedule: %s block %d needs a title", day, i+1)
			case tooLong(b.T, maxTimeRunes) || tooLong(b.D, maxLengthRunes) || tooLong(b.W, maxTitleRunes) || tooLong(b.H, maxNoteRunes):
				return nil, fmt.Errorf("schedule: %s block %d is too long (time ≤ %d, length ≤ %d, title ≤ %d, note ≤ %d characters)", day, i+1, maxTimeRunes, maxLengthRunes, maxTitleRunes, maxNoteRunes)
			case !blockStyles[b.C]:
				return nil, fmt.Errorf("schedule: %s block %d has unknown style %q", day, i+1, b.C)
			}
		}
		if blocks == nil {
			days[day] = []block{}
		}
	}
	return json.Marshal(days)
}

// focus is the user's week-focus override: new labels for the focus rows
// and/or the lines for particular weeks. Missing parts come from the track.
type focus struct {
	Labels []string            `json:"labels,omitempty"`
	Weeks  map[string][]string `json:"weeks,omitempty"` // "1".."12" → one line per label
}

const (
	maxFocusLabels = 6
	maxLabelRunes  = 40
	maxFocusWeeks  = 12
	maxLineRunes   = 300
)

// validateFocus checks a user focus and returns it in canonical form.
func validateFocus(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > maxCustomBytes {
		return nil, fmt.Errorf("focus: too large")
	}
	var f focus
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("focus: %v", err)
	}
	if len(f.Labels) == 0 && len(f.Weeks) == 0 {
		return nil, fmt.Errorf("focus: nothing to save; send null to go back to the track default")
	}
	if len(f.Labels) > maxFocusLabels {
		return nil, fmt.Errorf("focus: more than %d labels", maxFocusLabels)
	}
	for i, l := range f.Labels {
		if l == "" || tooLong(l, maxLabelRunes) {
			return nil, fmt.Errorf("focus: label %d must be 1–%d characters", i+1, maxLabelRunes)
		}
	}
	for week, lines := range f.Weeks {
		if n, err := strconv.Atoi(week); err != nil || n < 1 || n > maxFocusWeeks || strconv.Itoa(n) != week {
			return nil, fmt.Errorf("focus: week %q must be 1 to %d", week, maxFocusWeeks)
		}
		if len(lines) > maxFocusLabels {
			return nil, fmt.Errorf("focus: week %s has more than %d lines", week, maxFocusLabels)
		}
		for i, l := range lines {
			if tooLong(l, maxLineRunes) {
				return nil, fmt.Errorf("focus: week %s line %d is too long (≤ %d characters)", week, i+1, maxLineRunes)
			}
		}
		if lines == nil {
			f.Weeks[week] = []string{}
		}
	}
	return json.Marshal(f)
}

// customWrite is one validated settings change: clear the key, or set it.
type customWrite struct {
	key   string
	clear bool
	value json.RawMessage
}

// validateCustom checks the optional customization fields of a settings
// body and returns the writes to apply. It runs before any transaction so a
// bad field rejects the whole request.
func validateCustom(in Settings) ([]customWrite, error) {
	fields := []struct {
		key      string
		raw      json.RawMessage
		validate func(json.RawMessage) (json.RawMessage, error)
	}{
		{"schedule", in.Schedule, validateSchedule},
		{"focus", in.Focus, validateFocus},
	}
	var writes []customWrite
	for _, f := range fields {
		if len(f.raw) == 0 {
			continue
		}
		if isNull(f.raw) {
			writes = append(writes, customWrite{key: f.key, clear: true})
			continue
		}
		v, err := f.validate(f.raw)
		if err != nil {
			return nil, err
		}
		writes = append(writes, customWrite{key: f.key, value: v})
	}
	return writes, nil
}

func applyCustom(tx *sql.Tx, writes []customWrite) error {
	for _, w := range writes {
		var err error
		if w.clear {
			_, err = tx.Exec(`DELETE FROM settings WHERE key = ?`, w.key)
		} else {
			_, err = tx.Exec(`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, w.key, string(w.value))
		}
		if err != nil {
			return err
		}
	}
	return nil
}
