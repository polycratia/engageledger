package billable

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polycratia/engageledger/acceptance"
	"github.com/polycratia/engageledger/bizday"
	"github.com/polycratia/engageledger/canon"
	"github.com/polycratia/engageledger/journal"
	"github.com/polycratia/engageledger/nte"
	"github.com/polycratia/engageledger/terms"
)

var update = flag.Bool("update", false, "rewrite the golden file in testdata")

const (
	client     = "lantern-co"
	contractor = "oakline-studio"
	eur        = "EUR"
	zone       = "Europe/Berlin"
	rate       = int64(6000)
	golden     = "sample.jsonl"
)

// cal is the calendar the sample contract counts its acceptance windows on.
var cal = newCalendar()

func newCalendar() bizday.Calendar {
	c, err := bizday.New("2026.1", zone, nil, nil, "17:00")
	if err != nil {
		panic("building the test calendar: " + err.Error())
	}
	return c
}

// deliveredAt is the instant every delivery of these cases arrives on.
var deliveredAt = time.Date(2026, 4, 6, 8, 0, 0, 0, time.UTC)

// passed is an instant after every deadline the calendar fixes here, so Deem
// closes a window without the case naming the recorded instant.
var passed = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

func milestone(id, title string, amount int64, criterion string) map[string]any {
	return map[string]any{"id": id, "title": title, "amount": amount, "criteria": []any{criterion}}
}

// head is the contract every case opens on: three milestones, a ten-day
// acceptance window on cal, a forty-hour cap and the given billing config.
func head(billing map[string]any) map[string]any {
	h := map[string]any{
		"parties":         map[string]any{"client": client, "contractor": contractor},
		"currency":        eur,
		"calendar":        cal.Object(),
		"acceptance":      map[string]any{"window_days": int64(10), "retest_window": "restart"},
		"nte_cap_minutes": int64(2400),
		"milestones": []any{
			milestone("m1", "Discovery", 100000, "c1"),
			milestone("m2", "Build", 200000, "c2"),
			milestone("m3", "Handover", 50000, "c3"),
		},
	}
	if billing != nil {
		h["billing"] = billing
	}
	return h
}

func billing(minorPerHour int64) map[string]any {
	return map[string]any{keyRate: minorPerHour}
}

type ledger struct {
	t    *testing.T
	buf  bytes.Buffer
	app  *journal.Appender
	hash map[string]string
}

func newLedger(t *testing.T, h map[string]any) *ledger {
	t.Helper()
	l := &ledger{t: t, hash: map[string]string{}}
	l.app = journal.NewAppender(&l.buf)
	l.add("", EventOpened, h)
	return l
}

func (l *ledger) add(label, typ string, body map[string]any) journal.Event {
	l.t.Helper()
	e, err := l.app.Append(typ, body)
	if err != nil {
		l.t.Fatalf("appending %s: %v", typ, err)
	}
	if label != "" {
		l.hash[label] = e.Hash
	}
	return e
}

func (l *ledger) deliver(id string) {
	l.t.Helper()
	body, err := acceptance.DeliveryBody(cal, id, deliveredAt, 10)
	if err != nil {
		l.t.Fatalf("DeliveryBody(%q): %v", id, err)
	}
	l.add("", acceptance.EventDelivered, body)
}

func (l *ledger) accept(id string) {
	l.t.Helper()
	l.add("", acceptance.EventAccepted, map[string]any{"milestone": id})
}

func (l *ledger) log(date string, minutes int64) {
	l.t.Helper()
	l.add("", nte.EventLogged, map[string]any{"date": date, "minutes": minutes})
}

func (l *ledger) propose(label string, ops ...any) {
	l.t.Helper()
	l.add(label, terms.EventProposed, map[string]any{"currency": eur, "ops": ops})
}

// effective proposes a change order and has both parties approve it, which is
// all a manual line needs to reference.
func (l *ledger) effective(label string, ops ...any) {
	l.t.Helper()
	l.propose(label, ops...)
	for _, by := range []string{client, contractor} {
		l.add("", terms.EventApproved, map[string]any{"by": by, "change_order": l.hash[label]})
	}
}

