package terms

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/polycratia/engageledger/journal"
)

const (
	client     = "lantern-co"
	contractor = "oakline-studio"
	eur        = "EUR"
)

// ref is the index of an earlier step; it is replaced by that step's line hash
// before the step is appended.
type ref int

type step struct {
	typ  string
	body map[string]any
}

func build(t *testing.T, steps []step) []journal.Event {
	t.Helper()
	var buf bytes.Buffer
	a := journal.NewAppender(&buf)
	hashes := make([]string, 0, len(steps))
	for i, s := range steps {
		body, _ := resolve(s.body, hashes).(map[string]any)
		e, err := a.Append(s.typ, body)
		if err != nil {
			t.Fatalf("appending step %d (%s): %v", i, s.typ, err)
		}
		hashes = append(hashes, e.Hash)
	}
	events, err := journal.Replay(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("replaying the built journal: %v", err)
	}
	return events
}

func resolve(v any, hashes []string) any {
	switch t := v.(type) {
	case ref:
		return hashes[t]
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = resolve(item, hashes)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = resolve(item, hashes)
		}
		return out
	}
	return v
}

func milestone(id, title string, amount int64, criteria ...string) map[string]any {
	crit := make([]any, 0, len(criteria))
	for _, c := range criteria {
		crit = append(crit, c)
	}
	return map[string]any{"id": id, "title": title, "amount": amount, "criteria": crit}
}

func openedIn(currency string, milestones ...any) step {
	return step{EventOpened, map[string]any{
		"parties":    map[string]any{"client": client, "contractor": contractor},
		"currency":   currency,
		"milestones": append([]any{}, milestones...),
		"tm_cap":     int64(200000),
	}}
}

// base is the SOW every case starts from: three milestones worth 300000 minor
// units and a cap of 200000.
func base() []step {
	return []step{openedIn(eur,
		milestone("m1", "Discovery", 100000, "c1"),
		milestone("m2", "Build", 150000, "c2", "c3"),
		milestone("m3", "Handover", 50000, "c4"),
	)}
}

func with(steps ...step) []step {
	return append(base(), steps...)
}

func proposeIn(currency string, ops ...any) step {
	return step{EventProposed, map[string]any{"currency": currency, "ops": append([]any{}, ops...)}}
}

func propose(ops ...any) step {
	return proposeIn(eur, ops...)
}

func addOp(id, title string, amount int64, criteria ...string) map[string]any {
	op := milestone(id, title, amount, criteria...)
	delete(op, "id")
	op["op"] = string(OpAdd)
	op["milestone"] = id
	return op
}

func removeOp(id string) map[string]any {
	return map[string]any{"op": string(OpRemove), "milestone": id}
}

func repriceOp(id string, amount int64) map[string]any {
	return map[string]any{"op": string(OpReprice), "milestone": id, "amount": amount}
}

func capOp(amount int64) map[string]any {
	return map[string]any{"op": string(OpSetCap), "amount": amount}
}

func decide(typ, by string, co ref) step {
	return step{typ, map[string]any{"by": by, "change_order": co}}
}

func approve(by string, co ref) step   { return decide(EventApproved, by, co) }
func decline(by string, co ref) step   { return decide(EventDeclined, by, co) }
func withdraw(by string, co ref) step  { return decide(EventWithdrawn, by, co) }
func acceptance(id string) step {
	return step{EventAccepted, map[string]any{"milestone": id}}
}

func TestFoldOfAnEmptyJournalIsEmptyTerms(t *testing.T) {
	got, err := Fold(nil)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got.Currency != "" || got.Total != 0 || len(got.Milestones) != 0 {
		t.Errorf("Fold(nil) = %+v, want empty terms", got)
	}
}

func TestFoldReadsTheOpenedContract(t *testing.T) {
	got, err := Fold(build(t, with(acceptance("m2"))))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got.Parties != (Parties{Client: client, Contractor: contractor}) {
		t.Errorf("parties = %+v", got.Parties)
	}
	if got.Currency != eur {
		t.Errorf("currency = %q, want %q", got.Currency, eur)
	}
	m := got.Milestones[1]
	if m.ID != "m2" || m.Title != "Build" || m.Amount != 150000 {
		t.Errorf("second milestone = %+v", m)
	}
	if strings.Join(m.Criteria, ",") != "c2,c3" {
		t.Errorf("criteria = %v, want c2 and c3", m.Criteria)
	}
	if !m.Accepted {
		t.Error("milestone.accepted did not mark m2 accepted")
	}
	if got.Milestones[0].Accepted {
		t.Error("m1 is accepted without a milestone.accepted event")
	}
}

