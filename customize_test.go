package main

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

var sampleSchedule = map[string][]block{
	"weekday": {
		{T: "8:30", D: "30 min", W: "Plan the day", H: "Three targets, then go."},
		{T: "9:00", D: "2 hrs", W: "Portfolio work", H: "One piece, start to finish.", C: "deep"},
		{T: "12:00", D: "1 hr", W: "Lunch", H: "Away from the desk.", C: "rest"},
	},
	"sat": {{T: "—", D: "", W: "Rest", H: "Nothing. Really."}},
}

// rawSettings sends a settings body as a map so a test can put anything,
// including null, under a key.
func rawSettings(extra map[string]any) map[string]any {
	m := map[string]any{"weeklyGoal": 20}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func stateHasKey(t *testing.T, h http.Handler, key string) bool {
	t.Helper()
	rec := do(t, h, "GET", "/api/state", nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw["settings"], &settings); err != nil {
		t.Fatal(err)
	}
	_, ok := settings[key]
	return ok
}

func TestScheduleSettingStoreClearUntouched(t *testing.T) {
	_, h := newTestServer(t)
	if stateHasKey(t, h, "schedule") {
		t.Fatal("fresh db exposes a schedule")
	}
	if rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"schedule": sampleSchedule})); rec.Code != 204 {
		t.Fatalf("PUT schedule: %d %s", rec.Code, rec.Body)
	}
	var got map[string][]block
	if err := json.Unmarshal(getState(t, h).Settings.Schedule, &got); err != nil || !reflect.DeepEqual(got, sampleSchedule) {
		t.Fatalf("schedule after PUT = %v (err %v)", got, err)
	}
	// A settings write without the field leaves it alone.
	if rec := do(t, h, "PUT", "/api/settings", Settings{Track: "engineering", WeeklyGoal: 7}); rec.Code != 204 {
		t.Fatalf("PUT other settings: %d %s", rec.Code, rec.Body)
	}
	st := getState(t, h)
	if st.Settings.Track != "engineering" || st.Settings.WeeklyGoal != 7 || len(st.Settings.Schedule) == 0 {
		t.Fatalf("schedule lost on unrelated write: %+v", st.Settings)
	}
	// null clears.
	if rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"schedule": nil})); rec.Code != 204 {
		t.Fatalf("PUT schedule null: %d %s", rec.Code, rec.Body)
	}
	if stateHasKey(t, h, "schedule") {
		t.Fatal("schedule still present after null")
	}
	if st := getState(t, h); st.Settings.Track != "engineering" {
		t.Fatalf("track changed by the clear: %+v", st.Settings)
	}
}

func TestScheduleValidation(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"schedule": sampleSchedule}))
	before := getState(t, h).Settings.Schedule

	long := strings.Repeat("x", 301)
	bad := []struct {
		name string
		body any
		want string
	}{
		{"unknown day", map[string]any{"monday": []block{{W: "x"}}}, "unknown day"},
		{"no title", map[string]any{"mon": []block{{T: "9:00"}}}, "needs a title"},
		{"note too long", map[string]any{"mon": []block{{W: "x", H: long}}}, "too long"},
		{"title too long", map[string]any{"mon": []block{{W: strings.Repeat("t", 81)}}}, "too long"},
		{"too many blocks", map[string]any{"mon": make([]block, 41)}, "more than 40"},
		{"unknown style", map[string]any{"mon": []block{{W: "x", C: "bold"}}}, "unknown style"},
		{"unknown block field", map[string]any{"mon": []map[string]any{{"w": "x", "color": "red"}}}, "unknown field"},
		{"not an object", []block{{W: "x"}}, "schedule:"},
		{"empty object", map[string]any{}, "no days"},
	}
	for _, c := range bad {
		rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"schedule": c.body}))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s (want 400 containing %q)", c.name, rec.Code, rec.Body, c.want)
		}
	}
	if after := getState(t, h).Settings.Schedule; !bytes.Equal(before, after) {
		t.Fatalf("a rejected schedule changed the stored one:\n%s\n%s", before, after)
	}
	// The import path applies the same rules, prefixed for the caller.
	rec := do(t, h, "POST", "/api/import", map[string]any{"settings": map[string]any{"schedule": map[string]any{"monday": []block{{W: "x"}}}}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "settings.schedule") {
		t.Fatalf("import bad schedule: %d %s", rec.Code, rec.Body)
	}
}

