package acceptance

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/polycratia/engageledger/journal"
)

const (
	// weekendStart..weekendEnd runs from a Friday through the Tuesday after it:
	// five calendar days, three the acceptance clock runs on.
	weekendStart = "2026-03-06"
	weekendEnd   = "2026-03-10"
	// wantExtended is the deadline a three-day hold moves the ten-day window to.
	wantExtended = "2026-03-19T16:00:00Z"
)

// paused is the journal of a delivery both parties then agreed to pause, up to
// the approval that makes the hold effective.
func paused(t *testing.T, id, start, end string) []step {
	t.Helper()
	return []step{
		deliver(t, "m2", deliveredAt, 10),
		propose(id, "m2", start, end),
		approveBy(id, client), approveBy(id, contractor),
	}
}

// extensionBody is the deadline.extended body the fold of h and steps computes
// for one hold, which is what a writer appends next.
func extensionBody(t *testing.T, id string, h map[string]any, steps ...step) map[string]any {
	t.Helper()
	a := fold(t, h, steps...)
	if len(a.Refused) != 0 {
		t.Fatalf("the fold refused %v", a.Refused)
	}
	body, err := a.Extension(id)
	if err != nil {
		t.Fatalf("Extension(%q): %v", id, err)
	}
	return body
}

func extension(t *testing.T, id string, h map[string]any, steps ...step) step {
	t.Helper()
	return step{EventExtended, extensionBody(t, id, h, steps...)}
}

