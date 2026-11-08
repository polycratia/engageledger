// Package nte keeps the not-to-exceed cap of an engagement and the minutes
// logged against it. A time.logged event carries the minutes worked and the
// date they were worked on; the cap is the nte_cap_minutes of the contract
// head and moves only when a change order carrying a set_nte_cap op is
// approved by every party, which raises it and counts up its version. An
// entry that would carry the logged minutes past the cap in force is refused
// in full, with the minutes that remain named in the error: nothing here
// shortens an entry to make it fit, and a head that names no cap leaves no
// minutes to log. Notices fire at 75% and 90% of the cap as nte.notice events
// whose idempotency key is the hash of the contract, the cap version and the
// threshold, so a replay or a second run appends no notice twice, while a
// raised cap re-arms both thresholds under its new version.
package nte

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/polycratia/engageledger/approval"
	"github.com/polycratia/engageledger/canon"
	"github.com/polycratia/engageledger/journal"
)

// Event types the fold reads. An event of any other type passes through.
const (
	EventOpened    = "contract.opened"
	EventProposed  = "change_order.proposed"
	EventApproved  = "change_order.approved"
	EventDeclined  = "change_order.declined"
	EventWithdrawn = "change_order.withdrawn"
	EventLogged    = "time.logged"
	EventNotice    = "nte.notice"
)

// OpSetCap is the change order op that raises the cap, in minutes.
const OpSetCap = "set_nte_cap"

const maxMinutes = 1<<53 - 1

var thresholds = []int64{75, 90}

// Thresholds are the percentages of the cap a notice fires at.
func Thresholds() []int64 { return slices.Clone(thresholds) }

// Cap is the cap in force: its minutes, the version every raise counts up and
// the event that set it.
type Cap struct {
	Minutes int64
	Version int64
	Seq     int64
}

// Entry is one time.logged event: the minutes worked and the YYYY-MM-DD date
// they were worked on.
type Entry struct {
	Seq     int64
	Date    string
	Minutes int64
}

// Notice is one nte.notice event: the threshold it fired at, the cap version
// it fired under, the idempotency key of the two and the minutes logged when
// it fired.
type Notice struct {
	Seq        int64
	CapVersion int64
	Threshold  int64
	Key        string
	Logged     int64
}

// NTE is the cap of the engagement and what is logged against it. Contract is
// the hash of the contract.opened line, which every notice names.
type NTE struct {
	Contract string
	Cap      Cap
	Logged   int64
	Entries  []Entry
	Notices  []Notice
}

// Error reports the event Fold refused.
type Error struct {
	Seq  int64
	Type string
	Msg  string
}

func (e *Error) Error() string {
	return "nte: event " + strconv.FormatInt(e.Seq, 10) + " " + e.Type + ": " + e.Msg
}

// Fold reads the events in order and returns the cap they add up to, or an
// *Error naming the first event it refuses.
func Fold(events []journal.Event) (NTE, error) {
	f := folder{orders: map[string]*order{}, seen: map[string]int64{}}
	for _, e := range events {
		if err := f.apply(e); err != nil {
			return NTE{}, &Error{Seq: e.Seq, Type: e.Type, Msg: err.Error()}
		}
	}
	return f.nte(), nil
}

// Remaining is the minutes the cap in force still leaves.
func (n NTE) Remaining() int64 { return n.Cap.Minutes - n.Logged }

// Pending are the thresholds the logged minutes have reached under the cap in
// force that no notice of this cap version names yet, in rising order.
func (n NTE) Pending() []int64 {
	var out []int64
	for _, t := range thresholds {
		if n.reached(t) && !n.noticed(t) {
			out = append(out, t)
		}
	}
	return out
}

// EntryBody is the time.logged body of minutes worked on date. It refuses an
// entry that would carry the logged minutes past the cap in force, naming the
// minutes that remain, and refuses it whole.
func (n NTE) EntryBody(date string, minutes int64) (map[string]any, error) {
	if !isDate(date) {
		return nil, fmt.Errorf("nte: %q is not a YYYY-MM-DD date", date)
	}
	if minutes <= 0 {
		return nil, fmt.Errorf("nte: an entry of %d minutes logs nothing", minutes)
	}
	if err := fits(n.Logged, minutes, n.Cap, date); err != nil {
		return nil, fmt.Errorf("nte: %w", err)
	}
	return map[string]any{"date": date, "minutes": minutes}, nil
}

