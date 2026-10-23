// Package terms folds the journal into the money terms of an engagement: the
// milestones of the statement of work, the total price and the
// time-and-materials cap. A number moves only through a change order that both
// the client and the contractor approved by naming the hash of the line the
// proposal was written on, so proposed, declined and withdrawn change orders
// are listed next to terms they left untouched. Fold names the first event it
// refuses: a change order naming a milestone the contract does not carry, one
// priced in another currency, one that removes or reprices an accepted
// milestone, or a second decision on a settled change order.
package terms

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/polycratia/engageledger/journal"
)

// Event types the fold prices. An event of any other type passes through.
const (
	EventOpened    = "contract.opened"
	EventProposed  = "change_order.proposed"
	EventApproved  = "change_order.approved"
	EventDeclined  = "change_order.declined"
	EventWithdrawn = "change_order.withdrawn"
	EventAccepted  = "milestone.accepted"
)

const maxAmount = 1<<53 - 1

// State is where a change order stands.
type State string

const (
	StateProposed  State = "proposed"
	StateEffective State = "effective"
	StateDeclined  State = "declined"
	StateWithdrawn State = "withdrawn"
)

// OpKind is the move one change order op makes.
type OpKind string

const (
	OpAdd     OpKind = "add"
	OpRemove  OpKind = "remove"
	OpReprice OpKind = "reprice"
	OpSetCap  OpKind = "set_cap"
)

// Parties are the two signatures the engagement needs.
type Parties struct {
	Client     string
	Contractor string
}

// Milestone is one deliverable and the amount it bills, in minor units of the
// contract currency.
type Milestone struct {
	ID       string
	Title    string
	Amount   int64
	Criteria []string
	Accepted bool
}

// Op is one move a change order makes. Milestone, Title and Criteria stay
// empty where the kind has no use for them.
type Op struct {
	Kind      OpKind
	Milestone string
	Title     string
	Amount    int64
	Criteria  []string
}

// ChangeOrder is a proposal and what became of it. Hash is the journal line
// the proposal was written on, which its approvals name.
type ChangeOrder struct {
	Hash      string
	Seq       int64
	State     State
	Ops       []Op
	Approvals []string
	DecidedBy string
}

// Terms are the current money terms of the engagement.
type Terms struct {
	Parties      Parties
	Currency     string
	Milestones   []Milestone
	Total        int64
	Cap          int64
	ChangeOrders []ChangeOrder
}

// Error reports the event Fold refused.
type Error struct {
	Seq  int64
	Type string
	Msg  string
}

func (e *Error) Error() string {
	return "terms: event " + strconv.FormatInt(e.Seq, 10) + " " + e.Type + ": " + e.Msg
}

// Fold reads the events in order and returns the terms they add up to, or an
// *Error naming the first event it refuses. An empty journal folds to empty
// terms.
func Fold(events []journal.Event) (Terms, error) {
	f := folder{byHash: map[string]*ChangeOrder{}}
	for _, e := range events {
		if err := f.apply(e); err != nil {
			return Terms{}, &Error{Seq: e.Seq, Type: e.Type, Msg: err.Error()}
		}
	}
	return f.terms(), nil
}

type folder struct {
	opened     bool
	parties    Parties
	currency   string
	milestones []Milestone
	cap        int64
	orders     []*ChangeOrder
	byHash     map[string]*ChangeOrder
}

func (f *folder) apply(e journal.Event) error {
	switch e.Type {
	case EventOpened:
		return f.open(e)
	case EventProposed, EventApproved, EventDeclined, EventWithdrawn, EventAccepted:
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
		return f.settle(e, StateDeclined)
	case EventWithdrawn:
		return f.settle(e, StateWithdrawn)
	}
	return f.accept(e)
}

func (f *folder) open(e journal.Event) error {
	if f.opened {
		return errors.New("opens a contract that is already open")
	}
	parties, err := readParties(e.Body)
	if err != nil {
		return err
	}
	currency, err := readCurrency(e.Body)
	if err != nil {
		return err
	}
	milestones, err := readSOW(e.Body)
	if err != nil {
		return err
	}
	capAmount := int64(0)
	if v, ok := e.Body["tm_cap"]; ok {
		if capAmount, err = readAmount(v, "tm_cap"); err != nil {
			return err
		}
	}
	f.opened = true
	f.parties, f.currency, f.milestones, f.cap = parties, currency, milestones, capAmount
	return nil
}