func (l *ledger) manual(hash string, amount int64, note string) {
	l.t.Helper()
	body, err := ManualBody(amount, note, hash)
	if err != nil {
		l.t.Fatalf("ManualBody: %v", err)
	}
	l.add("", EventManual, body)
}

func (l *ledger) tryRead() (Fold, error) {
	l.t.Helper()
	events, err := journal.Replay(bytes.NewReader(l.buf.Bytes()))
	if err != nil {
		l.t.Fatalf("replaying the built journal: %v", err)
	}
	return Read(events)
}

func (l *ledger) read() Fold {
	l.t.Helper()
	f, err := l.tryRead()
	if err != nil {
		l.t.Fatalf("Read: %v", err)
	}
	return f
}

func (l *ledger) derive() []Event {
	l.t.Helper()
	got, err := Derive(l.read())
	if err != nil {
		l.t.Fatalf("Derive: %v", err)
	}
	return got
}

// sample is the contract the golden file pins: m1 accepted, m2 deemed
// accepted, m3 untouched, and three days of in-cap time across two months.
func sample(t *testing.T) Fold {
	t.Helper()
	l := newLedger(t, head(billing(rate)))
	l.deliver("m1")
	l.accept("m1")
	l.deliver("m2")
	l.log("2026-04-06", 480)
	l.log("2026-04-07", 480)
	l.log("2026-05-04", 240)
	f := l.read()
	if err := f.Acceptance.Deem("m2", passed); err != nil {
		t.Fatalf("Deem: %v", err)
	}
	return f
}

func render(t *testing.T, events []Event) string {
	t.Helper()
	var out strings.Builder
	for _, e := range events {
		b, err := canon.Marshal(e.Object())
		if err != nil {
			t.Fatalf("Marshal(%+v): %v", e, err)
		}
		out.Write(b)
		out.WriteString("\n")
	}
	return out.String()
}

func TestTheDerivedEventsOfTheSampleContract(t *testing.T) {
	events, err := Derive(sample(t))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	got := render(t, events)
	path := filepath.Join("testdata", golden)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if got != string(want) {
		t.Errorf("the derived events moved:\n got %s\nwant %s", got, want)
	}
	for _, e := range events {
		obj := e.Object()
		for _, key := range invoicing {
			if _, carries := obj[key]; carries {
				t.Errorf("a %s event carries %s", e.Kind, key)
			}
		}
		if e.Currency != eur {
			t.Errorf("event %+v is not in the contract currency", e)
		}
	}
}

func TestAManualLineNeedsAnEffectiveChangeOrder(t *testing.T) {
	l := newLedger(t, head(billing(rate)))
	l.effective("extra", map[string]any{"op": "reprice", "milestone": "m3", "amount": int64(70000)})
	l.manual(l.hash["extra"], 25000, "rush courier")
	got := l.derive()
	if len(got) != 1 {
		t.Fatalf("derived %+v, want the manual line alone", got)
	}
	e := got[0]
	if e.Kind != KindManual || e.Amount != 25000 || e.Note != "rush courier" || e.Currency != eur {
		t.Errorf("event = %+v, want a 25000 EUR manual line", e)
	}
	if e.ChangeOrder != l.hash["extra"] {
		t.Errorf("the line references %q, want the effective change order", e.ChangeOrder)
	}

	cases := []struct {
		name  string
		build func(t *testing.T) *ledger
		want  string
	}{
		{
			name: "a change order only one party approved",
			build: func(t *testing.T) *ledger {
				l := newLedger(t, head(billing(rate)))
				l.propose("open", map[string]any{"op": "set_cap", "amount": int64(500000)})
				l.manual(l.hash["open"], 25000, "rush courier")
				return l
			},
			want: "not effective",
		},
		{
			name: "a change order the journal never proposed",
			build: func(t *testing.T) *ledger {
				l := newLedger(t, head(billing(rate)))
				l.manual(strings.Repeat("ab", 32), 25000, "rush courier")
				return l
			},
			want: "no change_order.proposed carries",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Derive(c.build(t).read())
			if err == nil {
				t.Fatalf("Derive returned %+v, want an error", got)
			}
			var berr *Error
			if !errors.As(err, &berr) {
				t.Fatalf("error %v is not a *billable.Error", err)
			}
			if berr.Type != EventManual {
				t.Errorf("error names a %s event, want %s", berr.Type, EventManual)
			}
			if !strings.Contains(berr.Msg, c.want) {
				t.Errorf("message %q should mention %q", berr.Msg, c.want)
			}
		})
	}
}

