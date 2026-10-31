package acceptance

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/polycratia/engageledger/bizday"
	"github.com/polycratia/engageledger/journal"
)

const (
	zone         = "Europe/Berlin"
	deliveredAt  = "2026-03-02 10:00"
	wantDelivery = "2026-03-02T09:00:00Z"
	wantDeadline = "2026-03-16T16:00:00Z"
)

// cal is the calendar every case counts its business days on: Berlin, a
// Saturday and Sunday weekend, closing at 17:00.
var cal = newCalendar()

func newCalendar(holidays ...string) bizday.Calendar {
	c, err := bizday.New("2026.1", zone, nil, holidays, "17:00")
	if err != nil {
		panic("building the test calendar: " + err.Error())
	}
	return c
}

type step struct {
	typ  string
	body map[string]any
}

func milestone(id, title string, criteria ...string) map[string]any {
	crit := make([]any, 0, len(criteria))
	for _, c := range criteria {
		crit = append(crit, c)
	}
	return map[string]any{"id": id, "title": title, "amount": int64(100000), "criteria": crit}
}

// head is the statement of work every case starts from: m1 with one criterion
// and m2 with two, on the default calendar and under the given acceptance
// config or none when it is nil.
func head(config map[string]any) map[string]any {
	return headOn(cal.Object(), config)
}

// headOn is that statement of work carrying the given calendar value, or none
// when it is nil.
func headOn(calendar any, config map[string]any) map[string]any {
	h := map[string]any{
		"parties":  map[string]any{"client": "lantern-co", "contractor": "oakline-studio"},
		"currency": "EUR",
		"milestones": []any{
			milestone("m1", "Discovery", "c1"),
			milestone("m2", "Build", "c2", "c3"),
		},
	}
	if calendar != nil {
		h["calendar"] = calendar
	}
	if config != nil {
		h["acceptance"] = config
	}
	return h
}

func config(window int64, retest Retest) map[string]any {
	return map[string]any{"window_days": window, "retest_window": string(retest)}
}

func headConfigured(v any) map[string]any {
	h := head(nil)
	h["acceptance"] = v
	return h
}

func fold(t *testing.T, h map[string]any, steps ...step) Acceptance {
	t.Helper()
	var buf bytes.Buffer
	a := journal.NewAppender(&buf)
	if _, err := a.Append(EventOpened, h); err != nil {
		t.Fatalf("appending the contract head: %v", err)
	}
	for i, s := range steps {
		if _, err := a.Append(s.typ, s.body); err != nil {
			t.Fatalf("appending step %d (%s): %v", i, s.typ, err)
		}
	}
	events, err := journal.Replay(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("replaying the built journal: %v", err)
	}
	return Fold(events)
}