func TestScheduleSurvivesExportImport(t *testing.T) {
	_, h1 := newTestServer(t)
	do(t, h1, "PUT", "/api/settings", rawSettings(map[string]any{"schedule": sampleSchedule}))
	exp := do(t, h1, "GET", "/api/export", nil)
	_, h2 := newTestServer(t)
	if rec := do(t, h2, "POST", "/api/import", json.RawMessage(exp.Body.Bytes())); rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if a, b := getState(t, h1).Settings.Schedule, getState(t, h2).Settings.Schedule; !bytes.Equal(a, b) {
		t.Fatalf("schedule differs after round trip:\n%s\n%s", a, b)
	}
	// Import with null clears, like PUT.
	if rec := do(t, h2, "POST", "/api/import", map[string]any{"settings": map[string]any{"schedule": nil}}); rec.Code != 200 {
		t.Fatalf("import null: %d %s", rec.Code, rec.Body)
	}
	if stateHasKey(t, h2, "schedule") {
		t.Fatal("import null did not clear the schedule")
	}
}

// ---------- properties ----------

const textAlphabet = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 —–:|<>&\"'/#.,"

func randText(r *rand.Rand, max int) string {
	runes := []rune(textAlphabet)
	n := r.Intn(max + 1)
	out := make([]rune, n)
	for i := range out {
		out[i] = runes[r.Intn(len(runes))]
	}
	return string(out)
}

// validSchedule generates schedules inside every cap, so the property
// covers the whole accepted domain rather than one sample.
type validSchedule map[string][]block

func (validSchedule) Generate(r *rand.Rand, _ int) reflect.Value {
	days := []string{"weekday", "mon", "tue", "wed", "thu", "fri", "sat", "sun"}
	styles := []string{"", "deep", "apply", "rest"}
	out := validSchedule{}
	for len(out) == 0 {
		for _, d := range days {
			if r.Intn(3) != 0 {
				continue
			}
			n := r.Intn(6)
			blocks := make([]block, n)
			for i := range blocks {
				blocks[i] = block{T: randText(r, maxTimeRunes), D: randText(r, maxLengthRunes), W: "t" + randText(r, maxTitleRunes-1), H: randText(r, maxNoteRunes), C: styles[r.Intn(len(styles))]}
			}
			out[d] = blocks
		}
	}
	return reflect.ValueOf(out)
}

func TestScheduleProperty_AcceptedAndCanonical(t *testing.T) {
	_, h := newTestServer(t)
	prop := func(s validSchedule) bool {
		raw, _ := json.Marshal(s)
		canon, err := validateSchedule(raw)
		if err != nil {
			t.Logf("rejected valid schedule: %v", err)
			return false
		}
		again, err := validateSchedule(canon)
		if err != nil || !bytes.Equal(canon, again) {
			t.Logf("not idempotent: %v", err)
			return false
		}
		if rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"schedule": s})); rec.Code != 204 {
			t.Logf("PUT: %d %s", rec.Code, rec.Body)
			return false
		}
		var got map[string][]block
		if err := json.Unmarshal(getState(t, h).Settings.Schedule, &got); err != nil {
			return false
		}
		return reflect.DeepEqual(got, map[string][]block(s))
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// ---------- focus ----------

var sampleFocus = focus{
	Labels: []string{"Pipeline", "Story", "Practice"},
	Weeks:  map[string][]string{"1": {"Twenty target accounts", "The 90-second intro", "Record three answers"}, "3": {}},
}

func TestFocusSettingStoreClearUntouched(t *testing.T) {
	_, h := newTestServer(t)
	if stateHasKey(t, h, "focus") {
		t.Fatal("fresh db exposes a focus")
	}
	if rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"focus": sampleFocus})); rec.Code != 204 {
		t.Fatalf("PUT focus: %d %s", rec.Code, rec.Body)
	}
	var got focus
	if err := json.Unmarshal(getState(t, h).Settings.Focus, &got); err != nil || !reflect.DeepEqual(got, sampleFocus) {
		t.Fatalf("focus after PUT = %+v (err %v)", got, err)
	}
	if rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"schedule": sampleSchedule})); rec.Code != 204 {
		t.Fatalf("PUT schedule: %d %s", rec.Code, rec.Body)
	}
	st := getState(t, h)
	if len(st.Settings.Focus) == 0 || len(st.Settings.Schedule) == 0 {
		t.Fatalf("one customization clobbered the other: %+v", st.Settings)
	}
	if rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"focus": nil})); rec.Code != 204 {
		t.Fatalf("PUT focus null: %d %s", rec.Code, rec.Body)
	}
	if stateHasKey(t, h, "focus") || !stateHasKey(t, h, "schedule") {
		t.Fatal("clearing focus did not leave exactly the schedule")
	}
}

