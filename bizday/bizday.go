// Package bizday is the business-day clock an acceptance window runs on. A
// Calendar names the zone the client counts days in, the weekdays that are
// weekend, the holidays the contract lists and the local time a business day
// closes; its canonical JSON hashes to the fingerprint contract.opened
// carries, so a replay can tell whether a deadline was computed on the
// calendar the parties opened on. Deadline turns a delivery into one UTC
// instant: the close of the n-th business day after it, where a delivery past
// close counts from the next business day and weekends and listed holidays are
// skipped. The zoneinfo database is embedded, so the same journal reads the
// same deadline on any host.
package bizday

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/polycratia/engageledger/canon"
)

// DefaultClose is the local time a business day ends when a calendar names
// none.
const DefaultClose = "17:00"

const (
	dateLayout  = "2006-01-02"
	closeLayout = "15:04"
	maxSearch   = 3650
)

var weekdayNames = [7]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// Calendar is the business-day calendar a contract opens on. Holidays are
// YYYY-MM-DD dates in Zone and Close is the local time a business day ends.
type Calendar struct {
	Version  string
	Zone     string
	Weekend  []time.Weekday
	Holidays []string
	Close    string
}

// New returns the calendar for version, zone, the weekend weekdays, the
// holidays as YYYY-MM-DD dates in that zone and the local time a business day
// closes. A nil weekend reads as Saturday and Sunday and an empty closeAt as
// DefaultClose; the weekend and the holidays are sorted, so two calendars that
// mean the same thing hash the same.
func New(version, zone string, weekend []time.Weekday, holidays []string, closeAt string) (Calendar, error) {
	if version == "" {
		return Calendar{}, errors.New("bizday: a calendar must name its version")
	}
	c := Calendar{Version: version, Zone: zone, Close: closeAt}
	if c.Close == "" {
		c.Close = DefaultClose
	}
	if _, err := c.location(); err != nil {
		return Calendar{}, err
	}
	if _, _, err := parseClose(c.Close); err != nil {
		return Calendar{}, err
	}
	days, err := normalizeWeekend(weekend)
	if err != nil {
		return Calendar{}, err
	}
	dates, err := normalizeHolidays(holidays)
	if err != nil {
		return Calendar{}, err
	}
	c.Weekend, c.Holidays = days, dates
	return c, nil
}

// Object returns the calendar as the object a contract.opened body carries it
// as.
func (c Calendar) Object() map[string]any {
	weekend := make([]any, 0, len(c.Weekend))
	for _, d := range c.Weekend {
		weekend = append(weekend, weekdayNames[d])
	}
	holidays := make([]any, 0, len(c.Holidays))
	for _, h := range c.Holidays {
		holidays = append(holidays, h)
	}
	return map[string]any{
		"version":  c.Version,
		"zone":     c.Zone,
		"weekend":  weekend,
		"holidays": holidays,
		"close":    c.closeAt(),
	}
}

// Hash is the hex SHA-256 of the canonical JSON of the calendar, which
// contract.opened stores and every deadline names.
func (c Calendar) Hash() (string, error) {
	b, err := canon.Marshal(c.Object())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Deadline is the UTC instant the acceptance window of a delivery at
// deliveredAt runs out: the close of the n-th business day after it, counted
// in the zone of the calendar. A delivery past close, on a weekend or on a
// listed holiday counts from the next business day, which n then counts from;
// a delivery exactly at close still counts from its own day.
func (c Calendar) Deadline(deliveredAt time.Time, n int64) (time.Time, error) {
	if n < 0 {
		return time.Time{}, fmt.Errorf("bizday: a window of %d business days has no deadline", n)
	}
	loc, err := c.location()
	if err != nil {
		return time.Time{}, err
	}
	hour, minute, err := parseClose(c.closeAt())
	if err != nil {
		return time.Time{}, err
	}
	local := deliveredAt.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	closeOf := func(d time.Time) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, loc)
	}
	steps := 0
	advance := func() error {
		day = day.AddDate(0, 0, 1)
		steps++
		if steps > maxSearch {
			return fmt.Errorf("bizday: the calendar names no business day in the %d days after %s", maxSearch, local.Format(dateLayout))
		}
		return nil
	}
	for !c.business(day) || deliveredAt.After(closeOf(day)) {
		if err := advance(); err != nil {
			return time.Time{}, err
		}
	}
	for i := int64(0); i < n; i++ {
		for {
			if err := advance(); err != nil {
				return time.Time{}, err
			}
			if c.business(day) {
				break
			}
		}
	}
	return closeOf(day).UTC(), nil
}

// Read reads the calendar object a contract.opened body carries. A calendar
// that names no weekend, no holidays or no close reads as Saturday and Sunday,
// no holidays and DefaultClose.
func Read(body map[string]any) (Calendar, error) {
	obj, ok := body["calendar"].(map[string]any)
	if !ok {
		return Calendar{}, errors.New("has no calendar object")
	}
	c, err := read(obj)
	if err != nil {
		return Calendar{}, fmt.Errorf("carries a calendar that %w", err)
	}
	return c, nil
}