func TestABillingConfigThatReachesIntoInvoicingIsRefused(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"a tax rate", map[string]any{keyRate: rate, "tax_rate": int64(19)}, "tax_rate"},
		{"a tax amount", map[string]any{keyRate: rate, "tax_amount": int64(1900)}, "tax_amount"},
		{"an invoice number", map[string]any{keyRate: rate, "invoice_number": "2026-0001"}, "invoice_number"},
		{"a buyer tax id", map[string]any{keyRate: rate, "buyer_tax_id": "DE123456789"}, "buyer_tax_id"},
		{"a key the config does not carry", map[string]any{"net_days": int64(14)}, "net_days"},
		{"a rate that is not an amount", map[string]any{keyRate: "sixty"}, keyRate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newLedger(t, head(c.cfg)).tryRead()
			if err == nil {
				t.Fatal("Read accepted the config")
			}
			var berr *Error
			if !errors.As(err, &berr) {
				t.Fatalf("error %v is not a *billable.Error", err)
			}
			if berr.Seq != 1 || berr.Type != EventOpened {
				t.Errorf("error = %+v, want the contract head at seq 1", berr)
			}
			if !strings.Contains(berr.Msg, c.want) {
				t.Errorf("message %q should mention %q", berr.Msg, c.want)
			}
		})
	}

	h := head(nil)
	h["billing"] = []any{}
	if _, err := newLedger(t, h).tryRead(); err == nil {
		t.Error("Read accepted a billing value that is not an object")
	}
}

func TestTimeBillsInWholeMinorUnitsOrNotAtAll(t *testing.T) {
	l := newLedger(t, head(billing(8500)))
	l.log("2026-04-06", 60)
	got := l.derive()
	if len(got) != 1 {
		t.Fatalf("derived %+v, want the one month of time", got)
	}
	if e := got[0]; e.Kind != KindTime || e.Amount != 8500 || e.Minutes != 60 || e.Month != "2026-04" || e.Rate != 8500 {
		t.Errorf("event = %+v, want an hour of April at 8500", e)
	}

	part := newLedger(t, head(billing(8500)))
	part.log("2026-04-06", 10)
	if _, err := Derive(part.read()); err == nil {
		t.Error("Derive rounded ten minutes at 8500 per hour")
	} else if !strings.Contains(err.Error(), "whole minor unit") {
		t.Errorf("error %q should say the amount is not a whole minor unit", err)
	}

	free := newLedger(t, head(nil))
	free.log("2026-04-06", 60)
	if _, err := Derive(free.read()); err == nil {
		t.Error("Derive billed time under a contract that names no rate")
	} else if !strings.Contains(err.Error(), "names no rate") {
		t.Errorf("error %q should say the config names no rate", err)
	}
}

func TestAnAcceptedMilestoneTheTermsNoLongerCarryIsRefused(t *testing.T) {
	l := newLedger(t, head(billing(rate)))
	l.deliver("m3")
	l.effective("drop", map[string]any{"op": "remove", "milestone": "m3"})
	f := l.read()
	if err := f.Acceptance.Deem("m3", passed); err != nil {
		t.Fatalf("Deem: %v", err)
	}
	got, err := Derive(f)
	if err == nil {
		t.Fatalf("Derive returned %+v, want an error", got)
	}
	if !strings.Contains(err.Error(), "no longer carry") {
		t.Errorf("error %q should say the terms no longer carry the milestone", err)
	}
}

func TestAnUndeliveredContractDerivesNothing(t *testing.T) {
	if got := newLedger(t, head(billing(rate))).derive(); len(got) != 0 {
		t.Errorf("derived %+v on a contract with no accepted work", got)
	}
}