// at is a local wall-clock stamp in the calendar zone, the way a client reads
// the moment work arrived.
func at(t *testing.T, stamp string) time.Time {
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

func deliveryOn(t *testing.T, c bizday.Calendar, id, stamp string, days int64) step {
	t.Helper()
	body, err := DeliveryBody(c, id, at(t, stamp), days)
	if err != nil {
		t.Fatalf("DeliveryBody(%q, %q): %v", id, stamp, err)
	}
	return step{EventDelivered, body}
}

func deliver(t *testing.T, id, stamp string, days int64) step {
	t.Helper()
	return deliveryOn(t, cal, id, stamp, days)
}

// bareDelivery names only its milestone, which is all an event refused before
// its deadline is read needs to carry.
func bareDelivery(id string) step {
	return step{EventDelivered, map[string]any{"milestone": id}}
}

func accept(id string) step { return step{EventAccepted, map[string]any{"milestone": id}} }

func reject(id string, defects ...any) step {
	return step{EventRejected, map[string]any{"milestone": id, "defects": append([]any{}, defects...)}}
}

func rejectAfter(id string, days int64, defects ...any) step {
	s := reject(id, defects...)
	s.body["elapsed_days"] = days
	return s
}

func defect(criterion, note string) map[string]any {
	return map[string]any{"criterion": criterion, "note": note}
}

func milestoneOf(t *testing.T, a Acceptance, id string) Milestone {
	t.Helper()
	m, ok := a.Find(id)
	if !ok {
		t.Fatalf("the fold carries no milestone %q", id)
	}
	return m
}

func hashOf(t *testing.T, c bizday.Calendar) string {
	t.Helper()
	hash, err := c.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return hash
}

func TestDeliveryFixesTheDeadlineAndAcceptanceClosesTheWindow(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)), deliver(t, "m2", deliveredAt, 10), accept("m2"))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	if got.WindowDays != 10 || got.RetestWindow != RetestRestart {
		t.Errorf("config = %d days, %q, want 10 and restart", got.WindowDays, got.RetestWindow)
	}
	if got.CalendarHash != hashOf(t, cal) || got.Calendar.Zone != zone {
		t.Errorf("calendar = %+v with hash %q, want the one the head carries", got.Calendar, got.CalendarHash)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusAccepted {
		t.Errorf("status = %q, want accepted", m.Status)
	}
	if len(m.Windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(m.Windows))
	}
	w := m.Windows[0]
	if w.Seq != 2 || w.Days != 10 || !w.Closed || w.ClosedBy != 3 {
		t.Errorf("window = %+v, want a ten-day window opened at seq 2 and closed by seq 3", w)
	}
	if w.Deadline.Location() != time.UTC {
		t.Errorf("the deadline is in %v, want UTC", w.Deadline.Location())
	}
	if stamp := w.Deadline.Format(time.RFC3339); stamp != wantDeadline {
		t.Errorf("deadline = %s, want %s", stamp, wantDeadline)
	}
	if stamp := w.Delivered.Format(time.RFC3339); stamp != wantDelivery {
		t.Errorf("delivered at %s, want %s", stamp, wantDelivery)
	}
	if _, open := m.Open(); open {
		t.Error("the window is still open after an acceptance")
	}
	if m1 := milestoneOf(t, got, "m1"); m1.Status != "" || len(m1.Windows) != 0 {
		t.Errorf("m1 = %+v, want an undelivered milestone", m1)
	}
}

func TestADeliveryThatFixesNoDeadlineIsRefused(t *testing.T) {
	ten := func() map[string]any { return head(config(10, RetestRestart)) }
	good := deliver(t, "m2", deliveredAt, 10)
	copied := func() map[string]any {
		body := make(map[string]any, len(good.body))
		for k, v := range good.body {
			body[k] = v
		}
		return body
	}
	without := func(key string) step {
		body := copied()
		delete(body, key)
		return step{EventDelivered, body}
	}
	changed := func(key string, v any) step {
		body := copied()
		body[key] = v
		return step{EventDelivered, body}
	}
	cases := []struct {
		name string
		h    map[string]any
		ev   step
		want string
	}{
		{"a delivery naming no calendar", ten(), without("calendar_hash"), "no calendar_hash string"},
		{"a delivery fixing no deadline", ten(), without("deadline_utc"), "no deadline_utc string"},
		{"a delivery that does not say when it arrived", ten(), without("at"), "no at string"},
		{
			"a delivery computed on another calendar",
			ten(), deliveryOn(t, newCalendar("2026-03-16"), "m2", deliveredAt, 10), "the contract opened on",
		},
		{"a deadline the calendar does not fix", ten(), changed("deadline_utc", "2026-03-17T16:00:00Z"), "closes at"},
		{"a deadline that is not in UTC", ten(), changed("deadline_utc", "2026-03-16T17:00:00+01:00"), "RFC 3339 UTC instant"},
		{"a deadline that is not an instant", ten(), changed("deadline_utc", "2026-03-16"), "RFC 3339 UTC instant"},
		{"a deadline counted on another term", head(config(5, RetestRestart)), good, "closes at"},
		{"a deadline under a head that opened on no calendar", headOn(nil, nil), good, "no calendar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fold(t, c.h, c.ev)
			if len(got.Refused) != 1 {
				t.Fatalf("the fold refused %v, want exactly the delivery", got.Refused)
			}
			r := got.Refused[0]
			if r.Seq != 2 || r.Type != EventDelivered {
				t.Errorf("refusal = %+v, want the delivery at seq 2", r)
			}
			if !strings.Contains(r.Msg, c.want) {
				t.Errorf("message %q should mention %q", r.Msg, c.want)
			}
			if m := milestoneOf(t, got, "m2"); m.Status != "" || len(m.Windows) != 0 {
				t.Errorf("a refused delivery left milestone %+v", m)
			}
		})
	}
}

