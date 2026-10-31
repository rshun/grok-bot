package schedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var loc = time.FixedZone("CST", 8*3600)

func at(day, hour, minute, second int) time.Time {
	return time.Date(2026, 10, day, hour, minute, second, 0, loc)
}

func TestNextDaily(t *testing.T) {
	s := Spec{Kind: "daily", Time: "09:00"}
	if got, ok := s.Next(at(19, 9, 0, 10), loc); !ok || !got.Equal(at(20, 9, 0, 0)) {
		t.Fatalf("after 09:00:10 got %v %v", got, ok)
	}
	if got, ok := s.Next(at(19, 8, 59, 0), loc); !ok || !got.Equal(at(19, 9, 0, 0)) {
		t.Fatalf("before 09:00 got %v %v", got, ok)
	}
	if got, _ := s.Next(at(19, 9, 0, 0), loc); !got.Equal(at(20, 9, 0, 0)) {
		t.Fatalf("exactly 09:00 must move on, got %v", got)
	}
}

func TestNextWeekly(t *testing.T) {
	monday := at(19, 10, 0, 0)
	if monday.Weekday() != time.Monday {
		t.Fatalf("fixture is %v", monday.Weekday())
	}
	s := Spec{Kind: "weekly", Time: "09:00", Weekdays: []int{1}}
	if got, ok := s.Next(monday, loc); !ok || !got.Equal(at(26, 9, 0, 0)) {
		t.Fatalf("got %v %v", got, ok)
	}
	s.Weekdays = []int{3, 5}
	if got, _ := s.Next(monday, loc); !got.Equal(at(21, 9, 0, 0)) {
		t.Fatalf("got %v", got)
	}
}