func TestFoldPrices(t *testing.T) {
	cases := []struct {
		name   string
		steps  []step
		total  int64
		cap    int64
		ids    string
		states []State
	}{
		{
			name:  "the base SOW alone",
			steps: with(),
			total: 300000, cap: 200000, ids: "m1,m2,m3",
		},
		{
			name:  "an event the fold does not price",
			steps: with(step{"invoice.issued", map[string]any{"amount": int64(100000)}}),
			total: 300000, cap: 200000, ids: "m1,m2,m3",
		},
		{
			name:   "one approval moves no number",
			steps:  with(propose(addOp("m4", "Hardening", 40000, "c5")), approve(client, 1)),
			total:  300000, cap: 200000, ids: "m1,m2,m3",
			states: []State{StateProposed},
		},
		{
			name: "both approvals add a milestone",
			steps: with(propose(addOp("m4", "Hardening", 40000, "c5")),
				approve(client, 1), approve(contractor, 1)),
			total:  340000, cap: 200000, ids: "m1,m2,m3,m4",
			states: []State{StateEffective},
		},
		{
			name: "both approvals remove a milestone",
			steps: with(propose(removeOp("m2")),
				approve(contractor, 1), approve(client, 1)),
			total:  150000, cap: 200000, ids: "m1,m3",
			states: []State{StateEffective},
		},
		{
			name: "both approvals reprice a milestone",
			steps: with(propose(repriceOp("m3", 55000)),
				approve(client, 1), approve(contractor, 1)),
			total:  305000, cap: 200000, ids: "m1,m2,m3",
			states: []State{StateEffective},
		},
		{
			name: "both approvals move the cap",
			steps: with(propose(capOp(300000)),
				approve(client, 1), approve(contractor, 1)),
			total:  300000, cap: 300000, ids: "m1,m2,m3",
			states: []State{StateEffective},
		},
		{
			name: "one change order carries several ops",
			steps: with(propose(addOp("m4", "Hardening", 40000, "c5"), repriceOp("m2", 90000), capOp(0)),
				approve(client, 1), approve(contractor, 1)),
			total:  280000, cap: 0, ids: "m1,m2,m3,m4",
			states: []State{StateEffective},
		},
		{
			name:   "a declined change order moves no number",
			steps:  with(propose(removeOp("m2")), decline(client, 1)),
			total:  300000, cap: 200000, ids: "m1,m2,m3",
			states: []State{StateDeclined},
		},
		{
			name:   "a change order withdrawn after one approval moves no number",
			steps:  with(propose(repriceOp("m1", 120000)), approve(client, 1), withdraw(client, 1)),
			total:  300000, cap: 200000, ids: "m1,m2,m3",
			states: []State{StateWithdrawn},
		},
		{
			name: "change orders fold in the order they become effective",
			steps: with(propose(repriceOp("m1", 120000)), propose(removeOp("m3")),
				approve(client, 1), approve(contractor, 1),
				approve(contractor, 2), approve(client, 2)),
			total:  270000, cap: 200000, ids: "m1,m2",
			states: []State{StateEffective, StateEffective},
		},
		{
			name: "an accepted milestone does not block an unrelated reprice",
			steps: with(acceptance("m1"), propose(repriceOp("m2", 90000)),
				approve(client, 2), approve(contractor, 2)),
			total:  240000, cap: 200000, ids: "m1,m2,m3",
			states: []State{StateEffective},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Fold(build(t, c.steps))
			if err != nil {
				t.Fatalf("Fold: %v", err)
			}
			if got.Total != c.total {
				t.Errorf("total = %d, want %d", got.Total, c.total)
			}
			if got.Cap != c.cap {
				t.Errorf("cap = %d, want %d", got.Cap, c.cap)
			}
			ids := make([]string, 0, len(got.Milestones))
			for _, m := range got.Milestones {
				ids = append(ids, m.ID)
			}
			if join := strings.Join(ids, ","); join != c.ids {
				t.Errorf("milestones = %q, want %q", join, c.ids)
			}
			if len(got.ChangeOrders) != len(c.states) {
				t.Fatalf("got %d change orders, want %d", len(got.ChangeOrders), len(c.states))
			}
			for i, co := range got.ChangeOrders {
				if co.State != c.states[i] {
					t.Errorf("change order %d is %s, want %s", i, co.State, c.states[i])
				}
			}
		})
	}
}

