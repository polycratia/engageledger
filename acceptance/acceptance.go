// Package acceptance folds the journal into the acceptance state of every
// milestone the contract head carries: delivered while its acceptance window
// runs, accepted when the client signs it off, rejected when the client names
// the criteria it failed, and deemed_accepted when the window runs out. A
// rejection must name at least one criterion of that milestone, each with a
// defect note; a rejection that names none fails no criterion, so Fold lists
// it as refused and the milestone stays delivered with its clock running. A
// valid rejection closes the window, and a later delivery opens a retest
// window that restarts on the full term or resumes the days the closed one
// had left, as the contract head asks. The business-day calendar is not here
// yet: a window counts days, and the move to deemed_accepted takes the
// deadline the clock computes.
package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/polycratia/engageledger/journal"
)

// Event types the acceptance fold reads. An event of any other type passes
// through.
const (
	EventOpened    = "contract.opened"
	EventDelivered = "milestone.delivered"
	EventAccepted  = "milestone.accepted"
	EventRejected  = "milestone.rejected"
)

// Status is where the acceptance of one milestone stands. The zero Status is a
// milestone the journal has not seen delivered.
type Status string

const (
	StatusDelivered      Status = "delivered"
	StatusAccepted       Status = "accepted"
	StatusRejected       Status = "rejected"
	StatusDeemedAccepted Status = "deemed_accepted"
)

// Retest is how the window of a retest is measured.
type Retest string

const (
	// RetestRestart gives a retest the full acceptance window again.
	RetestRestart Retest = "restart"
	// RetestResume gives a retest only the days the closed window had left.
	RetestResume Retest = "resume"
)

// Defect is one criterion a rejection names as failed, with the note that says
// how it failed.
type Defect struct {
	Criterion string
	Note      string
}

// Window is one acceptance window: the delivery that opened it, the business
// days it runs and what closed it. Used is the days the clock had counted when
// a rejection closed it, ClosedBy is the event that closed it, and Deadline is
// the business day the clock named when the window ran out instead.
type Window struct {
	Seq      int64
	Days     int64
	Used     int64
	Retest   bool
	Closed   bool
	ClosedBy int64
	Deadline string
	Defects  []Defect
}

// Remaining is the days the window still had when it closed.
func (w Window) Remaining() int64 {
	if w.Used >= w.Days {
		return 0
	}
	return w.Days - w.Used
}

// Milestone is the acceptance state of one milestone of the statement of work
// and every window it has been through.
type Milestone struct {
	ID       string
	Criteria []string
	Status   Status
	Windows  []Window
}

// Open returns the window whose clock is running, if one is.
func (m Milestone) Open() (Window, bool) {
	if len(m.Windows) == 0 {
		return Window{}, false
	}
	if w := m.Windows[len(m.Windows)-1]; !w.Closed {
		return w, true
	}
	return Window{}, false
}

func (m Milestone) describe() string {
	if m.Status == "" {
		return "not delivered"
	}
	return string(m.Status)
}

// Refusal is an event the fold would not apply, and why. A refused event
// leaves every milestone as it was.
type Refusal struct {
	Seq  int64
	Type string
	Msg  string
}

func (r Refusal) String() string {
	return "event " + strconv.FormatInt(r.Seq, 10) + " " + r.Type + ": " + r.Msg
}

// Acceptance is the acceptance state of the engagement. WindowDays and
// RetestWindow are the acceptance config the contract head carries.
type Acceptance struct {
	WindowDays   int64
	RetestWindow Retest
	Milestones   []Milestone
	Refused      []Refusal
}

// Find returns the acceptance state of one milestone.
func (a Acceptance) Find(id string) (Milestone, bool) {
	i := indexOf(a.Milestones, id)
	if i < 0 {
		return Milestone{}, false
	}
	return a.Milestones[i], true
}