func read(obj map[string]any) (Calendar, error) {
	for key := range obj {
		switch key {
		case "version", "zone", "weekend", "holidays", "close":
		default:
			return Calendar{}, fmt.Errorf("carries a key beyond version, zone, weekend, holidays and close: %q", key)
		}
	}
	version, err := text(obj, "version")
	if err != nil {
		return Calendar{}, err
	}
	zone, err := text(obj, "zone")
	if err != nil {
		return Calendar{}, err
	}
	closeAt := DefaultClose
	if _, ok := obj["close"]; ok {
		if closeAt, err = text(obj, "close"); err != nil {
			return Calendar{}, err
		}
	}
	var weekend []time.Weekday
	if raw, ok := obj["weekend"]; ok {
		if weekend, err = readWeekend(raw); err != nil {
			return Calendar{}, err
		}
	}
	var holidays []string
	if raw, ok := obj["holidays"]; ok {
		if holidays, err = readHolidays(raw); err != nil {
			return Calendar{}, err
		}
	}
	c, err := New(version, zone, weekend, holidays, closeAt)
	if err != nil {
		return Calendar{}, errors.New(strings.TrimPrefix(err.Error(), "bizday: "))
	}
	return c, nil
}

func readWeekend(v any) ([]time.Weekday, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("has a weekend that is not an array")
	}
	out := make([]time.Weekday, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("names a weekend day that is not a string: %#v", item)
		}
		i := slices.Index(weekdayNames[:], name)
		if i < 0 {
			return nil, fmt.Errorf("names weekend day %q, want a lowercase weekday name", name)
		}
		out = append(out, time.Weekday(i))
	}
	return out, nil
}

func readHolidays(v any) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("has a holidays value that is not an array")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		date, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("lists a holiday that is not a string: %#v", item)
		}
		out = append(out, date)
	}
	return out, nil
}

func (c Calendar) business(day time.Time) bool {
	if slices.Contains(c.Weekend, day.Weekday()) {
		return false
	}
	return !slices.Contains(c.Holidays, day.Format(dateLayout))
}

func (c Calendar) closeAt() string {
	if c.Close == "" {
		return DefaultClose
	}
	return c.Close
}

func (c Calendar) location() (*time.Location, error) {
	if c.Zone == "" {
		return nil, errors.New("bizday: a calendar must name the zone its business days are counted in")
	}
	if c.Zone == "Local" {
		return nil, fmt.Errorf("bizday: zone %q depends on the host, so a deadline computed in it cannot be replayed", c.Zone)
	}
	loc, err := time.LoadLocation(c.Zone)
	if err != nil {
		return nil, fmt.Errorf("bizday: zone %q is not in the embedded zoneinfo database", c.Zone)
	}
	return loc, nil
}

func normalizeWeekend(weekend []time.Weekday) ([]time.Weekday, error) {
	if weekend == nil {
		return []time.Weekday{time.Sunday, time.Saturday}, nil
	}
	out := make([]time.Weekday, 0, len(weekend))
	for _, d := range weekend {
		if d < time.Sunday || d > time.Saturday {
			return nil, fmt.Errorf("bizday: %d is not a weekday", d)
		}
		if slices.Contains(out, d) {
			return nil, fmt.Errorf("bizday: the weekend repeats %s", weekdayNames[d])
		}
		out = append(out, d)
	}
	if len(out) == 7 {
		return nil, errors.New("bizday: a weekend of all seven days leaves no business day")
	}
	slices.Sort(out)
	return out, nil
}

func normalizeHolidays(holidays []string) ([]string, error) {
	out := make([]string, 0, len(holidays))
	for _, h := range holidays {
		day, err := time.Parse(dateLayout, h)
		if err != nil || day.Format(dateLayout) != h {
			return nil, fmt.Errorf("bizday: holiday %q is not a YYYY-MM-DD date", h)
		}
		if slices.Contains(out, h) {
			return nil, fmt.Errorf("bizday: the calendar repeats holiday %s", h)
		}
		out = append(out, h)
	}
	slices.Sort(out)
	return out, nil
}

func parseClose(closeAt string) (int, int, error) {
	at, err := time.Parse(closeLayout, closeAt)
	if err != nil || at.Format(closeLayout) != closeAt {
		return 0, 0, fmt.Errorf("bizday: close %q is not a 24-hour HH:MM time", closeAt)
	}
	return at.Hour(), at.Minute(), nil
}

func text(m map[string]any, key string) (string, error) {
	s, ok := m[key].(string)
	if !ok {
		return "", fmt.Errorf("has no %s string", key)
	}
	if s == "" {
		return "", fmt.Errorf("has an empty %s", key)
	}
	return s, nil
}
