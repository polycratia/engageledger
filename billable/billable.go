// Package billable derives the money an invoice would rest on from the
// journal, and stops there. Read folds the journal into the terms, the
// acceptance state and the not-to-exceed cap of the engagement, and Derive
// emits the billable events they add up to: one per accepted or
// deemed_accepted milestone at the price the terms carry, one per calendar
// month of logged time in the zone the contract counts days in, billed at the
// rate the billing config of the head names, and one per manual line that
// references an effective change order. Only the minutes the cap let into
// nte.Fold are billed, and an amount that does not come out in whole minor
// units is refused rather than rounded. Every event carries its amount in
// minor units of the one contract currency and nothing else about money: no
// tax rate, no tax amount, no invoice number and no buyer tax id, and a
// billing config that sets any of them is refused, because invoicing is where
// this library stops.
package billable

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/polycratia/engageledger/acceptance"
	"github.com/polycratia/engageledger/journal"
	"github.com/polycratia/engageledger/nte"
	"github.com/polycratia/engageledger/terms"
)

// Event types this package reads itself. The rest of the journal reaches it
// through the folds of the other packages.
const (
	EventOpened = "contract.opened"
	EventManual = "billable.manual"
)

// keyRate is the only key the billing config of the contract head carries:
// the rate of logged time, in minor units per hour.
const keyRate = "rate_minor_per_hour"

const (
	maxAmount      = 1<<53 - 1
	minutesPerHour = 60
	monthLayout    = "2006-01"
)

// invoicing are the keys that belong to an invoice rather than to a billable
// event.
var invoicing = []string{"tax_rate", "tax_amount", "invoice_number", "buyer_tax_id"}

// Kind is what one billable event bills for.
type Kind string

const (
	// KindMilestone bills an accepted or deemed_accepted milestone.
	KindMilestone Kind = "milestone"
	// KindTime bills one calendar month of logged time.
	KindTime Kind = "time"
	// KindManual bills a line an effective change order carries.
	KindManual Kind = "manual"
)

// Event is one billable line: an amount in minor units of the contract
// currency and what it bills for. Milestone is filled in on a milestone
// event, Month, Minutes and Rate on a time event, ChangeOrder and Note on a
// manual one.
type Event struct {
	Kind        Kind
	Currency    string
	Amount      int64
	Milestone   string
	Month       string
	Minutes     int64
	Rate        int64
	ChangeOrder string
	Note        string
}

// Object is the event as a JSON value, carrying only the keys its kind uses.
func (e Event) Object() map[string]any {
	obj := map[string]any{
		"kind":     string(e.Kind),
		"currency": e.Currency,
		"amount":   e.Amount,
	}
	switch e.Kind {
	case KindMilestone:
		obj["milestone"] = e.Milestone
	case KindTime:
		obj["month"], obj["minutes"], obj[keyRate] = e.Month, e.Minutes, e.Rate
	case KindManual:
		obj["change_order"], obj["note"] = e.ChangeOrder, e.Note
	}
	return obj
}

// Config is what the derivation needs beyond the folds: the currency the
// contract prices in, the rate logged time bills at in minor units per hour,
// and the zone the client reads calendar months in.
type Config struct {
	Currency string
	Rate     int64
	Zone     string
}

// Manual is one billable.manual line of the journal: an amount, the note that
// says what it is for and the change order that authorises it.
type Manual struct {
	Seq         int64
	Amount      int64
	Note        string
	ChangeOrder string
}

// Fold is the journal read into everything Derive bills from.
type Fold struct {
	Config     Config
	Terms      terms.Terms
	Acceptance acceptance.Acceptance
	NTE        nte.NTE
	Manual     []Manual
}

// Error reports the event Read or Derive refused.
type Error struct {
	Seq  int64
	Type string
	Msg  string
}

func (e *Error) Error() string {
	return "billable: event " + strconv.FormatInt(e.Seq, 10) + " " + e.Type + ": " + e.Msg
}

