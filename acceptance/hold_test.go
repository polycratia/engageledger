package acceptance

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/polycratia/engageledger/approval"
)

const (
	client     = "lantern-co"
	contractor = "oakline-studio"
	holdStart  = "2026-03-04"
	holdEnd    = "2026-03-06"
)

func propose(id, milestone, start, end string) step {
	return step{EventHeld, HoldBody(id, milestone, start, end)}
}

// approveBy is one party's approval of a proposal, evidenced outside the
// journal the way an email sign-off is.
func approveBy(id, party string) step {
	return step{EventHoldApproved, ApprovalBody(id, approval.Recorded(party, []byte(party+" approves "+id)))}
}

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 key: %v", err)
	}
	return pub, priv
}

func signBy(t *testing.T, key ed25519.PrivateKey, party, id string, body map[string]any) step {
	t.Helper()
	a, err := approval.Sign(key, party, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return step{EventHoldApproved, ApprovalBody(id, a)}
}

func holdOf(t *testing.T, a Acceptance, id string) Hold {
	t.Helper()
	h, ok := a.Hold(id)
	if !ok {
		t.Fatalf("the fold carries no hold %q", id)
	}
	return h
}

func TestAHoldBothPartiesApprovedIsEffective(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver(t, "m2", deliveredAt, 10),
		propose("h1", "m2", holdStart, holdEnd),
		approveBy("h1", client),
		approveBy("h1", contractor))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	if len(got.Holds) != 1 {
		t.Fatalf("got %d holds, want the one the journal proposed", len(got.Holds))
	}
	h := holdOf(t, got, "h1")
	if h.Seq != 3 || h.Milestone != "m2" || h.Start != holdStart || h.End != holdEnd {
		t.Errorf("hold = %+v, want the pause proposed at seq 3 over %s..%s", h, holdStart, holdEnd)
	}
	if h.Days != 3 {
		t.Errorf("the hold covers %d business days, want the three it spans", h.Days)
	}
	if !h.Effective || h.EffectiveBy != 5 {
		t.Errorf("hold = %+v, want it effective on the approval at seq 5", h)
	}
	if len(h.Approvals) != 2 || !h.Approved(client) || !h.Approved(contractor) {
		t.Errorf("approvals = %+v, want one from each party", h.Approvals)
	}
	if h.Approved("northwind-gmbh") {
		t.Error("the hold reads as approved by someone who never approved it")
	}
	if h.ExtendedBy != 0 {
		t.Errorf("the hold names event %d as its extension, want none yet", h.ExtendedBy)
	}
	m := milestoneOf(t, got, "m2")
	if m.Status != StatusDelivered {
		t.Errorf("status = %q, want the milestone to stay delivered under a hold", m.Status)
	}
	w, open := m.Open()
	if !open {
		t.Fatal("the hold closed the acceptance window")
	}
	if w.Held != 0 || w.Deadline.Format(time.RFC3339) != wantDeadline {
		t.Errorf("window = %+v, want the deadline held at %s until an extension records another one", w, wantDeadline)
	}
}

func TestAOneSidedHoldIsListedAndPausesNothing(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver(t, "m2", deliveredAt, 10),
		propose("h1", "m2", holdStart, holdEnd),
		approveBy("h1", client))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	h := holdOf(t, got, "h1")
	if h.Effective || h.EffectiveBy != 0 {
		t.Errorf("hold = %+v, want a hold one party approved to be effective for nothing", h)
	}
	if len(h.Approvals) != 1 || !h.Approved(client) || h.Approved(contractor) {
		t.Errorf("approvals = %+v, want the one the client gave", h.Approvals)
	}
	if h.Days != 3 {
		t.Errorf("the listed hold covers %d business days, want the three it spans", h.Days)
	}
	if _, ok := got.Hold("h9"); ok {
		t.Error("the fold carries a hold no event proposed")
	}
	w, open := milestoneOf(t, got, "m2").Open()
	if !open {
		t.Fatal("the one-sided hold closed the acceptance window")
	}
	if w.Held != 0 || w.Deadline.Format(time.RFC3339) != wantDeadline {
		t.Errorf("window = %+v, want the clock running to %s", w, wantDeadline)
	}
	if err := got.Deem("m2", w.Deadline.Add(time.Second)); err != nil {
		t.Errorf("Deem after the recorded deadline: %v, want a one-sided hold to stop no clock", err)
	}
}