// Deem moves a milestone to deemed_accepted because its open window ran out on
// deadline, the business day the clock computes from the days the window runs.
// It refuses a milestone the contract does not carry, one with no open window,
// and a contract that sets no acceptance window at all.
func (a *Acceptance) Deem(id, deadline string) error {
	if deadline == "" {
		return errors.New("acceptance: the deadline the window ran out on must not be empty")
	}
	if a.WindowDays <= 0 {
		return errors.New("acceptance: the contract sets no acceptance window, so no milestone is deemed accepted")
	}
	i := indexOf(a.Milestones, id)
	if i < 0 {
		return fmt.Errorf("acceptance: milestone %q is not in the contract", id)
	}
	m := &a.Milestones[i]
	if _, open := m.Open(); !open {
		return fmt.Errorf("acceptance: milestone %q is %s, with no open acceptance window", id, m.describe())
	}
	m.Status = StatusDeemedAccepted
	w := &m.Windows[len(m.Windows)-1]
	w.Closed, w.Deadline = true, deadline
	return nil
}

// Fold reads the events in order and returns the acceptance state they add up
// to. Unlike the fold that prices the engagement, a refused event does not
// stop it: the event is listed in Refused and the milestones keep the state
// they had.
func Fold(events []journal.Event) Acceptance {
	var f folder
	for _, e := range events {
		if err := f.apply(e); err != nil {
			f.refused = append(f.refused, Refusal{Seq: e.Seq, Type: e.Type, Msg: err.Error()})
		}
	}
	return Acceptance{
		WindowDays:   f.window,
		RetestWindow: f.retest,
		Milestones:   f.milestones,
		Refused:      f.refused,
	}
}

type folder struct {
	opened     bool
	window     int64
	retest     Retest
	milestones []Milestone
	refused    []Refusal
}

func (f *folder) apply(e journal.Event) error {
	switch e.Type {
	case EventOpened:
		return f.open(e)
	case EventDelivered, EventAccepted, EventRejected:
	default:
		return nil
	}
	if !f.opened {
		return errors.New("arrives before contract.opened")
	}
	i, err := f.locate(e)
	if err != nil {
		return err
	}
	switch e.Type {
	case EventDelivered:
		return f.deliver(e, i)
	case EventAccepted:
		return f.accept(e, i)
	}
	return f.reject(e, i)
}

func (f *folder) open(e journal.Event) error {
	if f.opened {
		return errors.New("opens a contract that is already open")
	}
	milestones, err := readSOW(e.Body)
	if err != nil {
		return err
	}
	window, retest, err := readConfig(e.Body)
	if err != nil {
		return err
	}
	f.opened, f.milestones = true, milestones
	f.window, f.retest = window, retest
	return nil
}

// locate finds the milestone an event names and refuses every event that
// arrives on a milestone whose acceptance is settled.
func (f *folder) locate(e journal.Event) (int, error) {
	id, err := readText(e.Body, "milestone")
	if err != nil {
		return 0, err
	}
	i := indexOf(f.milestones, id)
	if i < 0 {
		return 0, fmt.Errorf("names milestone %q, which the contract does not carry", id)
	}
	if s := f.milestones[i].Status; s == StatusAccepted || s == StatusDeemedAccepted {
		return 0, fmt.Errorf("arrives on milestone %q, which is already %s", id, s)
	}
	return i, nil
}

func (f *folder) deliver(e journal.Event, i int) error {
	m := &f.milestones[i]
	if _, open := m.Open(); open {
		return fmt.Errorf("delivers milestone %q again while its acceptance window is open", m.ID)
	}
	w := Window{Seq: e.Seq, Days: f.window}
	if m.Status == StatusRejected {
		w.Retest = true
		if f.retest == RetestResume {
			w.Days = m.Windows[len(m.Windows)-1].Remaining()
		}
	}
	m.Status = StatusDelivered
	m.Windows = append(m.Windows, w)
	return nil
}

func (f *folder) accept(e journal.Event, i int) error {
	m := &f.milestones[i]
	if _, open := m.Open(); !open {
		return fmt.Errorf("accepts milestone %q, whose acceptance window is not open", m.ID)
	}
	m.Status = StatusAccepted
	w := &m.Windows[len(m.Windows)-1]
	w.Closed, w.ClosedBy = true, e.Seq
	return nil
}

func (f *folder) reject(e journal.Event, i int) error {
	m := &f.milestones[i]
	if _, open := m.Open(); !open {
		return fmt.Errorf("rejects milestone %q, whose acceptance window is not open", m.ID)
	}
	defects, err := readDefects(e.Body, m.ID, m.Criteria)
	if err != nil {
		return err
	}
	used := int64(0)
	if v, ok := e.Body["elapsed_days"]; ok {
		if used, err = readCount(v, "elapsed_days"); err != nil {
			return err
		}
	}
	m.Status = StatusRejected
	w := &m.Windows[len(m.Windows)-1]
	w.Closed, w.ClosedBy, w.Used, w.Defects = true, e.Seq, used, defects
	return nil
}

