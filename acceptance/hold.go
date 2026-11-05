package acceptance

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/polycratia/engageledger/approval"
	"github.com/polycratia/engageledger/bizday"
	"github.com/polycratia/engageledger/journal"
)

// Hold is one pause of the acceptance clock of a milestone: the dates it
// covers, the business days of the calendar the paused window counts on that
// fall between them, and the approvals behind it. A hold is effective only
// once every party of the contract has approved it, and ExtendedBy is the
// deadline.extended event that recorded the deadline it moved the window to.
type Hold struct {
	Seq         int64
	ID          string
	Milestone   string
	Start       string
	End         string
	Days        int64
	Approvals   []approval.Approval
	Effective   bool
	EffectiveBy int64
	ExtendedBy  int64
}

// Approved reports whether party has approved the hold.
func (h Hold) Approved(party string) bool {
	return slices.ContainsFunc(h.Approvals, func(a approval.Approval) bool { return a.Party == party })
}

func (h Hold) covers(start, end string) bool {
	return h.Start <= end && start <= h.End
}

// Hold returns one hold of the journal by the id its events name it by.
func (a Acceptance) Hold(id string) (Hold, bool) {
	i := holdIndex(a.Holds, id)
	if i < 0 {
		return Hold{}, false
	}
	return a.Holds[i], true
}

// HoldBody is the hold.proposed body of a hold of id that pauses the
// acceptance clock of milestone from start through end, both YYYY-MM-DD dates.
// It is the body each party approves.
func HoldBody(id, milestone, start, end string) map[string]any {
	return map[string]any{"hold": id, "milestone": milestone, "start": start, "end": end}
}

// ApprovalBody is the hold.approved body carrying one party's approval of the
// hold.proposed body of id.
func ApprovalBody(id string, a approval.Approval) map[string]any {
	return map[string]any{"hold": id, "approval": a.Object()}
}

// Extension is the deadline.extended body of the hold id, computed once with
// the calendar version the paused window recorded: the close of the business
// day the term of that window reaches once the days the hold covered are added
// to it. It refuses a hold the journal does not carry, one the parties have not
// all approved, one already extended, and a milestone with no open window.
func (a Acceptance) Extension(id string) (map[string]any, error) {
	h, ok := a.Hold(id)
	if !ok {
		return nil, fmt.Errorf("acceptance: hold %q is not in the journal", id)
	}
	if !h.Effective {
		return nil, fmt.Errorf("acceptance: hold %q carries %d approvals, so it pauses nothing", id, len(h.Approvals))
	}
	if h.ExtendedBy > 0 {
		return nil, fmt.Errorf("acceptance: event %d already extended the deadline on hold %q", h.ExtendedBy, id)
	}
	m, ok := a.Find(h.Milestone)
	if !ok {
		return nil, fmt.Errorf("acceptance: milestone %q is not in the contract", h.Milestone)
	}
	w, open := m.Open()
	if !open {
		return nil, fmt.Errorf("acceptance: milestone %q is %s, with no open acceptance window to extend", m.ID, m.describe())
	}
	v, ok := versionOf(a.Calendars, w.CalendarHash)
	if !ok {
		return nil, fmt.Errorf("acceptance: the window of milestone %q counts on calendar %s, which the journal does not carry",
			m.ID, short(w.CalendarHash))
	}
	deadline, err := v.Calendar.Deadline(w.Delivered, w.Days+w.Held+h.Days)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"milestone":     m.ID,
		"hold":          id,
		"deadline_utc":  deadline.Format(time.RFC3339),
		"calendar_hash": w.CalendarHash,
	}, nil
}

// Extend appends the deadline.extended event of the hold id. A hold that pauses
// nothing leaves the journal untouched.
func (a Acceptance) Extend(ap *journal.Appender, id string) (journal.Event, error) {
	body, err := a.Extension(id)
	if err != nil {
		return journal.Event{}, err
	}
	return ap.Append(EventExtended, body)
}