// replay folds the journal written so far.
func replay(t *testing.T, buf *bytes.Buffer) Acceptance {
	t.Helper()
	events, err := journal.Replay(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return Fold(events)
}

func TestAnEffectiveHoldExtendsTheDeadlineOverTheDaysItCovered(t *testing.T) {
	ten := head(config(10, RetestRestart))
	up := paused(t, "h1", weekendStart, weekendEnd)
	got := fold(t, ten, slices.Concat(up, []step{extension(t, "h1", ten, up...)})...)
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	h := holdOf(t, got, "h1")
	if h.Days != 3 {
		t.Errorf("the hold covers %d business days, want the three of the five it spans", h.Days)
	}
	if !h.Effective || h.ExtendedBy != 6 {
		t.Errorf("hold = %+v, want the extension at seq 6 recorded on it", h)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusDelivered {
		t.Errorf("status = %q, want the milestone to stay delivered under an extended window", m.Status)
	}
	w, open := m.Open()
	if !open {
		t.Fatal("the extension closed the acceptance window")
	}
	if w.Days != 10 || w.Held != 3 {
		t.Errorf("window = %+v, want its ten days and the three the hold paused", w)
	}
	if stamp := w.Deadline.Format(time.RFC3339); stamp != wantExtended {
		t.Errorf("deadline = %s, want %s", stamp, wantExtended)
	}
	if w.CalendarHash != hashOf(t, cal) {
		t.Errorf("the extended window counts on %q, want the calendar its delivery named", w.CalendarHash)
	}
	if drifts := driftsOf(t, got); len(drifts) != 0 {
		t.Errorf("Verify reported %v on a deadline the window and its hold add up to", drifts)
	}
	ranOut, err := time.Parse(time.RFC3339, wantDeadline)
	if err != nil {
		t.Fatalf("parsing %q: %v", wantDeadline, err)
	}
	if err := got.Deem("m2", ranOut.Add(time.Second)); err == nil {
		t.Error("Deem closed the window on the deadline the delivery fixed, which the hold moved")
	}
	if err := got.Deem("m2", w.Deadline.Add(time.Second)); err != nil {
		t.Fatalf("Deem after the extended deadline: %v", err)
	}
	if status := milestoneOf(t, got, "m2").Status; status != StatusDeemedAccepted {
		t.Errorf("status = %q, want deemed_accepted once the extended deadline passed", status)
	}
}

func TestASecondHoldExtendsTheDeadlineAgain(t *testing.T) {
	ten := head(config(10, RetestRestart))
	first := paused(t, "h1", holdStart, holdEnd)
	once := slices.Concat(first, []step{extension(t, "h1", ten, first...)})
	twice := slices.Concat(once, []step{
		propose("h2", "m2", "2026-03-12", "2026-03-13"),
		approveBy("h2", client), approveBy("h2", contractor),
	})
	got := fold(t, ten, slices.Concat(twice, []step{extension(t, "h2", ten, twice...)})...)
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	for id, by := range map[string]int64{"h1": 6, "h2": 10} {
		if h := holdOf(t, got, id); !h.Effective || h.ExtendedBy != by {
			t.Errorf("hold %q = %+v, want the extension at seq %d recorded on it", id, h, by)
		}
	}
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the second extension closed the acceptance window")
	}
	if w.Held != 5 {
		t.Errorf("the window pauses %d days, want the three of the first hold and the two of the second", w.Held)
	}
	if stamp := w.Deadline.Format(time.RFC3339); stamp != "2026-03-23T16:00:00Z" {
		t.Errorf("deadline = %s, want 2026-03-23T16:00:00Z", stamp)
	}
	if drifts := driftsOf(t, got); len(drifts) != 0 {
		t.Errorf("Verify reported %v on a window both holds extended", drifts)
	}
}

func TestAHoldAfterTheWindowRanOutDoesNotReopenAcceptance(t *testing.T) {
	ten := head(config(10, RetestRestart))
	got := fold(t, ten,
		deliver(t, "m2", deliveredAt, 10),
		propose("h1", "m2", "2026-03-17", "2026-03-18"),
		approveBy("h1", client), approveBy("h1", contractor))
	if len(got.Refused) != 3 {
		t.Fatalf("the fold refused %v, want the proposal and the two approvals of it", got.Refused)
	}
	if !strings.Contains(got.Refused[0].Msg, "after the acceptance window") {
		t.Errorf("message %q should say the hold starts after the window ran out", got.Refused[0].Msg)
	}
	for _, r := range got.Refused[1:] {
		if !strings.Contains(r.Msg, "no hold.proposed") {
			t.Errorf("message %q should say no proposal carries the hold", r.Msg)
		}
	}
	if len(got.Holds) != 0 {
		t.Errorf("the fold carries holds %+v, want the refused proposal to leave none", got.Holds)
	}
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the refused hold closed the acceptance window")
	}
	if w.Held != 0 || w.Deadline.Format(time.RFC3339) != wantDeadline {
		t.Errorf("window = %+v, want the clock running to %s", w, wantDeadline)
	}
	if body, err := got.Extension("h1"); err == nil {
		t.Errorf("Extension computed %v for a hold the fold refused", body)
	}
	if err := got.Deem("m2", w.Deadline.Add(time.Second)); err != nil {
		t.Fatalf("Deem after the recorded deadline: %v", err)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusDeemedAccepted {
		t.Errorf("status = %q, want deemed_accepted", m.Status)
	}
	if _, open := m.Open(); open {
		t.Error("the milestone carries an open window after the clock ran out")
	}

	// An effective hold that recorded no extension moved no deadline, so the
	// clock still ran out, and a window that is gone cannot be extended.
	ran := fold(t, ten, paused(t, "h1", holdStart, holdEnd)...)
	if len(ran.Refused) != 0 {
		t.Fatalf("the fold refused %v", ran.Refused)
	}
	if err := ran.Deem("m2", w.Deadline.Add(time.Second)); err != nil {
		t.Fatalf("Deem after the recorded deadline of a paused window: %v", err)
	}
	_, err := ran.Extension("h1")
	if err == nil {
		t.Fatal("Extension extended the window of a milestone the clock ran out on")
	}
	if !strings.Contains(err.Error(), "no open acceptance window") {
		t.Errorf("error %q should say there is no open window left to extend", err)
	}
}

func TestAnExtensionTheFoldWillNotApply(t *testing.T) {
	ten := head(config(10, RetestRestart))
	up := paused(t, "h1", holdStart, holdEnd)
	body := extensionBody(t, "h1", ten, up...)
	good := step{EventExtended, body}
	with := func(key string, v any) step {
		b := copyBody(body)
		b[key] = v
		return step{EventExtended, b}
	}
	without := func(key string) step {
		b := copyBody(body)
		delete(b, key)
		return step{EventExtended, b}
	}
	broken := reject("m2", defect("c3", "the export drops the last row"))
	twoDeliveries := []step{
		deliver(t, "m2", deliveredAt, 10), deliver(t, "m1", deliveredAt, 10),
		propose("h1", "m2", holdStart, holdEnd),
		approveBy("h1", client), approveBy("h1", contractor),
	}
	elsewhere := copyBody(extensionBody(t, "h1", ten, twoDeliveries...))
	elsewhere["milestone"] = "m1"

	cases := []struct {
		name  string
		steps []step
		seq   int64
		want  string
	}{
		{"an extension of a hold no event proposed", slices.Concat(up, []step{with("hold", "h9")}), 6, "no hold.proposed"},
		{"an extension naming no hold", slices.Concat(up, []step{without("hold")}), 6, "no hold string"},
		{"an extension naming no calendar", slices.Concat(up, []step{without("calendar_hash")}), 6, "no calendar_hash string"},
		{"an extension fixing no deadline", slices.Concat(up, []step{without("deadline_utc")}), 6, "no deadline_utc string"},
		{
			"an extension counting on another calendar version",
			slices.Concat(up, []step{with("calendar_hash", hashOf(t, newCalendar("2026-03-16")))}),
			6, "while the window of milestone",
		},
		{
			"an extension moving the deadline back",
			slices.Concat(up, []step{with("deadline_utc", "2026-03-16T15:00:00Z")}), 6, "a pause cannot do",
		},
		{"an extension on a hold one party approved", []step{up[0], up[1], up[2], good}, 5, "pauses nothing"},
		{"a second extension of the same hold", slices.Concat(up, []step{good, good}), 7, "again"},
		{
			"an extension on a milestone whose window a rejection closed",
			slices.Concat(up, []step{broken, good}), 7, "window is not open",
		},
		{
			"an extension on a milestone that is already accepted",
			slices.Concat(up, []step{accept("m2"), good}), 7, "already accepted",
		},
		{
			"an extension naming another milestone than its hold pauses",
			slices.Concat(twoDeliveries, []step{{EventExtended, elsewhere}}), 7, "while hold",
		},
		{
			"an extension on a hold proposed before the open window",
			slices.Concat(up, []step{broken, deliver(t, "m2", "2026-03-06 09:00", 10), good}),
			8, "proposed before the window",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fold(t, ten, c.steps...)
			if len(got.Refused) != 1 {
				t.Fatalf("the fold refused %v, want exactly one event", got.Refused)
			}
			r := got.Refused[0]
			if r.Seq != c.seq || r.Type != EventExtended {
				t.Errorf("refusal = %+v, want the %s at seq %d", r, EventExtended, c.seq)
			}
			if !strings.Contains(r.Msg, c.want) {
				t.Errorf("message %q should mention %q", r.Msg, c.want)
			}
		})
	}
}