// Log appends the time.logged event of minutes worked on date. An entry that
// crosses the cap leaves the journal untouched.
func (n NTE) Log(ap *journal.Appender, date string, minutes int64) (journal.Event, error) {
	body, err := n.EntryBody(date, minutes)
	if err != nil {
		return journal.Event{}, err
	}
	return ap.Append(EventLogged, body)
}

// NoticeBody is the nte.notice body of one threshold under the cap in force,
// carrying the idempotency key that keeps a second run from firing it again.
func (n NTE) NoticeBody(threshold int64) (map[string]any, error) {
	if !slices.Contains(thresholds, threshold) {
		return nil, fmt.Errorf("nte: %d%% is not a notice threshold", threshold)
	}
	key, err := NoticeKey(n.Contract, n.Cap.Version, threshold)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"contract":    n.Contract,
		"cap_version": n.Cap.Version,
		"threshold":   threshold,
		"key":         key,
	}, nil
}

// Notify appends one nte.notice for every pending threshold. A run that finds
// every reached threshold already noticed leaves the journal untouched.
func (n NTE) Notify(ap *journal.Appender) ([]journal.Event, error) {
	var out []journal.Event
	for _, t := range n.Pending() {
		body, err := n.NoticeBody(t)
		if err != nil {
			return out, err
		}
		e, err := ap.Append(EventNotice, body)
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}

// NoticeKey is the idempotency key of the notice fired at threshold percent of
// one cap version of one contract: the hex SHA-256 of the canonical JSON of
// the three, so every run computes the same key.
func NoticeKey(contract string, version, threshold int64) (string, error) {
	b, err := canon.Marshal(map[string]any{
		"contract":    contract,
		"cap_version": version,
		"threshold":   threshold,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (n NTE) reached(threshold int64) bool {
	return n.Cap.Minutes > 0 && n.Logged*100 >= n.Cap.Minutes*threshold
}

func (n NTE) noticed(threshold int64) bool {
	return slices.ContainsFunc(n.Notices, func(x Notice) bool {
		return x.Threshold == threshold && x.CapVersion == n.Cap.Version
	})
}

// order is a change order that carries a set_nte_cap op, with the parties that
// have approved it so far.
type order struct {
	seq       int64
	body      map[string]any
	minutes   int64
	state     string
	approvals []string
}

type folder struct {
	opened   bool
	contract string
	parties  []string
	keys     approval.Keys
	cap      Cap
	logged   int64
	entries  []Entry
	notices  []Notice
	seen     map[string]int64
	orders   map[string]*order
}

func (f *folder) apply(e journal.Event) error {
	switch e.Type {
	case EventOpened:
		return f.open(e)
	case EventProposed, EventApproved, EventDeclined, EventWithdrawn, EventLogged, EventNotice:
		if !f.opened {
			return errors.New("arrives before contract.opened")
		}
	default:
		return nil
	}
	switch e.Type {
	case EventProposed:
		return f.propose(e)
	case EventApproved:
		return f.approve(e)
	case EventDeclined:
		return f.settle(e, "declined")
	case EventWithdrawn:
		return f.settle(e, "withdrawn")
	case EventLogged:
		return f.log(e)
	}
	return f.notice(e)
}

func (f *folder) open(e journal.Event) error {
	if f.opened {
		return errors.New("opens a contract that is already open")
	}
	parties, err := readParties(e.Body)
	if err != nil {
		return err
	}
	keys, err := approval.ReadKeys(e.Body)
	if err != nil {
		return err
	}
	minutes := int64(0)
	if v, ok := e.Body["nte_cap_minutes"]; ok {
		if minutes, err = readInt(v, "nte_cap_minutes"); err != nil {
			return err
		}
	}
	f.opened = true
	f.contract, f.parties, f.keys = e.Hash, parties, keys
	f.cap = Cap{Minutes: minutes, Version: 1, Seq: e.Seq}
	return nil
}

// propose lists a change order that carries a set_nte_cap op. One that carries
// none moves no cap, so it passes through for the terms fold to price.
func (f *folder) propose(e journal.Event) error {
	items, ok := e.Body["ops"].([]any)
	if !ok {
		return nil
	}
	minutes, found := int64(0), false
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := obj["op"].(string); kind != OpSetCap {
			continue
		}
		if found {
			return fmt.Errorf("carries more than one %s op", OpSetCap)
		}
		m, err := readInt(obj["minutes"], "minutes")
		if err != nil {
			return fmt.Errorf("op %d %w", i, err)
		}
		minutes, found = m, true
	}
	if !found {
		return nil
	}
	if e.Hash == "" {
		return errors.New("carries no line hash, so no approval could name it")
	}
	if other, ok := f.orders[e.Hash]; ok {
		return fmt.Errorf("repeats the hash of the change order of event %d", other.seq)
	}
	f.orders[e.Hash] = &order{seq: e.Seq, body: e.Body, minutes: minutes}
	return nil
}

// approve records one party's approval of a cap change order. The approval
// that leaves no party without one raises the cap and counts up its version.
func (f *folder) approve(e journal.Event) error {
	o, hash, err := f.decision(e)
	if err != nil || o == nil {
		return err
	}
	by, err := readText(e.Body, "by")
	if err != nil {
		return err
	}
	if !slices.Contains(f.parties, by) {
		return fmt.Errorf("is signed by %q, who is not a party of the contract", by)
	}
	if slices.Contains(o.approvals, by) {
		return fmt.Errorf("repeats the approval of %q on change order %s", by, short(hash))
	}
	if _, ok := e.Body["approval"]; ok {
		a, err := approval.Read(e.Body)
		if err != nil {
			return err
		}
		if a.Party != by {
			return fmt.Errorf("carries an approval by %q on an event signed by %q", a.Party, by)
		}
		if err := approval.Verify(a, o.body, f.keys); err != nil {
			return fmt.Errorf("carries an approval that %w", err)
		}
	}
	o.approvals = append(o.approvals, by)
	if len(f.parties) == 0 || len(o.approvals) < len(f.parties) {
		return nil
	}
	if o.minutes <= f.cap.Minutes {
		return fmt.Errorf("makes change order %s effective, which sets the cap to %d minutes, at or below the %d minutes it already stands at",
			short(hash), o.minutes, f.cap.Minutes)
	}
	o.state = "effective"
	f.cap = Cap{Minutes: o.minutes, Version: f.cap.Version + 1, Seq: e.Seq}
	return nil
}

func (f *folder) settle(e journal.Event, state string) error {
	o, _, err := f.decision(e)
	if err != nil || o == nil {
		return err
	}
	o.state = state
	return nil
}

// decision reads the change order one decision names. A hash no cap change
// order carries returns no order, which leaves the event to the terms fold.
func (f *folder) decision(e journal.Event) (*order, string, error) {
	hash, ok := e.Body["change_order"].(string)
	if !ok {
		return nil, "", nil
	}
	o, found := f.orders[hash]
	if !found {
		return nil, "", nil
	}
	if o.state != "" {
		return nil, "", fmt.Errorf("decides change order %s again: it is already %s", short(hash), o.state)
	}
	return o, hash, nil
}

func (f *folder) log(e journal.Event) error {
	date, err := readDate(e.Body, "date")
	if err != nil {
		return err
	}
	minutes, err := readInt(e.Body["minutes"], "minutes")
	if err != nil {
		return err
	}
	if minutes == 0 {
		return errors.New("logs no minutes")
	}
	if err := fits(f.logged, minutes, f.cap, date); err != nil {
		return err
	}
	f.logged += minutes
	f.entries = append(f.entries, Entry{Seq: e.Seq, Date: date, Minutes: minutes})
	return nil
}

// notice reads one notice back. Its idempotency key is what the fold checks:
// the same contract, cap version and threshold compute the same key, so a
// second notice for them is refused rather than counted twice.
func (f *folder) notice(e journal.Event) error {
	if len(e.Body) != 4 {
		return fmt.Errorf("carries %d keys, want contract, cap_version, threshold and key", len(e.Body))
	}
	contract, err := readText(e.Body, "contract")
	if err != nil {
		return err
	}
	if contract != f.contract {
		return fmt.Errorf("names contract %s, while the journal opened on %s", short(contract), short(f.contract))
	}
	threshold, err := readInt(e.Body["threshold"], "threshold")
	if err != nil {
		return err
	}
	if !slices.Contains(thresholds, threshold) {
		return fmt.Errorf("fires at %d%%, which is not a notice threshold", threshold)
	}
	version, err := readInt(e.Body["cap_version"], "cap_version")
	if err != nil {
		return err
	}
	if version != f.cap.Version {
		return fmt.Errorf("names cap version %d, while the cap stands at version %d", version, f.cap.Version)
	}
	key, err := readText(e.Body, "key")
	if err != nil {
		return err
	}
	want, err := NoticeKey(f.contract, version, threshold)
	if err != nil {
		return err
	}
	if key != want {
		return fmt.Errorf("carries idempotency key %s, want %s for %d%% of cap version %d",
			short(key), short(want), threshold, version)
	}
	if seq, ok := f.seen[key]; ok {
		return fmt.Errorf("repeats the notice event %d appended for %d%% of cap version %d", seq, threshold, version)
	}
	if f.cap.Minutes <= 0 || f.logged*100 < f.cap.Minutes*threshold {
		return fmt.Errorf("fires at %d%% of the cap with %d of %d minutes logged", threshold, f.logged, f.cap.Minutes)
	}
	f.seen[key] = e.Seq
	f.notices = append(f.notices, Notice{
		Seq:        e.Seq,
		CapVersion: version,
		Threshold:  threshold,
		Key:        key,
		Logged:     f.logged,
	})
	return nil
}

func (f *folder) nte() NTE {
	return NTE{
		Contract: f.contract,
		Cap:      f.cap,
		Logged:   f.logged,
		Entries:  slices.Clone(f.entries),
		Notices:  slices.Clone(f.notices),
	}
}

// fits reports the refusal of an entry that would carry the logged minutes
// past the cap, which names the minutes that remain under it.
func fits(logged, minutes int64, c Cap, date string) error {
	if logged+minutes <= c.Minutes {
		return nil
	}
	return fmt.Errorf("logs %d minutes on %s, which crosses the not-to-exceed cap of %d minutes: %d minutes remain under cap version %d",
		minutes, date, c.Minutes, c.Minutes-logged, c.Version)
}

// readParties reads the parties object of a contract.opened body, one party
// name per role. A head that names none reads as no parties, which leaves
// every cap change order one approval short of effective.
func readParties(body map[string]any) ([]string, error) {
	v, ok := body["parties"]
	if !ok {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("has a parties value that is not an object")
	}
	parties := make([]string, 0, len(obj))
	for role, raw := range obj {
		name, ok := raw.(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("names the %s with something other than a non-empty string", role)
		}
		if slices.Contains(parties, name) {
			return nil, fmt.Errorf("names %q as more than one party", name)
		}
		parties = append(parties, name)
	}
	slices.Sort(parties)
	return parties, nil
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

func readDate(m map[string]any, key string) (string, error) {
	s, err := readText(m, key)
	if err != nil {
		return "", err
	}
	if !isDate(s) {
		return "", fmt.Errorf("has a %s that is not a YYYY-MM-DD date: %q", key, s)
	}
	return s, nil
}

func isDate(s string) bool {
	at, err := time.Parse(time.DateOnly, s)
	return err == nil && at.Format(time.DateOnly) == s
}

func readInt(v any, key string) (int64, error) {
	var n int64
	switch t := v.(type) {
	case nil:
		return 0, fmt.Errorf("has no %s", key)
	case json.Number:
		parsed, err := strconv.ParseInt(t.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("has a %s that is not an integer: %s", key, t.String())
		}
		n = parsed
	case int64:
		n = t
	case int:
		n = int64(t)
	default:
		return 0, fmt.Errorf("has a %s of type %T, want an integer", key, v)
	}
	if n < 0 {
		return 0, fmt.Errorf("has a negative %s: %d", key, n)
	}
	if n > maxMinutes {
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