// Read folds the events into the terms, the acceptance state and the cap, and
// reads the billing config of the contract head with the manual lines of the
// journal. It returns the error of the first fold that refuses an event.
func Read(events []journal.Event) (Fold, error) {
	t, err := terms.Fold(events)
	if err != nil {
		return Fold{}, err
	}
	n, err := nte.Fold(events)
	if err != nil {
		return Fold{}, err
	}
	a := acceptance.Fold(events)
	f := Fold{
		Config:     Config{Currency: t.Currency, Zone: a.Calendar.Zone},
		Terms:      t,
		Acceptance: a,
		NTE:        n,
	}
	opened := false
	for _, e := range events {
		switch e.Type {
		case EventOpened:
			rate, err := readConfig(e.Body)
			if err != nil {
				return Fold{}, &Error{Seq: e.Seq, Type: e.Type, Msg: err.Error()}
			}
			f.Config.Rate, opened = rate, true
		case EventManual:
			if !opened {
				return Fold{}, &Error{Seq: e.Seq, Type: e.Type, Msg: "arrives before contract.opened"}
			}
			m, err := readManual(e.Body)
			if err != nil {
				return Fold{}, &Error{Seq: e.Seq, Type: e.Type, Msg: err.Error()}
			}
			m.Seq = e.Seq
			f.Manual = append(f.Manual, m)
		}
	}
	return f, nil
}

// Derive emits the billable events of the fold: the accepted milestones in the
// order the terms carry them, the months of logged time in rising order, then
// the manual lines in journal order.
func Derive(f Fold) ([]Event, error) {
	out, err := f.milestoneEvents()
	if err != nil {
		return nil, err
	}
	months, err := f.timeEvents()
	if err != nil {
		return nil, err
	}
	manual, err := f.manualEvents()
	if err != nil {
		return nil, err
	}
	return append(append(out, months...), manual...), nil
}

// ManualBody is the billable.manual body of a line of amount minor units that
// references the change order of hash changeOrder.
func ManualBody(amount int64, note, changeOrder string) (map[string]any, error) {
	body := map[string]any{"amount": amount, "note": note, "change_order": changeOrder}
	if _, err := readManual(body); err != nil {
		return nil, fmt.Errorf("billable: a manual line %w", err)
	}
	return body, nil
}

func (f Fold) milestoneEvents() ([]Event, error) {
	var out []Event
	for _, m := range f.Terms.Milestones {
		s, ok := f.Acceptance.Find(m.ID)
		if !ok || !billableStatus(s.Status) {
			continue
		}
		out = append(out, Event{
			Kind:      KindMilestone,
			Currency:  f.Config.Currency,
			Amount:    m.Amount,
			Milestone: m.ID,
		})
	}
	for _, m := range f.Acceptance.Milestones {
		if !billableStatus(m.Status) {
			continue
		}
		if slices.ContainsFunc(f.Terms.Milestones, func(x terms.Milestone) bool { return x.ID == m.ID }) {
			continue
		}
		return nil, fmt.Errorf("billable: milestone %q is %s, which the terms no longer carry, so it has no price to bill", m.ID, m.Status)
	}
	return out, nil
}

func (f Fold) timeEvents() ([]Event, error) {
	if len(f.NTE.Entries) == 0 {
		return nil, nil
	}
	if f.Config.Rate <= 0 {
		return nil, fmt.Errorf("billable: %d minutes are logged under a contract whose billing config names no rate", f.NTE.Logged)
	}
	loc := time.UTC
	if f.Config.Zone != "" {
		zone, err := time.LoadLocation(f.Config.Zone)
		if err != nil {
			return nil, fmt.Errorf("billable: the contract counts days in zone %q: %w", f.Config.Zone, err)
		}
		loc = zone
	}
	minutes := map[string]int64{}
	for _, entry := range f.NTE.Entries {
		day, err := time.ParseInLocation(time.DateOnly, entry.Date, loc)
		if err != nil {
			return nil, fmt.Errorf("billable: the entry of event %d is dated %q: %w", entry.Seq, entry.Date, err)
		}
		minutes[day.Format(monthLayout)] += entry.Minutes
	}
	out := make([]Event, 0, len(minutes))
	for _, month := range slices.Sorted(maps.Keys(minutes)) {
		amount, err := bill(minutes[month], f.Config.Rate, month)
		if err != nil {
			return nil, err
		}
		out = append(out, Event{
			Kind:     KindTime,
			Currency: f.Config.Currency,
			Amount:   amount,
			Month:    month,
			Minutes:  minutes[month],
			Rate:     f.Config.Rate,
		})
	}
	return out, nil
}