func (f *folder) propose(e journal.Event) error {
	if e.Hash == "" {
		return errors.New("carries no line hash, so no approval could name it")
	}
	if _, ok := f.byHash[e.Hash]; ok {
		return errors.New("repeats the hash of an earlier change order")
	}
	currency, err := readText(e.Body, "currency")
	if err != nil {
		return err
	}
	if currency != f.currency {
		return fmt.Errorf("prices in %s, but the contract is in %s", currency, f.currency)
	}
	ops, err := readOps(e.Body)
	if err != nil {
		return err
	}
	if _, _, err := applyOps(ops, f.milestones, f.cap); err != nil {
		return err
	}
	co := &ChangeOrder{Hash: e.Hash, Seq: e.Seq, State: StateProposed, Ops: ops}
	f.orders = append(f.orders, co)
	f.byHash[e.Hash] = co
	return nil
}

func (f *folder) approve(e journal.Event) error {
	co, by, err := f.decision(e)
	if err != nil {
		return err
	}
	if slices.Contains(co.Approvals, by) {
		return fmt.Errorf("repeats the approval of %q on change order %s", by, short(co.Hash))
	}
	co.Approvals = append(co.Approvals, by)
	if len(co.Approvals) < 2 {
		return nil
	}
	milestones, capAmount, err := applyOps(co.Ops, f.milestones, f.cap)
	if err != nil {
		return err
	}
	f.milestones, f.cap, co.State = milestones, capAmount, StateEffective
	return nil
}

func (f *folder) settle(e journal.Event, state State) error {
	co, by, err := f.decision(e)
	if err != nil {
		return err
	}
	co.State, co.DecidedBy = state, by
	return nil
}

func (f *folder) decision(e journal.Event) (*ChangeOrder, string, error) {
	hash, err := readText(e.Body, "change_order")
	if err != nil {
		return nil, "", err
	}
	co, ok := f.byHash[hash]
	if !ok {
		return nil, "", fmt.Errorf("names change order %s, which no change_order.proposed carries", short(hash))
	}
	if co.State != StateProposed {
		return nil, "", fmt.Errorf("decides change order %s again: it is already %s", short(co.Hash), co.State)
	}
	by, err := readText(e.Body, "by")
	if err != nil {
		return nil, "", err
	}
	if by != f.parties.Client && by != f.parties.Contractor {
		return nil, "", fmt.Errorf("is signed by %q, who is neither %q nor %q", by, f.parties.Client, f.parties.Contractor)
	}
	return co, by, nil
}

func (f *folder) accept(e journal.Event) error {
	id, err := readText(e.Body, "milestone")
	if err != nil {
		return err
	}
	i := indexOf(f.milestones, id)
	if i < 0 {
		return fmt.Errorf("accepts milestone %q, which the contract does not carry", id)
	}
	if f.milestones[i].Accepted {
		return fmt.Errorf("accepts milestone %q a second time", id)
	}
	f.milestones[i].Accepted = true
	return nil
}

func (f *folder) terms() Terms {
	t := Terms{
		Parties:    f.parties,
		Currency:   f.currency,
		Milestones: slices.Clone(f.milestones),
		Cap:        f.cap,
	}
	for _, m := range t.Milestones {
		t.Total += m.Amount
	}
	for _, co := range f.orders {
		t.ChangeOrders = append(t.ChangeOrders, *co)
	}
	return t
}

func applyOps(ops []Op, milestones []Milestone, capAmount int64) ([]Milestone, int64, error) {
	out := slices.Clone(milestones)
	for i, op := range ops {
		switch op.Kind {
		case OpAdd:
			if indexOf(out, op.Milestone) >= 0 {
				return nil, 0, fmt.Errorf("op %d adds milestone %q, which the contract already carries", i, op.Milestone)
			}
			out = append(out, Milestone{ID: op.Milestone, Title: op.Title, Amount: op.Amount, Criteria: slices.Clone(op.Criteria)})
		case OpRemove:
			j, err := locate(out, op.Milestone, i, "removes")
			if err != nil {
				return nil, 0, err
			}
			out = slices.Delete(out, j, j+1)
		case OpReprice:
			j, err := locate(out, op.Milestone, i, "reprices")
			if err != nil {
				return nil, 0, err
			}
			out[j].Amount = op.Amount
		case OpSetCap:
			capAmount = op.Amount
		}
	}
	return out, capAmount, nil
}