func TestAReplayReportsADriftedExtensionAndKeepsTheRecordedOne(t *testing.T) {
	ten := head(config(10, RetestRestart))
	up := paused(t, "h1", holdStart, holdEnd)
	body := extensionBody(t, "h1", ten, up...)
	recorded, err := time.Parse(time.RFC3339, body["deadline_utc"].(string))
	if err != nil {
		t.Fatalf("parsing the computed deadline: %v", err)
	}
	cases := []struct {
		name  string
		off   time.Duration
		want  string
		cause Cause
	}{
		{"an hour off, as another zone database reads the close", time.Hour, "2026-03-19T17:00:00Z", CauseTzdata},
		{"a day off, which the calendar does not fix at all", 24 * time.Hour, "2026-03-20T16:00:00Z", CauseCalendar},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drifted := copyBody(body)
			drifted["deadline_utc"] = recorded.Add(c.off).UTC().Format(time.RFC3339)
			got := fold(t, ten, slices.Concat(up, []step{{EventExtended, drifted}})...)
			if len(got.Refused) != 0 {
				t.Fatalf("the fold refused %v, want the recorded deadline taken as it stands", got.Refused)
			}
			w, open := milestoneOf(t, got, "m2").Open()
			if !open {
				t.Fatal("the extension closed the acceptance window")
			}
			if stamp := w.Deadline.Format(time.RFC3339); stamp != c.want {
				t.Errorf("the fold uses deadline %s, want the recorded %s", stamp, c.want)
			}
			drifts := driftsOf(t, got)
			if len(drifts) != 1 {
				t.Fatalf("Verify reported %v, want the one drifted deadline", drifts)
			}
			d := drifts[0]
			if d.Seq != 2 || d.Milestone != "m2" || d.Cause != c.cause {
				t.Errorf("drift = %+v, want the window at seq 2 of m2 caused by %s", d, c.cause)
			}
			if stamp := d.Recomputed.Format(time.RFC3339); stamp != wantExtended {
				t.Errorf("drift recomputes %s, want the term the window and its hold add up to, %s", stamp, wantExtended)
			}
			if err := got.Deem("m2", d.Recomputed.Add(time.Second)); err == nil {
				t.Error("Deem closed the window on the deadline a replay recomputed, not the recorded one")
			}
		})
	}
}