// hold proposes a pause of the open window of a milestone. The proposal alone
// pauses nothing: it is listed until the approvals of every party make it
// effective.
func (f *folder) hold(e journal.Event, i int) error {
	m := &f.milestones[i]
	w, open := m.Open()
	if !open {
		return fmt.Errorf("holds milestone %q, whose acceptance window is not open", m.ID)
	}
	id, err := readText(e.Body, "hold")
	if err != nil {
		return err
	}
	if j := holdIndex(f.holds, id); j >= 0 {
		return fmt.Errorf("proposes hold %q again, which event %d already proposed", id, f.holds[j].Seq)
	}
	start, err := readDate(e.Body, "start")
	if err != nil {
		return err
	}
	end, err := readDate(e.Body, "end")
	if err != nil {
		return err
	}
	if end.Before(start) {
		return fmt.Errorf("ends on %s, before it starts on %s", day(end), day(start))
	}
	if w.Deadline.IsZero() {
		return fmt.Errorf("pauses the window of milestone %q, whose delivery fixed no deadline", m.ID)
	}
	v, _ := versionOf(f.calendars, w.CalendarHash)
	begins, err := dayStart(v.Calendar, start)
	if err != nil {
		return err
	}
	if begins.After(w.Deadline) {
		return fmt.Errorf("starts on %s, after the acceptance window of milestone %q ran out on %s",
			day(start), m.ID, w.Deadline.Format(time.RFC3339))
	}
	if other, ok := f.overlap(m.ID, day(start), day(end)); ok {
		return fmt.Errorf("covers %s..%s, which overlaps hold %q of milestone %q, effective since event %d",
			day(start), day(end), other.ID, m.ID, other.EffectiveBy)
	}
	if f.proposals == nil {
		f.proposals = map[string]map[string]any{}
	}
	f.proposals[id] = e.Body
	f.holds = append(f.holds, Hold{
		Seq:       e.Seq,
		ID:        id,
		Milestone: m.ID,
		Start:     day(start),
		End:       day(end),
		Days:      businessDays(v.Calendar, start, end),
	})
	return nil
}

// approveHold records one party's approval of a hold, evidenced as the approval
// of a change order is. The hold becomes effective on the approval that leaves
// no party without one.
func (f *folder) approveHold(e journal.Event) error {
	id, err := readText(e.Body, "hold")
	if err != nil {
		return err
	}
	j := holdIndex(f.holds, id)
	if j < 0 {
		return fmt.Errorf("approves hold %q, which no hold.proposed before it carries", id)
	}
	h := &f.holds[j]
	if h.Effective {
		return fmt.Errorf("approves hold %q, which every party already approved at event %d", id, h.EffectiveBy)
	}
	a, err := approval.Read(e.Body)
	if err != nil {
		return err
	}
	if !slices.Contains(f.parties, a.Party) {
		return fmt.Errorf("approves for %q, who is not a party of the contract", a.Party)
	}
	if h.Approved(a.Party) {
		return fmt.Errorf("approves hold %q for %q a second time", id, a.Party)
	}
	if err := approval.Verify(a, f.proposals[id], f.keys); err != nil {
		return fmt.Errorf("carries an approval that %w", err)
	}
	last := len(h.Approvals)+1 == len(f.parties)
	if last {
		if other, ok := f.overlap(h.Milestone, h.Start, h.End); ok {
			return fmt.Errorf("makes hold %q effective over %s..%s, which overlaps hold %q, effective since event %d",
				id, h.Start, h.End, other.ID, other.EffectiveBy)
		}
	}
	h.Approvals = append(h.Approvals, a)
	if last {
		h.Effective, h.EffectiveBy = true, e.Seq
	}
	return nil
}

