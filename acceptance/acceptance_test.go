package acceptance

import (
	"bytes"
	"strings"
	"testing"

	"github.com/polycratia/engageledger/journal"
)

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
// and m2 with two, under the given acceptance config or none when it is nil.
func head(config map[string]any) map[string]any {
	h := map[string]any{
		"parties":  map[string]any{"client": "lantern-co", "contractor": "oakline-studio"},
		"currency": "EUR",
		"milestones": []any{
			milestone("m1", "Discovery", "c1"),
			milestone("m2", "Build", "c2", "c3"),
		},
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

func deliver(id string) step { return step{EventDelivered, map[string]any{"milestone": id}} }
func accept(id string) step  { return step{EventAccepted, map[string]any{"milestone": id}} }

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

func TestDeliveryOpensAWindowAndAcceptanceClosesIt(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)), deliver("m2"), accept("m2"))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	if got.WindowDays != 10 || got.RetestWindow != RetestRestart {
		t.Errorf("config = %d days, %q, want 10 and restart", got.WindowDays, got.RetestWindow)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusAccepted {
		t.Errorf("status = %q, want accepted", m.Status)
	}
	if len(m.Windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(m.Windows))
	}
	if w := m.Windows[0]; w.Seq != 2 || w.Days != 10 || !w.Closed || w.ClosedBy != 3 {
		t.Errorf("window = %+v, want a ten-day window opened at seq 2 and closed by seq 3", w)
	}
	if _, open := m.Open(); open {
		t.Error("the window is still open after an acceptance")
	}
	if m1 := milestoneOf(t, got, "m1"); m1.Status != "" || len(m1.Windows) != 0 {
		t.Errorf("m1 = %+v, want an undelivered milestone", m1)
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
			got := fold(t, head(config(10, RetestRestart)), deliver("m2"), step{EventRejected, c.body})
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
			if w.Days != 10 || len(w.Defects) != 0 {
				t.Errorf("window = %+v, want the clock still running on ten days", w)
			}
		})
	}
}

func TestAValidRejectionClosesTheWindowAndARetestOpensANewOne(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver("m2"),
		rejectAfter("m2", 4, defect("c3", "the export drops the last row")),
		deliver("m2"))
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
}

func TestAResumedRetestCarriesTheRemainingDays(t *testing.T) {
	got := fold(t, head(config(10, RetestResume)),
		deliver("m2"),
		rejectAfter("m2", 4, defect("c2", "the API answers 500 on an empty cart")),
		deliver("m2"))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the retest opened no acceptance window")
	}
	if !w.Retest || w.Days != 6 {
		t.Errorf("retest window = %+v, want the six days the first one had left", w)
	}

	spent := fold(t, head(config(10, RetestResume)),
		deliver("m1"),
		rejectAfter("m1", 12, defect("c1", "the discovery report is missing")),
		deliver("m1"))
	over, open := milestoneOf(t, spent, "m1").Open()
	if !open {
		t.Fatal("the retest opened no acceptance window")
	}
	if over.Days != 0 {
		t.Errorf("a retest after a window spent over runs %d days, want 0", over.Days)
	}
}

func TestEveryEventOnASettledMilestoneIsRefused(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver("m1"), accept("m1"),
		deliver("m1"), accept("m1"), reject("m1", defect("c1", "too late")))
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

	deemed := fold(t, head(config(10, RetestRestart)), deliver("m2"))
	if err := deemed.Deem("m2", "2026-03-05"); err != nil {
		t.Fatalf("Deem: %v", err)
	}
	after := Fold(nil)
	_ = after
}

func TestDeemTakesTheDeadlineTheClockSupplies(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)), deliver("m2"))
	if err := got.Deem("m2", "2026-03-05"); err != nil {
		t.Fatalf("Deem: %v", err)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusDeemedAccepted {
		t.Errorf("status = %q, want deemed_accepted", m.Status)
	}
	if w := m.Windows[0]; !w.Closed || w.Deadline != "2026-03-05" || w.ClosedBy != 0 {
		t.Errorf("window = %+v, want it closed by the clock on 2026-03-05", w)
	}
	for name, call := range map[string]func() error{
		"a milestone that is already deemed accepted": func() error { return got.Deem("m2", "2026-03-06") },
		"a milestone the contract does not carry":     func() error { return got.Deem("m9", "2026-03-05") },
		"a milestone with no open window":             func() error { return got.Deem("m1", "2026-03-05") },
		"an empty deadline":                           func() error { return got.Deem("m2", "") },
	} {
		if err := call(); err == nil {
			t.Errorf("Deem accepted %s", name)
		}
	}

	bare := fold(t, head(nil), deliver("m2"))
	if err := bare.Deem("m2", "2026-03-05"); err == nil {
		t.Error("Deem ran under a contract that sets no acceptance window")
	}
}

func TestFoldRefusesEventsItCannotApply(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		step{EventDelivered, map[string]any{}},
		deliver("m9"),
		accept("m2"),
		reject("m1", defect("c1", "no sign-off")),
		deliver("m2"), deliver("m2"),
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
		"a head with no milestones":                {"currency": "EUR"},
		"a milestone with no criteria array":       {"milestones": []any{map[string]any{"id": "m1", "title": "Discovery"}}},
		"a milestone repeating an id":              {"milestones": []any{milestone("m1", "Discovery", "c1"), milestone("m1", "Again", "c2")}},
		"an acceptance value that is not an object": headConfigured([]any{}),
		"an unknown retest_window":                  headConfigured(map[string]any{"retest_window": "extend"}),
		"a window that is not a count":              headConfigured(map[string]any{"window_days": "ten"}),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			got := fold(t, h, deliver("m1"))
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