func readDefects(body map[string]any, id string, criteria []string) ([]Defect, error) {
	items, ok := body["defects"].([]any)
	if !ok {
		return nil, errors.New("carries no defects array, so it names no failed criterion and does not stop the clock")
	}
	if len(items) == 0 {
		return nil, errors.New("names no failed criterion, so it does not stop the clock")
	}
	defects := make([]Defect, 0, len(items))
	for i, item := range items {
		d, err := readDefect(item, id, criteria)
		if err != nil {
			return nil, fmt.Errorf("defect %d %w", i, err)
		}
		if slices.ContainsFunc(defects, func(seen Defect) bool { return seen.Criterion == d.Criterion }) {
			return nil, fmt.Errorf("defect %d repeats criterion %q", i, d.Criterion)
		}
		defects = append(defects, d)
	}
	return defects, nil
}

func readDefect(v any, id string, criteria []string) (Defect, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return Defect{}, errors.New("is not an object")
	}
	criterion, err := readText(obj, "criterion")
	if err != nil {
		return Defect{}, err
	}
	note, err := readText(obj, "note")
	if err != nil {
		return Defect{}, err
	}
	if len(obj) != 2 {
		return Defect{}, errors.New("carries keys beyond criterion and note")
	}
	if !slices.Contains(criteria, criterion) {
		return Defect{}, fmt.Errorf("names criterion %q, which milestone %q does not carry", criterion, id)
	}
	return Defect{Criterion: criterion, Note: note}, nil
}

func readSOW(body map[string]any) ([]Milestone, error) {
	items, ok := body["milestones"].([]any)
	if !ok {
		return nil, errors.New("has no milestones array")
	}
	milestones := make([]Milestone, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("milestone %d is not an object", i)
		}
		id, err := readText(obj, "id")
		if err != nil {
			return nil, fmt.Errorf("milestone %d %w", i, err)
		}
		criteria, err := readCriteria(obj["criteria"])
		if err != nil {
			return nil, fmt.Errorf("milestone %d %w", i, err)
		}
		if indexOf(milestones, id) >= 0 {
			return nil, fmt.Errorf("milestone %d repeats id %q", i, id)
		}
		milestones = append(milestones, Milestone{ID: id, Criteria: criteria})
	}
	return milestones, nil
}

func readConfig(body map[string]any) (int64, Retest, error) {
	v, ok := body["acceptance"]
	if !ok {
		return 0, RetestRestart, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, "", errors.New("has an acceptance value that is not an object")
	}
	window := int64(0)
	if raw, ok := obj["window_days"]; ok {
		var err error
		if window, err = readCount(raw, "window_days"); err != nil {
			return 0, "", err
		}
	}
	retest := RetestRestart
	if raw, ok := obj["retest_window"]; ok {
		s, _ := raw.(string)
		switch Retest(s) {
		case RetestRestart, RetestResume:
			retest = Retest(s)
		default:
			return 0, "", fmt.Errorf("names retest_window %v, want %q or %q", raw, RetestRestart, RetestResume)
		}
	}
	return window, retest, nil
}

func readCriteria(v any) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("has no criteria array")
	}
	criteria := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, errors.New("has a criterion that is not a non-empty string")
		}
		if slices.Contains(criteria, s) {
			return nil, fmt.Errorf("repeats criterion %q", s)
		}
		criteria = append(criteria, s)
	}
	return criteria, nil
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

func readCount(v any, key string) (int64, error) {
	var n int64
	switch t := v.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(t.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("has a %s that is not a whole number of days: %s", key, t.String())
		}
		n = parsed
	case int64:
		n = t
	case int:
		n = int64(t)
	default:
		return 0, fmt.Errorf("has a %s of type %T, want a whole number of days", key, v)
	}
	if n < 0 {
		return 0, fmt.Errorf("has a negative %s: %d", key, n)
	}
	return n, nil
}

func indexOf(milestones []Milestone, id string) int {
	return slices.IndexFunc(milestones, func(m Milestone) bool { return m.ID == id })
}