func TestDeliverWritesTheDeadlineTheFoldReadsBack(t *testing.T) {
	var buf bytes.Buffer
	a := journal.NewAppender(&buf)
	if _, err := a.Append(EventOpened, head(config(10, RetestRestart))); err != nil {
		t.Fatalf("appending the contract head: %v", err)
	}
	e, err := Deliver(a, cal, "m2", at(t, deliveredAt), 10)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if e.Body["deadline_utc"] != wantDeadline {
		t.Errorf("deadline_utc = %#v, want %q", e.Body["deadline_utc"], wantDeadline)
	}
	if e.Body["calendar_hash"] != hashOf(t, cal) {
		t.Errorf("calendar_hash = %#v, want the hash of the calendar the deadline was computed on", e.Body["calendar_hash"])
	}
	events, err := journal.Replay(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := Fold(events)
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the delivery opened no acceptance window")
	}
	if stamp := w.Deadline.Format(time.RFC3339); stamp != wantDeadline {
		t.Errorf("the replayed deadline = %s, want %s", stamp, wantDeadline)
	}
	before := buf.Len()
	if _, err := Deliver(a, bizday.Calendar{Version: "2026.1"}, "m2", at(t, deliveredAt), 10); err == nil {
		t.Error("Deliver wrote a delivery on a calendar that names no zone")
	}
	if buf.Len() != before {
		t.Error("a refused Deliver wrote a line")
	}
}

func TestARejectionThatNamesNoFailedCriterionDoesNotStopTheClock(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no defects array", map[string]any{"milestone": "m2"}, "no defects array"},
		{"an empty defects array", map[string]any{"milestone": "m2", "defects": []any{}}, "no failed criterion"},
		{"a criterion of another milestone", map[string]any{"milestone": "m2", "defects": []any{defect("c1", "the report is missing")}}, "does not carry"},
		{"a criterion the contract never named", map[string]any{"milestone": "m2", "defects": []any{defect("c9", "the build is red")}}, "does not carry"},
		{"a defect with no note", map[string]any{"milestone": "m2", "defects": []any{map[string]any{"criterion": "c2"}}}, "note"},
		{"a defect with an empty note", map[string]any{"milestone": "m2", "defects": []any{defect("c2", "")}}, "note"},
		{"a defect that is not an object", map[string]any{"milestone": "m2", "defects": []any{"c2"}}, "not an object"},
		{"a defect carrying another key", map[string]any{"milestone": "m2", "defects": []any{map[string]any{"criterion": "c2", "note": "slow", "severity": "high"}}}, "beyond criterion and note"},
		{"the same criterion twice", map[string]any{"milestone": "m2", "defects": []any{defect("c2", "first"), defect("c2", "again")}}, "repeats criterion"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fold(t, head(config(10, RetestRestart)), deliver(t, "m2", deliveredAt, 10), step{EventRejected, c.body})
			if len(got.Refused) != 1 {
				t.Fatalf("the fold refused %v, want exactly the rejection", got.Refused)
			}
			r := got.Refused[0]
			if r.Seq != 3 || r.Type != EventRejected {
				t.Errorf("refusal = %+v, want the rejection at seq 3", r)
			}
			if !strings.Contains(r.Msg, c.want) {
				t.Errorf("message %q should mention %q", r.Msg, c.want)
			}
			m := milestoneOf(t, got, "m2")
			if m.Status != StatusDelivered {
				t.Errorf("status = %q, want the milestone to stay delivered", m.Status)
			}
			w, open := m.Open()
			if !open {
				t.Fatal("a rejection naming no failed criterion closed the window")
			}
			if w.Days != 10 || len(w.Defects) != 0 || w.Deadline.Format(time.RFC3339) != wantDeadline {
				t.Errorf("window = %+v, want the clock still running to %s", w, wantDeadline)
			}
		})
	}
}