func TestNextInterval(t *testing.T) {
	s := Spec{Kind: "interval", EveryMinutes: 90}
	start := at(19, 10, 0, 0)
	if got, ok := s.Next(start, loc); !ok || !got.Equal(start.Add(90*time.Minute)) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestNextOnce(t *testing.T) {
	s := Spec{Kind: "once", At: "2026-10-20 15:00"}
	if got, ok := s.Next(at(19, 10, 0, 0), loc); !ok || !got.Equal(at(20, 15, 0, 0)) {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := s.Next(at(21, 10, 0, 0), loc); ok {
		t.Fatal("past one-off task still has a next run")
	}
}

func TestNextDatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dates.txt")
	content := "# custom schedule\n\n20261025\r\n2026-10-20\n20261018\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Spec{Kind: "dates", Time: "09:30", DateFile: path}
	if err := s.Validate(at(19, 10, 0, 0), loc, time.Hour); err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	got, ok := s.Next(at(19, 10, 0, 0), loc)
	if !ok || !got.Equal(at(20, 9, 30, 0)) {
		t.Fatalf("first next got %v %v, want 2026-10-20 09:30", got, ok)
	}
	got, ok = s.Next(at(20, 9, 30, 0), loc)
	if !ok || !got.Equal(at(25, 9, 30, 0)) {
		t.Fatalf("second next got %v %v, want 2026-10-25 09:30", got, ok)
	}
	if _, ok := s.Next(at(25, 9, 30, 0), loc); ok {
		t.Fatal("expected no more runs after last date in file")
	}
}

func TestValidateDatesFileErrors(t *testing.T) {
	dir := t.TempDir()
	now := at(19, 10, 0, 0)

	badFormat := filepath.Join(dir, "bad.txt")
	_ = os.WriteFile(badFormat, []byte("2026/10/20\n"), 0o600)

	pastOnly := filepath.Join(dir, "past.txt")
	_ = os.WriteFile(pastOnly, []byte("20261018\n"), 0o600)

	emptyFile := filepath.Join(dir, "empty.txt")
	_ = os.WriteFile(emptyFile, []byte("# only comments\n\n"), 0o600)

	cases := map[string]Spec{
		"missing path":  {Kind: "dates", Time: "09:00"},
		"relative path": {Kind: "dates", Time: "09:00", DateFile: "dates.txt"},
		"not found":     {Kind: "dates", Time: "09:00", DateFile: filepath.Join(dir, "missing.txt")},
		"bad line":      {Kind: "dates", Time: "09:00", DateFile: badFormat},
		"past only":     {Kind: "dates", Time: "09:00", DateFile: pastOnly},
		"empty":         {Kind: "dates", Time: "09:00", DateFile: emptyFile},
	}
	for name, s := range cases {
		if err := s.Validate(now, loc, time.Hour); err == nil {
			t.Errorf("%s: expected error for %#v", name, s)
		}
	}
}

func TestValidate(t *testing.T) {
	now := at(19, 10, 0, 0)
	bad := map[string]Spec{
		"unknown kind":    {Kind: "monthly", Time: "09:00"},
		"bad time":        {Kind: "daily", Time: "25:00"},
		"no weekdays":     {Kind: "weekly", Time: "09:00"},
		"weekday 7":       {Kind: "weekly", Time: "09:00", Weekdays: []int{7}},
		"short interval":  {Kind: "interval", EveryMinutes: 30},
		"past once":       {Kind: "once", At: "2026-10-18 09:00"},
		"malformed once":  {Kind: "once", At: "tomorrow"},
		"zero interval":   {Kind: "interval"},
		"negative minute": {Kind: "daily", Time: "09:-1"},
	}
	for name, s := range bad {
		if err := s.Validate(now, loc, time.Hour); err == nil {
			t.Errorf("%s: accepted %#v", name, s)
		}
	}
	good := []Spec{
		{Kind: "daily", Time: "09:00"},
		{Kind: "weekly", Time: "23:59", Weekdays: []int{0, 6}},
		{Kind: "interval", EveryMinutes: 60},
		{Kind: "once", At: "2026-10-19 10:30"},
	}
	for _, s := range good {
		if err := s.Validate(now, loc, time.Hour); err != nil {
			t.Errorf("rejected %#v: %v", s, err)
		}
	}
}

func TestDecode(t *testing.T) {
	structured := json.RawMessage(`{"kind":"daily","time":"09:00","prompt":"看新闻","summary":"每天 09:00：看新闻"}`)
	got, err := Decode(structured, "ignored")
	if err != nil || got.Kind != "daily" || got.Time != "09:00" || got.Prompt != "看新闻" || got.Summary != "每天 09:00：看新闻" {
		t.Fatalf("%#v %v", got, err)
	}
	fenced := "```json\n{\"kind\":\"interval\",\"every_minutes\":120,\"prompt\":\"p\",\"summary\":\"s\"}\n```"
	got, err = Decode(nil, fenced)
	if err != nil || got.Kind != "interval" || got.EveryMinutes != 120 {
		t.Fatalf("%#v %v", got, err)
	}
	if _, err := Decode(nil, "not json"); err == nil {
		t.Fatal("expected error")
	}
	got, err = Decode(json.RawMessage(`{"error":"没有说时间"}`), "")
	if err != nil || got.Error != "没有说时间" {
		t.Fatalf("%#v %v", got, err)
	}
	got, err = Decode(json.RawMessage(`{"kind":"daily","date_file":"/srv/dates.txt","prompt":"p","summary":"s"}`), "")
	if err != nil || got.Kind != "dates" || got.Time != "09:00" || got.DateFile != "/srv/dates.txt" {
		t.Fatalf("date_file decode: %#v %v", got, err)
	}
}

func TestPromptContainsNowAndZone(t *testing.T) {
	p := Prompt("每天九点看新闻", time.Date(2026, 10, 19, 2, 30, 0, 0, time.UTC), loc)
	for _, want := range []string{"2026-10-19 10:30", "CST", "每天九点看新闻"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in %s", want, p)
		}
	}
	if !json.Valid([]byte(Schema)) {
		t.Fatal("Schema is not valid JSON")
	}
}