func TestFoldListsAChangeOrderWithItsHashAndApprovals(t *testing.T) {
	events := build(t, with(propose(removeOp("m2")), approve(client, 1), approve(contractor, 1)))
	got, err := Fold(events)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	co := got.ChangeOrders[0]
	if co.Hash != events[1].Hash {
		t.Errorf("hash = %q, want the hash of the proposal line %q", co.Hash, events[1].Hash)
	}
	if co.Seq != 2 {
		t.Errorf("seq = %d, want 2", co.Seq)
	}
	if strings.Join(co.Approvals, ",") != client+","+contractor {
		t.Errorf("approvals = %v, want both parties in order", co.Approvals)
	}
	if len(co.Ops) != 1 || co.Ops[0].Kind != OpRemove || co.Ops[0].Milestone != "m2" {
		t.Errorf("ops = %+v", co.Ops)
	}
}

func TestFoldRefuses(t *testing.T) {
	unknownHash := strings.Repeat("ab", 32)
	cases := []struct {
		name  string
		steps []step
		seq   int64
		want  string
	}{
		{"a removal naming an unknown milestone", with(propose(removeOp("m9"))), 2, "does not carry"},
		{"a reprice naming an unknown milestone", with(propose(repriceOp("m9", 10000))), 2, "does not carry"},
		{"a change order in another currency", with(proposeIn("USD", repriceOp("m1", 110000))), 2, "USD"},
		{"a removal of an accepted milestone", with(acceptance("m2"), propose(removeOp("m2"))), 3, "already accepted"},
		{"a reprice of an accepted milestone", with(acceptance("m2"), propose(repriceOp("m2", 90000))), 3, "already accepted"},
		{
			"a decision on an effective change order",
			with(propose(removeOp("m2")), approve(client, 1), approve(contractor, 1), decline(client, 1)),
			5, "already effective",
		},
		{
			"a second decline",
			with(propose(removeOp("m2")), decline(client, 1), decline(contractor, 1)),
			4, "already declined",
		},
		{
			"an approval after a withdrawal",
			with(propose(removeOp("m2")), withdraw(client, 1), approve(contractor, 1)),
			4, "already withdrawn",
		},
		{
			"the same party approving twice",
			with(propose(removeOp("m2")), approve(client, 1), approve(client, 1)),
			4, "repeats the approval",
		},
		{
			"an approval naming an unknown change order",
			with(step{EventApproved, map[string]any{"by": client, "change_order": unknownHash}}),
			2, "no change_order.proposed carries",
		},
		{
			"a decision by someone who is not a party",
			with(propose(removeOp("m2")), approve("third-party-ltd", 1)),
			3, "neither",
		},
		{
			"a change order the later journal made impossible",
			with(propose(repriceOp("m2", 90000)), propose(removeOp("m2")),
				approve(client, 2), approve(contractor, 2),
				approve(client, 1), approve(contractor, 1)),
			7, "does not carry",
		},
		{"a change order before the contract is open", []step{propose(removeOp("m1"))}, 1, "before contract.opened"},
		{"a second contract.opened", append(base(), base()[0]), 2, "already open"},
		{"an add repeating a milestone id", with(propose(addOp("m2", "Rebuild", 10000, "c9"))), 2, "already carries"},
		{"a change order with no ops", with(propose()), 2, "no ops"},
		{
			"an unknown op",
			with(propose(map[string]any{"op": "rename", "milestone": "m1"})),
			2, "unknown op",
		},
		{"an acceptance naming an unknown milestone", with(acceptance("m9")), 2, "does not carry"},
		{"a second acceptance of the same milestone", with(acceptance("m1"), acceptance("m1")), 3, "a second time"},
		{"a negative amount in the SOW", []step{openedIn(eur, milestone("m1", "Discovery", -100, "c1"))}, 1, "negative"},
		{"a currency that is not ISO 4217", []step{openedIn("euro")}, 1, "ISO 4217"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Fold(build(t, c.steps))
			if err == nil {
				t.Fatalf("Fold returned %+v, want an error", got)
			}
			if got.Currency != "" || got.Total != 0 || len(got.Milestones) != 0 {
				t.Errorf("a refused fold returned terms %+v", got)
			}
			var terr *Error
			if !errors.As(err, &terr) {
				t.Fatalf("error %v is not a *terms.Error", err)
			}
			if terr.Seq != c.seq {
				t.Errorf("seq = %d, want %d (%v)", terr.Seq, c.seq, err)
			}
			if !strings.Contains(terr.Msg, c.want) {
				t.Errorf("message %q should mention %q", terr.Msg, c.want)
			}
		})
	}
}

func TestErrorNamesTheRefusedEvent(t *testing.T) {
	_, err := Fold(build(t, with(propose(removeOp("m9")))))
	if err == nil {
		t.Fatal("want an error")
	}
	if want := "terms: event 2 change_order.proposed: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q should start with %q", err.Error(), want)
	}
}
