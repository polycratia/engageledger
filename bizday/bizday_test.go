package bizday

import (
	"strings"
	"testing"
	"time"
)

const (
	newYork = "America/New_York"
	berlin  = "Europe/Berlin"
)

func calendarOf(t *testing.T, zone string, holidays ...string) Calendar {
	t.Helper()
	c, err := New("2026.1", zone, nil, holidays, "17:00")
	if err != nil {
		t.Fatalf("New(%q): %v", zone, err)
	}
	return c
}

// at is a local wall-clock stamp in zone, the way a client reads the moment
// work arrived.
func at(t *testing.T, zone, stamp string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", zone, err)
	}
	when, err := time.ParseInLocation("2006-01-02 15:04", stamp, loc)
	if err != nil {
		t.Fatalf("parsing %q: %v", stamp, err)
	}
	return when
}

func readCalendar(t *testing.T, obj map[string]any) Calendar {
	t.Helper()
	c, err := Read(map[string]any{"calendar": obj})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return c
}

func hashOf(t *testing.T, c Calendar) string {
	t.Helper()
	hash, err := c.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return hash
}

func TestDeadlineIsTheCloseOfTheNthBusinessDay(t *testing.T) {
	cases := []struct {
		name      string
		zone      string
		holidays  []string
		delivered string
		days      int64
		want      string
	}{
		{
			name: "a holiday inside the window is skipped",
			zone: newYork, holidays: []string{"2026-11-26"},
			delivered: "2026-11-24 10:00", days: 3, want: "2026-11-30T22:00:00Z",
		},
		{
			name: "a delivery at close still counts from its own day",
			zone: newYork, delivered: "2026-11-24 17:00", days: 1, want: "2026-11-25T22:00:00Z",
		},
		{
			name: "a delivery at 17:01 counts from the next business day",
			zone: newYork, delivered: "2026-11-24 17:01", days: 1, want: "2026-11-26T22:00:00Z",
		},
		{
			name: "a weekend delivery counts from the Monday",
			zone: newYork, delivered: "2026-03-07 09:00", days: 0, want: "2026-03-09T21:00:00Z",
		},
		{
			name: "the spring-forward week in New York",
			zone: newYork, delivered: "2026-03-06 10:00", days: 3, want: "2026-03-11T21:00:00Z",
		},
		{
			name: "the fall-back week in New York",
			zone: newYork, delivered: "2026-10-30 10:00", days: 1, want: "2026-11-02T22:00:00Z",
		},
		{
			name: "the spring-forward week in Berlin",
			zone: berlin, delivered: "2026-03-27 10:00", days: 1, want: "2026-03-30T15:00:00Z",
		},
		{
			name: "the fall-back week in Berlin",
			zone: berlin, delivered: "2026-10-23 10:00", days: 1, want: "2026-10-26T16:00:00Z",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cal := calendarOf(t, c.zone, c.holidays...)
			got, err := cal.Deadline(at(t, c.zone, c.delivered), c.days)
			if err != nil {
				t.Fatalf("Deadline: %v", err)
			}
			if got.Location() != time.UTC {
				t.Errorf("deadline is in %v, want UTC", got.Location())
			}
			if stamp := got.Format(time.RFC3339); stamp != c.want {
				t.Errorf("deadline = %s, want %s", stamp, c.want)
			}
		})
	}
}

func TestTheSameCloseIsAnotherInstantAcrossATransition(t *testing.T) {
	cal := calendarOf(t, newYork)
	before, err := cal.Deadline(at(t, newYork, "2026-03-05 10:00"), 0)
	if err != nil {
		t.Fatalf("Deadline: %v", err)
	}
	after, err := cal.Deadline(at(t, newYork, "2026-03-06 10:00"), 1)
	if err != nil {
		t.Fatalf("Deadline: %v", err)
	}
	if _, off := before.In(time.UTC), before.Format("15:04"); off != "22:00" {
		t.Errorf("close before the transition = %sZ, want 22:00Z", off)
	}
	if off := after.Format("15:04"); off != "21:00" {
		t.Errorf("close after the transition = %sZ, want 21:00Z", off)
	}
}