func TestAValidRejectionClosesTheWindowAndARetestOpensANewOne(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver(t, "m2", deliveredAt, 10),
		rejectAfter("m2", 4, defect("c3", "the export drops the last row")),
		deliver(t, "m2", "2026-03-06 09:00", 10))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusDelivered {
		t.Errorf("status = %q, want the retest to put the milestone back to delivered", m.Status)
	}
	if len(m.Windows) != 2 {
		t.Fatalf("got %d windows, want the first one and the retest", len(m.Windows))
	}
	first := m.Windows[0]
	if !first.Closed || first.ClosedBy != 3 {
		t.Errorf("first window = %+v, want it closed by the rejection at seq 3", first)
	}
	if first.Used != 4 || first.Remaining() != 6 {
		t.Errorf("first window used %d of %d days, want 4 of 10", first.Used, first.Days)
	}
	if len(first.Defects) != 1 || first.Defects[0].Criterion != "c3" || first.Defects[0].Note == "" {
		t.Errorf("defects = %+v, want the one failed criterion with its note", first.Defects)
	}
	retest, open := m.Open()
	if !open {
		t.Fatal("the retest opened no acceptance window")
	}
	if !retest.Retest || retest.Seq != 4 || retest.Days != 10 {
		t.Errorf("retest window = %+v, want a restarted ten-day window opened at seq 4", retest)
	}
	if !retest.Deadline.After(first.Deadline) {
		t.Errorf("the retest runs to %s, want an instant after the first deadline %s", retest.Deadline, first.Deadline)
	}
}

func TestAResumedRetestCarriesTheRemainingDays(t *testing.T) {
	got := fold(t, head(config(10, RetestResume)),
		deliver(t, "m2", deliveredAt, 10),
		rejectAfter("m2", 4, defect("c2", "the API answers 500 on an empty cart")),
		deliver(t, "m2", "2026-03-06 09:00", 6))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the retest opened no acceptance window")
	}
	if !w.Retest || w.Days != 6 || w.Deadline.IsZero() {
		t.Errorf("retest window = %+v, want the six days the first one had left", w)
	}

	full := fold(t, head(config(10, RetestResume)),
		deliver(t, "m2", deliveredAt, 10),
		rejectAfter("m2", 4, defect("c2", "the API answers 500 on an empty cart")),
		deliver(t, "m2", "2026-03-06 09:00", 10))
	if len(full.Refused) != 1 {
		t.Fatalf("the fold refused %v, want the retest counted on the full term", full.Refused)
	}
	if !strings.Contains(full.Refused[0].Msg, "6-business-day window") {
		t.Errorf("message %q should say the resumed window runs six business days", full.Refused[0].Msg)
	}

	spent := fold(t, head(config(10, RetestResume)),
		deliver(t, "m1", deliveredAt, 10),
		rejectAfter("m1", 12, defect("c1", "the discovery report is missing")),
		deliver(t, "m1", "2026-03-06 09:00", 0))
	over, open := milestoneOf(t, spent, "m1").Open()
	if !open {
		t.Fatal("the retest opened no acceptance window")
	}
	if over.Days != 0 {
		t.Errorf("a retest after a window spent over runs %d days, want 0", over.Days)
	}
	if stamp := over.Deadline.Format(time.RFC3339); stamp != "2026-03-06T16:00:00Z" {
		t.Errorf("a retest of no days runs to %s, want the close of the delivery day", stamp)
	}
}

func TestDeemClosesAWindowOnceItsDeadlineHasPassed(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)), deliver(t, "m2", deliveredAt, 10))
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the delivery opened no acceptance window")
	}
	if err := got.Deem("m2", w.Deadline); err == nil {
		t.Error("Deem closed a window at the very instant it still runs to")
	}
	past := w.Deadline.Add(time.Second)
	if err := got.Deem("m2", past); err != nil {
		t.Fatalf("Deem: %v", err)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusDeemedAccepted {
		t.Errorf("status = %q, want deemed_accepted", m.Status)
	}
	if closed := m.Windows[0]; !closed.Closed || closed.ClosedBy != 0 || !closed.Deadline.Equal(w.Deadline) {
		t.Errorf("window = %+v, want it closed by the clock on %s", closed, w.Deadline)
	}
	for name, call := range map[string]func() error{
		"a milestone that is already deemed accepted": func() error { return got.Deem("m2", past) },
		"a milestone the contract does not carry":     func() error { return got.Deem("m9", past) },
		"a milestone with no open window":             func() error { return got.Deem("m1", past) },
	} {
		if err := call(); err == nil {
			t.Errorf("Deem accepted %s", name)
		}
	}

	undated := fold(t, headOn(nil, nil), bareDelivery("m2"))
	if len(undated.Refused) != 0 {
		t.Fatalf("the fold refused %v", undated.Refused)
	}
	if err := undated.Deem("m2", past); err == nil {
		t.Error("Deem ran on a delivery that fixed no deadline")
	}
}

