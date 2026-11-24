package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/polycratia/engageledger/acceptance"
	"github.com/polycratia/engageledger/canon"
	"github.com/polycratia/engageledger/journal"
	"github.com/polycratia/engageledger/terms"
)

const usage = `usage: engageledger <verify|fold|clock|seed> <journal.jsonl>

verify  walk the chain, the canonical form of every line, the approval
        evidence and the recorded deadlines, naming the first line refused
fold    print the milestones, the price, the cap and the evidence strength
clock   print the acceptance windows whose clock is running
seed    write the example journal the demo runs on
`

// failure is a journal the commands refuse, carrying the line that answers
// for it where one line does.
type failure struct {
	Line int64
	Msg  string
}

func (f *failure) Error() string {
	if f.Line > 0 {
		return "line " + strconv.FormatInt(f.Line, 10) + ": " + f.Msg
	}
	return f.Msg
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	out, err := do(args[0], args[1])
	if err != nil {
		fmt.Fprintln(stderr, "engageledger: "+err.Error())
		var refused *failure
		if errors.As(err, &refused) {
			return 1
		}
		return 2
	}
	if len(out) > 0 {
		stdout.Write(append(out, '\n'))
	}
	return 0
}

func do(cmd, path string) ([]byte, error) {
	switch cmd {
	case "seed":
		return nil, seed(path)
	case "verify", "fold", "clock":
	default:
		return nil, fmt.Errorf("unknown command %q", cmd)
	}
	events, err := read(path)
	if err != nil {
		return nil, err
	}
	switch cmd {
	case "verify":
		return verify(events)
	case "fold":
		return fold(events)
	}
	return clock(events)
}

func read(path string) ([]journal.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	events, err := journal.Replay(f)
	if err != nil {
		var jerr *journal.Error
		if errors.As(err, &jerr) {
			return nil, &failure{Line: int64(jerr.Line), Msg: jerr.Msg}
		}
		return nil, err
	}
	return events, nil
}

// verify collects what the folds refuse and returns the refusal standing on
// the lowest line, so a journal broken in several places still names the
// first line a reader has to look at.
func verify(events []journal.Event) ([]byte, error) {
	var refusals []*failure
	if _, err := price(events); err != nil {
		var refused *failure
		if !errors.As(err, &refused) {
			return nil, err
		}
		refusals = append(refusals, refused)
	}
	a := acceptance.Fold(events)
	for _, r := range a.Refused {
		refusals = append(refusals, &failure{Line: r.Seq, Msg: r.Type + ": " + r.Msg})
	}
	drifts, err := a.Verify()
	if err != nil {
		return nil, &failure{Msg: err.Error()}
	}
	for _, d := range drifts {
		refusals = append(refusals, &failure{Line: d.Seq, Msg: d.String()})
	}
	if len(refusals) > 0 {
		slices.SortStableFunc(refusals, func(x, y *failure) int { return cmp.Compare(x.Line, y.Line) })
		return nil, refusals[0]
	}
	return canon.Marshal(map[string]any{"lines": int64(len(events)), "ok": true})
}

func fold(events []journal.Event) ([]byte, error) {
	t, err := price(events)
	if err != nil {
		return nil, err
	}
	milestones := make([]any, 0, len(t.Milestones))
	for _, m := range t.Milestones {
		milestones = append(milestones, map[string]any{
			"id":       m.ID,
			"title":    m.Title,
			"amount":   m.Amount,
			"criteria": list(m.Criteria),
			"accepted": m.Accepted,
		})
	}
	orders := make([]any, 0, len(t.ChangeOrders))
	for _, co := range t.ChangeOrders {
		obj := map[string]any{"seq": co.Seq, "hash": co.Hash, "state": string(co.State)}
		if len(co.Approvals) > 0 {
			obj["approvals"] = list(co.Approvals)
		}
		if co.State == terms.StateEffective {
			obj["evidence"] = string(co.Strength)
			obj["delta"] = co.Delta
		}
		orders = append(orders, obj)
	}
	return canon.Marshal(map[string]any{
		"parties":       map[string]any{"client": t.Parties.Client, "contractor": t.Parties.Contractor},
		"currency":      t.Currency,
		"milestones":    milestones,
		"total":         t.Total,
		"weak_total":    t.WeakTotal,
		"cap":           t.Cap,
		"change_orders": orders,
	})
}

func clock(events []journal.Event) ([]byte, error) {
	a := acceptance.Fold(events)
	windows := make([]any, 0, len(a.Milestones))
	for _, m := range a.Milestones {
		w, open := m.Open()
		if !open {
			continue
		}
		obj := map[string]any{
			"milestone": m.ID,
			"seq":       w.Seq,
			"days":      w.Days,
			"held":      w.Held,
			"retest":    w.Retest,
		}
		if !w.Deadline.IsZero() {
			zone, local, err := localize(a, m.ID, w)
			if err != nil {
				return nil, err
			}
			obj["delivered_utc"] = w.Delivered.Format(time.RFC3339)
			obj["deadline_utc"] = w.Deadline.Format(time.RFC3339)
			obj["deadline_local"] = local
			obj["zone"] = zone
		}
		windows = append(windows, obj)
	}
	return canon.Marshal(map[string]any{"open_windows": windows})
}

// localize reads the recorded deadline of a window in the zone of the
// calendar version its delivery named, which is the zone the client counts
// the window in.
func localize(a acceptance.Acceptance, id string, w acceptance.Window) (string, string, error) {
	i := slices.IndexFunc(a.Calendars, func(v acceptance.Version) bool { return v.Hash == w.CalendarHash })
	if i < 0 {
		return "", "", &failure{
			Line: w.Seq,
			Msg:  "delivers " + id + " on calendar " + w.CalendarHash + ", which the journal does not carry",
		}
	}
	zone := a.Calendars[i].Calendar.Zone
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return "", "", err
	}
	return zone, w.Deadline.In(loc).Format(time.RFC3339), nil
}

func price(events []journal.Event) (terms.Terms, error) {
	t, err := terms.Fold(events)
	if err == nil {
		return t, nil
	}
	var terr *terms.Error
	if errors.As(err, &terr) {
		return terms.Terms{}, &failure{Line: terr.Seq, Msg: terr.Type + ": " + terr.Msg}
	}
	return terms.Terms{}, err
}

func list(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