func (f Fold) manualEvents() ([]Event, error) {
	var out []Event
	for _, m := range f.Manual {
		i := slices.IndexFunc(f.Terms.ChangeOrders, func(co terms.ChangeOrder) bool { return co.Hash == m.ChangeOrder })
		if i < 0 {
			return nil, &Error{Seq: m.Seq, Type: EventManual, Msg: fmt.Sprintf(
				"references change order %s, which no change_order.proposed carries", short(m.ChangeOrder))}
		}
		if co := f.Terms.ChangeOrders[i]; co.State != terms.StateEffective {
			return nil, &Error{Seq: m.Seq, Type: EventManual, Msg: fmt.Sprintf(
				"references change order %s, which is %s, not effective", short(co.Hash), co.State)}
		}
		out = append(out, Event{
			Kind:        KindManual,
			Currency:    f.Config.Currency,
			Amount:      m.Amount,
			ChangeOrder: m.ChangeOrder,
			Note:        m.Note,
		})
	}
	return out, nil
}

func billableStatus(s acceptance.Status) bool {
	return s == acceptance.StatusAccepted || s == acceptance.StatusDeemedAccepted
}

// bill is the money a month of logged minutes comes to at a rate per hour. An
// amount that does not land on a whole minor unit is refused: how to round it
// is an invoicing decision, which this library does not take.
func bill(minutes, rate int64, month string) (int64, error) {
	if minutes > maxAmount/rate {
		return 0, fmt.Errorf("billable: the %d minutes logged in %s at %d per hour are outside the exact integer range", minutes, month, rate)
	}
	product := minutes * rate
	if product%minutesPerHour != 0 {
		return 0, fmt.Errorf("billable: the %d minutes logged in %s at %d per hour come to %d/%d, which is not a whole minor unit",
			minutes, month, rate, product, minutesPerHour)
	}
	return product / minutesPerHour, nil
}

// readConfig reads the billing config of the contract head, which carries the
// rate and nothing else. A config that sets a tax rate, a tax amount, an
// invoice number or a buyer tax id is refused: those belong to an invoice,
// which is past where this library stops.
func readConfig(body map[string]any) (int64, error) {
	v, ok := body["billing"]
	if !ok {
		return 0, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, errors.New("has a billing value that is not an object")
	}
	for _, key := range invoicing {
		if _, carries := obj[key]; carries {
			return 0, fmt.Errorf("sets %s in its billing config, which is invoicing: a billable event carries no tax rate, tax amount, invoice number or buyer tax id", key)
		}
	}
	rate := int64(0)
	for key, raw := range obj {
		if key != keyRate {
			return 0, fmt.Errorf("sets %s in its billing config, which carries %s alone", key, keyRate)
		}
		var err error
		if rate, err = readAmount(raw, key); err != nil {
			return 0, err
		}
	}
	return rate, nil
}

func readManual(body map[string]any) (Manual, error) {
	if len(body) != 3 {
		return Manual{}, fmt.Errorf("carries %d keys, want amount, note and change_order", len(body))
	}
	amount, err := readAmount(body["amount"], "amount")
	if err != nil {
		return Manual{}, err
	}
	if amount == 0 {
		return Manual{}, errors.New("bills no minor units")
	}
	note, err := readText(body, "note")
	if err != nil {
		return Manual{}, err
	}
	hash, err := readText(body, "change_order")
	if err != nil {
		return Manual{}, err
	}
	return Manual{Amount: amount, Note: note, ChangeOrder: hash}, nil
}

func readText(m map[string]any, key string) (string, error) {
	s, ok := m[key].(string)
	if !ok {
		return "", fmt.Errorf("has no %s string", key)
	}
	if s == "" {
		return "", fmt.Errorf("has an empty %s", key)
	}
	return s, nil
}

func readAmount(v any, key string) (int64, error) {
	var n int64
	switch t := v.(type) {
	case nil:
		return 0, fmt.Errorf("has no %s", key)
	case json.Number:
		parsed, err := strconv.ParseInt(t.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("has a %s that is not an integer in minor units: %s", key, t.String())
		}
		n = parsed
	case int64:
		n = t
	case int:
		n = int64(t)
	default:
		return 0, fmt.Errorf("has a %s of type %T, want an integer in minor units", key, v)
	}
	if n < 0 {
		return 0, fmt.Errorf("has a negative %s: %d", key, n)
	}
	if n > maxAmount {
		return 0, fmt.Errorf("has a %s outside the exact integer range: %d", key, n)
	}
	return n, nil
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