func TestExtendWritesTheDeadlineTheFoldReadsBack(t *testing.T) {
	var buf bytes.Buffer
	ap := journal.NewAppender(&buf)
	if _, err := ap.Append(EventOpened, head(config(10, RetestRestart))); err != nil {
		t.Fatalf("appending the contract head: %v", err)
	}
	if _, err := Deliver(ap, cal, "m2", at(t, deliveredAt), 10); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	for _, s := range []step{
		propose("h1", "m2", weekendStart, weekendEnd),
		approveBy("h1", client), approveBy("h1", contractor),
	} {
		if _, err := ap.Append(s.typ, s.body); err != nil {
			t.Fatalf("appending %s: %v", s.typ, err)
		}
	}
	a := replay(t, &buf)
	if len(a.Refused) != 0 {
		t.Fatalf("the fold refused %v", a.Refused)
	}
	e, err := a.Extend(ap, "h1")
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if e.Body["deadline_utc"] != wantExtended {
		t.Errorf("deadline_utc = %#v, want %q", e.Body["deadline_utc"], wantExtended)
	}
	if e.Body["calendar_hash"] != hashOf(t, cal) {
		t.Errorf("calendar_hash = %#v, want the calendar version the paused window recorded", e.Body["calendar_hash"])
	}
	if e.Body["hold"] != "h1" || e.Body["milestone"] != "m2" {
		t.Errorf("body = %#v, want the hold and the milestone it pauses", e.Body)
	}
	got := replay(t, &buf)
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the extension closed the acceptance window")
	}
	if w.Held != 3 || w.Deadline.Format(time.RFC3339) != wantExtended {
		t.Errorf("window = %+v, want the three days the hold paused and the deadline %s", w, wantExtended)
	}
	if drifts := driftsOf(t, got); len(drifts) != 0 {
		t.Errorf("Verify reported %v on an extension its own calendar fixes", drifts)
	}
	before := buf.Len()
	if _, err := got.Extend(ap, "h1"); err == nil {
		t.Error("Extend wrote a second extension of one hold")
	}
	if _, err := got.Extend(ap, "h9"); err == nil {
		t.Error("Extend wrote an extension of a hold the journal does not carry")
	}
	if buf.Len() != before {
		t.Error("a refused Extend wrote a line")
	}
}