// extend reads the deadline a hold moved its window to. The instant enters the
// window as recorded, the way a delivery deadline does, and Verify is what
// reads it back against the term the window and its holds add up to.
func (f *folder) extend(e journal.Event, i int) error {
	m := &f.milestones[i]
	w, open := m.Open()
	if !open {
		return fmt.Errorf("extends the deadline of milestone %q, whose acceptance window is not open", m.ID)
	}
	id, err := readText(e.Body, "hold")
	if err != nil {
		return err
	}
	j := holdIndex(f.holds, id)
	if j < 0 {
		return fmt.Errorf("extends on hold %q, which no hold.proposed before it carries", id)
	}
	h := &f.holds[j]
	if h.Milestone != m.ID {
		return fmt.Errorf("names milestone %q, while hold %q pauses %q", m.ID, id, h.Milestone)
	}
	if !h.Effective {
		return fmt.Errorf("extends on hold %q, which %d of the %d parties approved, so it pauses nothing",
			id, len(h.Approvals), len(f.parties))
	}
	if h.ExtendedBy > 0 {
		return fmt.Errorf("extends on hold %q again, which event %d already extended", id, h.ExtendedBy)
	}
	if h.Seq < w.Seq {
		return fmt.Errorf("extends on hold %q, which was proposed before the window at event %d opened", id, w.Seq)
	}
	hash, err := readText(e.Body, "calendar_hash")
	if err != nil {
		return err
	}
	if hash != w.CalendarHash {
		return fmt.Errorf("counts on calendar %s, while the window of milestone %q recorded %s",
			short(hash), m.ID, short(w.CalendarHash))
	}
	deadline, err := readInstant(e.Body, "deadline_utc")
	if err != nil {
		return err
	}
	if deadline.Before(w.Deadline) {
		return fmt.Errorf("moves the deadline of milestone %q back from %s to %s, which a pause cannot do",
			m.ID, w.Deadline.Format(time.RFC3339), deadline.Format(time.RFC3339))
	}
	cur := &m.Windows[len(m.Windows)-1]
	cur.Held, cur.Deadline = cur.Held+h.Days, deadline
	h.ExtendedBy = e.Seq
	return nil
}

// overlap returns the effective hold of a milestone whose dates meet the range
// start..end, if one does.
func (f *folder) overlap(milestone, start, end string) (Hold, bool) {
	i := slices.IndexFunc(f.holds, func(h Hold) bool {
		return h.Effective && h.Milestone == milestone && h.covers(start, end)
	})
	if i < 0 {
		return Hold{}, false
	}
	return f.holds[i], true
}

func holdIndex(holds []Hold, id string) int {
	return slices.IndexFunc(holds, func(h Hold) bool { return h.ID == id })
}

// readParties reads the optional parties object of a contract.opened body, one
// party name per role. A head that names none reads as no parties, which leaves
// every hold one approval short of effective.
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

func readDate(m map[string]any, key string) (time.Time, error) {
	s, err := readText(m, key)
	if err != nil {
		return time.Time{}, err
	}
	at, err := time.Parse(time.DateOnly, s)
	if err != nil || at.Format(time.DateOnly) != s {
		return time.Time{}, fmt.Errorf("has a %s that is not a YYYY-MM-DD date: %q", key, s)
	}
	return at, nil
}

func day(t time.Time) string { return t.Format(time.DateOnly) }

// dayStart is the instant a date begins in the zone of a calendar, which is
// what tells whether a hold starts before a deadline or after it.
func dayStart(cal bizday.Calendar, date time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(cal.Zone)
	if err != nil {
		return time.Time{}, fmt.Errorf("counts on calendar zone %q, which is not in the embedded zoneinfo database", cal.Zone)
	}
	return time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc), nil
}

// businessDays counts the days between start and end, both included, that the
// acceptance clock of cal would have run on. It reads the weekend and the
// holidays of the calendar the same way its own deadline does.
func businessDays(cal bizday.Calendar, start, end time.Time) int64 {
	days := int64(0)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if slices.Contains(cal.Weekend, d.Weekday()) || slices.Contains(cal.Holidays, day(d)) {
			continue
		}
		days++
	}
	return days
}