func TestHashDoesNotDependOnHowTheCalendarIsWritten(t *testing.T) {
	spelled := readCalendar(t, map[string]any{
		"version":  "2026.1",
		"zone":     berlin,
		"weekend":  []any{"sunday", "saturday"},
		"holidays": []any{"2026-12-25", "2026-05-01"},
		"close":    "17:00",
	})
	terse := readCalendar(t, map[string]any{
		"version":  "2026.1",
		"zone":     berlin,
		"holidays": []any{"2026-05-01", "2026-12-25"},
	})
	if hashOf(t, spelled) != hashOf(t, terse) {
		t.Error("two spellings of one calendar hash apart")
	}
	if spelled.Close != DefaultClose || len(spelled.Weekend) != 2 {
		t.Errorf("calendar = %+v, want a weekend of two days closing at %s", spelled, DefaultClose)
	}
	elsewhere := readCalendar(t, map[string]any{
		"version": "2026.1", "zone": newYork, "holidays": []any{"2026-05-01", "2026-12-25"},
	})
	if hashOf(t, elsewhere) == hashOf(t, terse) {
		t.Error("a calendar in another zone hashes the same")
	}
}

func TestReadRefuses(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"no calendar object", nil, "no calendar object"},
		{"a calendar that is not an object", []any{}, "no calendar object"},
		{"no version", map[string]any{"zone": berlin}, "no version string"},
		{"no zone", map[string]any{"version": "2026.1"}, "no zone string"},
		{"an unknown zone", map[string]any{"version": "2026.1", "zone": "Mars/Olympus"}, "zoneinfo"},
		{"the host zone", map[string]any{"version": "2026.1", "zone": "Local"}, "depends on the host"},
		{"a key beyond the five", map[string]any{"version": "2026.1", "zone": berlin, "note": "x"}, "beyond version"},
		{"a weekend that is not an array", map[string]any{"version": "2026.1", "zone": berlin, "weekend": "saturday"}, "not an array"},
		{"an unknown weekday", map[string]any{"version": "2026.1", "zone": berlin, "weekend": []any{"caturday"}}, "lowercase weekday name"},
		{"a repeated weekend day", map[string]any{"version": "2026.1", "zone": berlin, "weekend": []any{"sunday", "sunday"}}, "repeats sunday"},
		{
			"a weekend of all seven days",
			map[string]any{"version": "2026.1", "zone": berlin, "weekend": []any{
				"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday",
			}},
			"no business day",
		},
		{"a holiday that is not a date", map[string]any{"version": "2026.1", "zone": berlin, "holidays": []any{"May 1"}}, "YYYY-MM-DD"},
		{"a loose holiday date", map[string]any{"version": "2026.1", "zone": berlin, "holidays": []any{"2026-5-1"}}, "YYYY-MM-DD"},
		{"a repeated holiday", map[string]any{"version": "2026.1", "zone": berlin, "holidays": []any{"2026-05-01", "2026-05-01"}}, "repeats holiday"},
		{"a close outside the day", map[string]any{"version": "2026.1", "zone": berlin, "close": "25:00"}, "24-hour HH:MM"},
		{"a close without its leading zero", map[string]any{"version": "2026.1", "zone": berlin, "close": "9:00"}, "24-hour HH:MM"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{}
			if c.in != nil {
				body["calendar"] = c.in
			}
			got, err := Read(body)
			if err == nil {
				t.Fatalf("Read returned %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err, c.want)
			}
		})
	}
}

func TestDeadlineRefuses(t *testing.T) {
	if _, err := calendarOf(t, berlin).Deadline(time.Now(), -1); err == nil {
		t.Error("Deadline accepted a window of less than no days")
	}
	if _, err := (Calendar{Version: "2026.1"}).Deadline(time.Now(), 1); err == nil {
		t.Error("Deadline ran on a calendar that names no zone")
	}
}