func locate(milestones []Milestone, id string, i int, verb string) (int, error) {
	j := indexOf(milestones, id)
	if j < 0 {
		return 0, fmt.Errorf("op %d %s milestone %q, which the contract does not carry", i, verb, id)
	}
	if milestones[j].Accepted {
		return 0, fmt.Errorf("op %d %s milestone %q, which is already accepted", i, verb, id)
	}
	return j, nil
}

func indexOf(milestones []Milestone, id string) int {
	return slices.IndexFunc(milestones, func(m Milestone) bool { return m.ID == id })
}

func readParties(body map[string]any) (Parties, error) {
	obj, ok := body["parties"].(map[string]any)
	if !ok {
		return Parties{}, errors.New("has no parties object")
	}
	client, err := readText(obj, "client")
	if err != nil {
		return Parties{}, fmt.Errorf("parties: %w", err)
	}
	contractor, err := readText(obj, "contractor")
	if err != nil {
		return Parties{}, fmt.Errorf("parties: %w", err)
	}
	if client == contractor {
		return Parties{}, fmt.Errorf("names %q as both client and contractor", client)
	}
	return Parties{Client: client, Contractor: contractor}, nil
}

func readCurrency(body map[string]any) (string, error) {
	currency, err := readText(body, "currency")
	if err != nil {
		return "", err
	}
	if len(currency) != 3 {
		return "", fmt.Errorf("carries currency %q, want three ISO 4217 letters", currency)
	}
	for i := 0; i < len(currency); i++ {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return "", fmt.Errorf("carries currency %q, want three ISO 4217 letters", currency)
		}
	}
	return currency, nil
}

func readSOW(body map[string]any) ([]Milestone, error) {
	items, ok := body["milestones"].([]any)
	if !ok {
		return nil, errors.New("has no milestones array")
	}
	milestones := make([]Milestone, 0, len(items))
	for i, item := range items {
		m, err := readMilestone(item)
		if err != nil {
			return nil, fmt.Errorf("milestone %d: %w", i, err)
		}
		if indexOf(milestones, m.ID) >= 0 {
			return nil, fmt.Errorf("milestone %d: repeats id %q", i, m.ID)
		}
		milestones = append(milestones, m)
	}
	return milestones, nil
}

func readMilestone(v any) (Milestone, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return Milestone{}, errors.New("is not an object")
	}
	id, err := readText(obj, "id")
	if err != nil {
		return Milestone{}, err
	}
	title, err := readText(obj, "title")
	if err != nil {
		return Milestone{}, err
	}
	amount, err := readAmount(obj["amount"], "amount")
	if err != nil {
		return Milestone{}, err
	}
	criteria, err := readCriteria(obj["criteria"])
	if err != nil {
		return Milestone{}, err
	}
	return Milestone{ID: id, Title: title, Amount: amount, Criteria: criteria}, nil
}

func readOps(body map[string]any) ([]Op, error) {
	items, ok := body["ops"].([]any)
	if !ok {
		return nil, errors.New("has no ops array")
	}
	if len(items) == 0 {
		return nil, errors.New("carries no ops")
	}
	ops := make([]Op, 0, len(items))
	for i, item := range items {
		op, err := readOp(item)
		if err != nil {
			return nil, fmt.Errorf("op %d: %w", i, err)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func readOp(v any) (Op, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return Op{}, errors.New("is not an object")
	}
	kind, err := readText(obj, "op")
	if err != nil {
		return Op{}, err
	}
	op := Op{Kind: OpKind(kind)}
	switch op.Kind {
	case OpAdd:
		if op.Title, err = readText(obj, "title"); err != nil {
			return Op{}, err
		}
		if op.Criteria, err = readCriteria(obj["criteria"]); err != nil {
			return Op{}, err
		}
	case OpRemove, OpReprice, OpSetCap:
	default:
		return Op{}, fmt.Errorf("names an unknown op %q", kind)
	}
	if op.Kind != OpSetCap {
		if op.Milestone, err = readText(obj, "milestone"); err != nil {
			return Op{}, err
		}
	}
	if op.Kind != OpRemove {
		if op.Amount, err = readAmount(obj["amount"], "amount"); err != nil {
			return Op{}, err
		}
	}
	return op, nil
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