func TestHoldsOnOtherDatesAndOtherMilestonesAreEffectiveToo(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver(t, "m2", deliveredAt, 10),
		deliver(t, "m1", deliveredAt, 10),
		propose("h1", "m2", holdStart, holdEnd),
		approveBy("h1", client), approveBy("h1", contractor),
		propose("h2", "m2", "2026-03-09", "2026-03-10"),
		approveBy("h2", client), approveBy("h2", contractor),
		propose("h3", "m1", holdStart, holdEnd),
		approveBy("h3", client), approveBy("h3", contractor))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	if len(got.Holds) != 3 {
		t.Fatalf("got %d holds, want the three the journal proposed", len(got.Holds))
	}
	for id, by := range map[string]int64{"h1": 6, "h2": 9, "h3": 12} {
		h := holdOf(t, got, id)
		if !h.Effective || h.EffectiveBy != by {
			t.Errorf("hold %q = %+v, want it effective on the approval at seq %d", id, h, by)
		}
	}
	if days := holdOf(t, got, "h2").Days; days != 2 {
		t.Errorf("the second hold covers %d business days, want 2", days)
	}
}

func TestASignedHoldApprovalVerifiesAgainstTheProposal(t *testing.T) {
	clientPub, clientPriv := keypair(t)
	contractorPub, contractorPriv := keypair(t)
	h := head(config(10, RetestRestart))
	h["keys"] = map[string]any{
		client:     hex.EncodeToString(clientPub),
		contractor: hex.EncodeToString(contractorPub),
	}
	body := HoldBody("h1", "m2", holdStart, holdEnd)

	got := fold(t, h,
		deliver(t, "m2", deliveredAt, 10),
		step{EventHeld, body},
		signBy(t, clientPriv, client, "h1", body),
		signBy(t, contractorPriv, contractor, "h1", body))
	if len(got.Refused) != 0 {
		t.Fatalf("the fold refused %v", got.Refused)
	}
	signed := holdOf(t, got, "h1")
	if !signed.Effective || signed.EffectiveBy != 5 {
		t.Errorf("hold = %+v, want it effective on the signature at seq 5", signed)
	}
	for i, a := range signed.Approvals {
		if !a.Strong() {
			t.Errorf("approval %d = %+v, want the journal itself to check it", i, a)
		}
	}

	other := fold(t, h,
		deliver(t, "m2", deliveredAt, 10),
		step{EventHeld, body},
		signBy(t, clientPriv, client, "h1", HoldBody("h1", "m2", holdStart, "2026-03-10")))
	if len(other.Refused) != 1 {
		t.Fatalf("the fold refused %v, want the approval of another proposal", other.Refused)
	}
	r := other.Refused[0]
	if r.Seq != 4 || r.Type != EventHoldApproved {
		t.Errorf("refusal = %+v, want the approval at seq 4", r)
	}
	if !strings.Contains(r.Msg, "names body hash") {
		t.Errorf("message %q should say the approval names another body hash", r.Msg)
	}
	if left := holdOf(t, other, "h1"); left.Effective || len(left.Approvals) != 0 {
		t.Errorf("hold = %+v, want a refused approval to leave it as proposed", left)
	}
}

func TestAnApprovalThatWouldOverlapAnEffectiveHoldIsRefused(t *testing.T) {
	got := fold(t, head(config(10, RetestRestart)),
		deliver(t, "m2", deliveredAt, 10),
		propose("h1", "m2", holdStart, holdEnd),
		propose("h2", "m2", "2026-03-05", "2026-03-09"),
		approveBy("h1", client), approveBy("h1", contractor),
		approveBy("h2", client), approveBy("h2", contractor))
	if len(got.Refused) != 1 {
		t.Fatalf("the fold refused %v, want the approval that would overlap", got.Refused)
	}
	r := got.Refused[0]
	if r.Seq != 8 || r.Type != EventHoldApproved {
		t.Errorf("refusal = %+v, want the approval at seq 8", r)
	}
	if !strings.Contains(r.Msg, "overlaps hold") {
		t.Errorf("message %q should name the effective hold it overlaps", r.Msg)
	}
	first := holdOf(t, got, "h1")
	if !first.Effective || first.EffectiveBy != 6 {
		t.Errorf("hold = %+v, want the first one effective at seq 6", first)
	}
	second := holdOf(t, got, "h2")
	if second.Effective || len(second.Approvals) != 1 || !second.Approved(client) {
		t.Errorf("hold = %+v, want it listed with the one approval it keeps", second)
	}
}