func TestFocusValidation(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"focus": sampleFocus}))
	before := getState(t, h).Settings.Focus

	bad := []struct {
		name string
		body any
		want string
	}{
		{"unknown key", map[string]any{"labels": []string{"A"}, "theme": "x"}, "unknown field"},
		{"week 13", map[string]any{"weeks": map[string]any{"13": []string{"x"}}}, "must be 1 to 12"},
		{"week 0", map[string]any{"weeks": map[string]any{"0": []string{"x"}}}, "must be 1 to 12"},
		{"week 01", map[string]any{"weeks": map[string]any{"01": []string{"x"}}}, "must be 1 to 12"},
		{"seven labels", map[string]any{"labels": []string{"a", "b", "c", "d", "e", "f", "g"}}, "more than 6 labels"},
		{"empty label", map[string]any{"labels": []string{""}}, "label 1 must be"},
		{"label too long", map[string]any{"labels": []string{strings.Repeat("l", 41)}}, "label 1 must be"},
		{"line too long", map[string]any{"weeks": map[string]any{"2": []string{strings.Repeat("x", 301)}}}, "too long"},
		{"seven lines", map[string]any{"weeks": map[string]any{"2": []string{"a", "b", "c", "d", "e", "f", "g"}}}, "more than 6 lines"},
		{"week not an array", map[string]any{"weeks": map[string]any{"1": "x"}}, "focus:"},
		{"empty object", map[string]any{}, "nothing to save"},
		{"not an object", []string{"x"}, "focus:"},
	}
	for _, c := range bad {
		rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"focus": c.body}))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s (want 400 containing %q)", c.name, rec.Code, rec.Body, c.want)
		}
	}
	if after := getState(t, h).Settings.Focus; !bytes.Equal(before, after) {
		t.Fatalf("a rejected focus changed the stored one:\n%s\n%s", before, after)
	}
	rec := do(t, h, "POST", "/api/import", map[string]any{"settings": map[string]any{"focus": map[string]any{"weeks": map[string]any{"13": []string{}}}}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "settings.focus") {
		t.Fatalf("import bad focus: %d %s", rec.Code, rec.Body)
	}
}

func TestFocusSurvivesExportImport(t *testing.T) {
	_, h1 := newTestServer(t)
	do(t, h1, "PUT", "/api/settings", rawSettings(map[string]any{"focus": sampleFocus}))
	exp := do(t, h1, "GET", "/api/export", nil)
	_, h2 := newTestServer(t)
	if rec := do(t, h2, "POST", "/api/import", json.RawMessage(exp.Body.Bytes())); rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if a, b := getState(t, h1).Settings.Focus, getState(t, h2).Settings.Focus; !bytes.Equal(a, b) {
		t.Fatalf("focus differs after round trip:\n%s\n%s", a, b)
	}
	if rec := do(t, h2, "POST", "/api/import", map[string]any{"settings": map[string]any{"focus": nil}}); rec.Code != 200 {
		t.Fatalf("import null: %d %s", rec.Code, rec.Body)
	}
	if stateHasKey(t, h2, "focus") {
		t.Fatal("import null did not clear the focus")
	}
}

// validFocus generates focus overrides inside every cap.
type validFocus focus

func (validFocus) Generate(r *rand.Rand, _ int) reflect.Value {
	var f validFocus
	for len(f.Labels) == 0 && len(f.Weeks) == 0 {
		if n := r.Intn(maxFocusLabels + 1); n > 0 {
			f.Labels = make([]string, n)
			for i := range f.Labels {
				f.Labels[i] = "L" + randText(r, maxLabelRunes-1)
			}
		}
		for w := 1; w <= maxFocusWeeks; w++ {
			if r.Intn(4) != 0 {
				continue
			}
			if f.Weeks == nil {
				f.Weeks = map[string][]string{}
			}
			lines := make([]string, r.Intn(maxFocusLabels+1))
			for i := range lines {
				lines[i] = randText(r, maxLineRunes)
			}
			f.Weeks[strconv.Itoa(w)] = lines
		}
	}
	return reflect.ValueOf(f)
}

func TestFocusProperty_AcceptedAndCanonical(t *testing.T) {
	_, h := newTestServer(t)
	prop := func(f validFocus) bool {
		raw, _ := json.Marshal(f)
		canon, err := validateFocus(raw)
		if err != nil {
			t.Logf("rejected valid focus: %v", err)
			return false
		}
		again, err := validateFocus(canon)
		if err != nil || !bytes.Equal(canon, again) {
			t.Logf("not idempotent: %v", err)
			return false
		}
		if rec := do(t, h, "PUT", "/api/settings", rawSettings(map[string]any{"focus": f})); rec.Code != 204 {
			t.Logf("PUT: %d %s", rec.Code, rec.Body)
			return false
		}
		var got focus
		if err := json.Unmarshal(getState(t, h).Settings.Focus, &got); err != nil {
			return false
		}
		return reflect.DeepEqual(got, focus(f))
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}