func TestEveryEventOnASettledMilestoneIsRefused(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver(t, "m1", deliveredAt, 10), accept("m1"),
		bareDelivery("m1"), accept("m1"), reject("m1", defect("c1", "too late")))
	if len(got.Refused) != 3 {
		t.Fatalf("the fold refused %v, want the three events after the acceptance", got.Refused)
	}
	for _, r := range got.Refused {
		if !strings.Contains(r.Msg, "already accepted") {
			t.Errorf("message %q should say the milestone is already accepted", r.Msg)
		}
	}
	if m := milestoneOf(t, got, "m1"); m.Status != StatusAccepted || len(m.Windows) != 1 {
		t.Errorf("m1 = %+v, want one window and the accepted status", m)
	}
}

func TestFoldRefusesEventsItCannotApply(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		step{EventDelivered, map[string]any{}},
		bareDelivery("m9"),
		accept("m2"),
		reject("m1", defect("c1", "no sign-off")),
		deliver(t, "m2", deliveredAt, 10), bareDelivery("m2"),
		step{"invoice.issued", map[string]any{"amount": int64(1)}},
		step{EventOpened, head(nil)})
	want := []string{
		"no milestone string",
		"does not carry",
		"window is not open",
		"window is not open",
		"window is open",
		"already open",
	}
	if len(got.Refused) != len(want) {
		t.Fatalf("the fold refused %v, want %d events", got.Refused, len(want))
	}
	for i, w := range want {
		if !strings.Contains(got.Refused[i].Msg, w) {
			t.Errorf("refusal %d = %q, should mention %q", i, got.Refused[i], w)
		}
	}
	if m := milestoneOf(t, got, "m2"); m.Status != StatusDelivered || len(m.Windows) != 1 {
		t.Errorf("m2 = %+v, want one open window", m)
	}
}

func TestFoldRefusesABadHeadAndEverythingAfterIt(t *testing.T) {
	cases := map[string]map[string]any{
		"a head with no milestones":                 {"currency": "EUR"},
		"a milestone with no criteria array":        {"milestones": []any{map[string]any{"id": "m1", "title": "Discovery"}}},
		"a milestone repeating an id":               {"milestones": []any{milestone("m1", "Discovery", "c1"), milestone("m1", "Again", "c2")}},
		"an acceptance value that is not an object": headConfigured([]any{}),
		"an unknown retest_window":                  headConfigured(map[string]any{"retest_window": "extend"}),
		"a window that is not a count":              headConfigured(map[string]any{"window_days": "ten"}),
		"a window with no calendar to count it on":  headOn(nil, config(10, RetestRestart)),
		"a calendar that is not an object":          headOn([]any{}, nil),
		"a calendar in an unknown zone":             headOn(map[string]any{"version": "2026.1", "zone": "Mars/Olympus"}, nil),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			got := fold(t, h, bareDelivery("m1"))
			if len(got.Refused) != 2 {
				t.Fatalf("the fold refused %v, want the head and the delivery after it", got.Refused)
			}
			if got.Refused[0].Seq != 1 || got.Refused[0].Type != EventOpened {
				t.Errorf("first refusal = %+v, want the head at seq 1", got.Refused[0])
			}
			if !strings.Contains(got.Refused[1].Msg, "before contract.opened") {
				t.Errorf("message %q should say the event arrives before contract.opened", got.Refused[1].Msg)
			}
			if len(got.Milestones) != 0 {
				t.Errorf("a refused head left milestones %+v", got.Milestones)
			}
		})
	}
}