func TestAHoldTheFoldWillNotApply(t *testing.T) {
	ten := func() map[string]any { return head(config(10, RetestRestart)) }
	delivery := func() step { return deliver(t, "m2", deliveredAt, 10) }
	cases := []struct {
		name  string
		h     map[string]any
		steps []step
		seq   int64
		typ   string
		want  string
	}{
		{
			"a hold overlapping an effective hold of the same milestone", ten(),
			[]step{delivery(), propose("h1", "m2", holdStart, holdEnd),
				approveBy("h1", client), approveBy("h1", contractor),
				propose("h2", "m2", holdEnd, "2026-03-09")},
			6, EventHeld, "overlaps hold",
		},
		{
			"a hold starting after the acceptance window ran out", ten(),
			[]step{delivery(), propose("h1", "m2", "2026-03-17", "2026-03-18")},
			3, EventHeld, "after the acceptance window",
		},
		{
			"a hold on a milestone whose window is not open", ten(),
			[]step{propose("h1", "m2", holdStart, holdEnd)},
			2, EventHeld, "window is not open",
		},
		{
			"a hold on a milestone the contract does not carry", ten(),
			[]step{propose("h1", "m9", holdStart, holdEnd)},
			2, EventHeld, "does not carry",
		},
		{
			"a hold the journal already proposed", ten(),
			[]step{delivery(), propose("h1", "m2", holdStart, holdEnd), propose("h1", "m2", "2026-03-09", "2026-03-10")},
			4, EventHeld, "again",
		},
		{
			"a hold ending before it starts", ten(),
			[]step{delivery(), propose("h1", "m2", holdEnd, holdStart)},
			3, EventHeld, "before it starts",
		},
		{
			"a hold naming no dates", ten(),
			[]step{delivery(), {EventHeld, map[string]any{"hold": "h1", "milestone": "m2"}}},
			3, EventHeld, "no start string",
		},
		{
			"a hold naming no id", ten(),
			[]step{delivery(), {EventHeld, map[string]any{"milestone": "m2", "start": holdStart, "end": holdEnd}}},
			3, EventHeld, "no hold string",
		},
		{
			"a hold whose start is not a date", ten(),
			[]step{delivery(), {EventHeld, map[string]any{"hold": "h1", "milestone": "m2", "start": "2026-3-4", "end": holdEnd}}},
			3, EventHeld, "YYYY-MM-DD",
		},
		{
			"a hold on a window whose delivery fixed no deadline", headOn(nil, nil),
			[]step{bareDelivery("m2"), propose("h1", "m2", holdStart, holdEnd)},
			3, EventHeld, "fixed no deadline",
		},
		{
			"an approval of a hold no event proposed", ten(),
			[]step{delivery(), approveBy("h1", client)},
			3, EventHoldApproved, "no hold.proposed",
		},
		{
			"an approval carrying no approval object", ten(),
			[]step{delivery(), propose("h1", "m2", holdStart, holdEnd), {EventHoldApproved, map[string]any{"hold": "h1"}}},
			4, EventHoldApproved, "no approval object",
		},
		{
			"an approval naming no hold", ten(),
			[]step{delivery(), propose("h1", "m2", holdStart, holdEnd),
				{EventHoldApproved, map[string]any{"approval": approval.Recorded(client, []byte("sign-off")).Object()}}},
			4, EventHoldApproved, "no hold string",
		},
		{
			"an approval by someone who is not a party", ten(),
			[]step{delivery(), propose("h1", "m2", holdStart, holdEnd), approveBy("h1", "northwind-gmbh")},
			4, EventHoldApproved, "not a party of the contract",
		},
		{
			"the same party approving twice", ten(),
			[]step{delivery(), propose("h1", "m2", holdStart, holdEnd), approveBy("h1", client), approveBy("h1", client)},
			5, EventHoldApproved, "a second time",
		},
		{
			"an approval after every party approved", ten(),
			[]step{delivery(), propose("h1", "m2", holdStart, holdEnd),
				approveBy("h1", client), approveBy("h1", contractor), approveBy("h1", client)},
			6, EventHoldApproved, "already approved",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fold(t, c.h, c.steps...)
			if len(got.Refused) != 1 {
				t.Fatalf("the fold refused %v, want exactly one event", got.Refused)
			}
			r := got.Refused[0]
			if r.Seq != c.seq || r.Type != c.typ {
				t.Errorf("refusal = %+v, want the %s at seq %d", r, c.typ, c.seq)
			}
			if !strings.Contains(r.Msg, c.want) {
				t.Errorf("message %q should mention %q", r.Msg, c.want)
			}
			for _, m := range got.Milestones {
				for _, w := range m.Windows {
					if w.Held != 0 {
						t.Errorf("window %+v of milestone %q pauses %d days, want nothing paused", w, m.ID, w.Held)
					}
				}
			}
		})
	}
}
