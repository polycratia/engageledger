package terms

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/polycratia/engageledger/approval"
	"github.com/polycratia/engageledger/journal"
)

const stranger = "third-party-ltd"

var (
	clientKey     = keyOf(client)
	contractorKey = keyOf(contractor)
	strangerKey   = keyOf(stranger)
)

// keyOf is a real ed25519 key, seeded by the party name so a failing test
// prints the same bytes twice.
func keyOf(party string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte(party))
	return ed25519.NewKeyFromSeed(seed[:])
}

func pubHex(key ed25519.PrivateKey) string {
	return hex.EncodeToString(key.Public().(ed25519.PublicKey))
}

func bothKeys() map[string]any {
	return map[string]any{client: pubHex(clientKey), contractor: pubHex(contractorKey)}
}

// openedHead is the base SOW, worth 300000 with a cap of 200000, carrying the
// given keys object or none when keys is nil.
func openedHead(keys any) map[string]any {
	head := map[string]any{
		"parties":  map[string]any{"client": client, "contractor": contractor},
		"currency": eur,
		"milestones": []any{
			milestone("m1", "Discovery", 100000, "c1"),
			milestone("m2", "Build", 150000, "c2", "c3"),
			milestone("m3", "Handover", 50000, "c4"),
		},
		"tm_cap": int64(200000),
	}
	if keys != nil {
		head["keys"] = keys
	}
	return head
}

// ledger writes a journal step by step, keeping the body of every labelled
// proposal so an approval can sign the bytes it approves.
type ledger struct {
	t    *testing.T
	buf  bytes.Buffer
	app  *journal.Appender
	hash map[string]string
	body map[string]map[string]any
}

func newLedger(t *testing.T, head map[string]any) *ledger {
	t.Helper()
	l := &ledger{t: t, hash: map[string]string{}, body: map[string]map[string]any{}}
	l.app = journal.NewAppender(&l.buf)
	l.add("", EventOpened, head)
	return l
}

func (l *ledger) add(label, typ string, body map[string]any) {
	l.t.Helper()
	e, err := l.app.Append(typ, body)
	if err != nil {
		l.t.Fatalf("appending %s: %v", typ, err)
	}
	if label != "" {
		l.hash[label] = e.Hash
		l.body[label] = body
	}
}

func (l *ledger) propose(label string, ops ...any) {
	l.t.Helper()
	l.add(label, EventProposed, map[string]any{"currency": eur, "ops": append([]any{}, ops...)})
}

func (l *ledger) approve(label, by string, evidence map[string]any) {
	l.t.Helper()
	body := map[string]any{"by": by, "change_order": l.hash[label]}
	if evidence != nil {
		body["approval"] = evidence
	}
	l.add("", EventApproved, body)
}

func (l *ledger) sign(label, party string, key ed25519.PrivateKey) {
	l.t.Helper()
	l.approve(label, party, signObject(l.t, key, party, l.body[label]))
}

func (l *ledger) record(label, party string) {
	l.t.Helper()
	l.approve(label, party, approval.Recorded(party, []byte("e-sign export for "+party)).Object())
}

func (l *ledger) events() []journal.Event {
	l.t.Helper()
	events, err := journal.Replay(bytes.NewReader(l.buf.Bytes()))
	if err != nil {
		l.t.Fatalf("replaying the built journal: %v", err)
	}
	return events
}

func signObject(t *testing.T, key ed25519.PrivateKey, party string, body map[string]any) map[string]any {
	t.Helper()
	a, err := approval.Sign(key, party, body)
	if err != nil {
		t.Fatalf("signing for %s: %v", party, err)
	}
	return a.Object()
}

func bodyHash(t *testing.T, body map[string]any) string {
	t.Helper()
	hash, err := approval.BodyHash(body)
	if err != nil {
		t.Fatalf("BodyHash: %v", err)
	}
	return hash
}

func TestFoldReportsEvidenceStrengthAndWeakTotal(t *testing.T) {
	l := newLedger(t, openedHead(bothKeys()))
	l.propose("signed", repriceOp("m1", 120000))
	l.sign("signed", client, clientKey)
	l.sign("signed", contractor, contractorKey)
	l.propose("mixed", removeOp("m3"))
	l.sign("mixed", client, clientKey)
	l.record("mixed", contractor)
	l.propose("bare", capOp(250000))
	l.approve("bare", client, nil)
	l.approve("bare", contractor, nil)

	got, err := Fold(l.events())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got.Total != 270000 || got.Cap != 250000 {
		t.Errorf("total = %d and cap = %d, want 270000 and 250000", got.Total, got.Cap)
	}
	want := []struct {
		strength Strength
		delta    int64
	}{
		{StrengthSigned, 20000},
		{StrengthRecorded, -50000},
		{StrengthAsserted, 0},
	}
	if len(got.ChangeOrders) != len(want) {
		t.Fatalf("got %d change orders, want %d", len(got.ChangeOrders), len(want))
	}
	for i, co := range got.ChangeOrders {
		if co.State != StateEffective {
			t.Errorf("change order %d is %s, want effective", i, co.State)
		}
		if co.Strength != want[i].strength {
			t.Errorf("change order %d rests on %q evidence, want %q", i, co.Strength, want[i].strength)
		}
		if co.Delta != want[i].delta {
			t.Errorf("change order %d moved %d, want %d", i, co.Delta, want[i].delta)
		}
	}
	if got.WeakTotal != 50000 {
		t.Errorf("weak total = %d, want 50000: only the removal rests on weak evidence", got.WeakTotal)
	}
}

func TestFoldCountsEveryWeakMoveByItsSize(t *testing.T) {
	l := newLedger(t, openedHead(nil))
	l.propose("a", repriceOp("m1", 120000))
	l.record("a", client)
	l.record("a", contractor)
	l.propose("b", removeOp("m3"))
	l.record("b", contractor)
	l.record("b", client)

	got, err := Fold(l.events())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got.Total != 270000 {
		t.Errorf("total = %d, want 270000", got.Total)
	}
	if got.WeakTotal != 70000 {
		t.Errorf("weak total = %d, want 70000", got.WeakTotal)
	}
}

func TestFoldRefusesApprovalEvidence(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) []journal.Event
		seq   int64
		want  string
	}{
		{
			name: "a signature over another body",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(bothKeys()))
				l.propose("a", repriceOp("m1", 120000))
				l.propose("b", removeOp("m3"))
				obj := signObject(t, clientKey, client, l.body["b"])
				obj["evidence_hash"] = bodyHash(t, l.body["a"])
				l.approve("a", client, obj)
				return l.events()
			},
			seq: 4, want: "does not verify",
		},
		{
			name: "an approval naming a different body hash",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(bothKeys()))
				l.propose("a", repriceOp("m1", 120000))
				l.propose("b", removeOp("m3"))
				obj := signObject(t, clientKey, client, l.body["a"])
				obj["evidence_hash"] = bodyHash(t, l.body["b"])
				l.approve("a", client, obj)
				return l.events()
			},
			seq: 4, want: "names body hash",
		},
		{
			name: "a key the contract head does not list",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(map[string]any{contractor: pubHex(contractorKey)}))
				l.propose("a", repriceOp("m1", 120000))
				l.sign("a", client, clientKey)
				return l.events()
			},
			seq: 3, want: "lists no key",
		},
		{
			name: "a contract head with no keys at all",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(nil))
				l.propose("a", repriceOp("m1", 120000))
				l.sign("a", client, clientKey)
				return l.events()
			},
			seq: 3, want: "lists no key",
		},
		{
			name: "an approval by someone who is not a party",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(bothKeys()))
				l.propose("a", repriceOp("m1", 120000))
				l.sign("a", stranger, strangerKey)
				return l.events()
			},
			seq: 3, want: "neither",
		},
		{
			name: "an approval naming another party than the event",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(bothKeys()))
				l.propose("a", repriceOp("m1", 120000))
				l.approve("a", contractor, signObject(t, clientKey, client, l.body["a"]))
				return l.events()
			},
			seq: 3, want: "on an event signed by",
		},
		{
			name: "an approval naming an unknown method",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(bothKeys()))
				l.propose("a", repriceOp("m1", 120000))
				l.approve("a", client, map[string]any{
					"party": client, "method": "fax", "evidence_hash": strings.Repeat("ab", sha256.Size),
				})
				return l.events()
			},
			seq: 3, want: "unknown method",
		},
		{
			name: "an evidence hash that is not a SHA-256",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, openedHead(bothKeys()))
				l.propose("a", repriceOp("m1", 120000))
				l.approve("a", client, map[string]any{
					"party": client, "method": "recorded", "evidence_hash": "deadbeef",
				})
				return l.events()
			},
			seq: 3, want: "lowercase hex",
		},
		{
			name: "a head listing a key for a stranger",
			build: func(t *testing.T) []journal.Event {
				return newLedger(t, openedHead(map[string]any{stranger: pubHex(strangerKey)})).events()
			},
			seq: 1, want: "neither",
		},
		{
			name: "a head listing a key that is not hex",
			build: func(t *testing.T) []journal.Event {
				return newLedger(t, openedHead(map[string]any{client: "zz"})).events()
			},
			seq: 1, want: "lowercase hex",
		},
		{
			name: "a head whose keys are not an object",
			build: func(t *testing.T) []journal.Event {
				return newLedger(t, openedHead([]any{})).events()
			},
			seq: 1, want: "not an object",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Fold(c.build(t))
			if err == nil {
				t.Fatalf("Fold returned %+v, want an error", got)
			}
			if got.Total != 0 || len(got.Milestones) != 0 {
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

func TestASignedApprovalStillVerifiesAfterAReplay(t *testing.T) {
	l := newLedger(t, openedHead(bothKeys()))
	l.propose("a", repriceOp("m2", 90000))
	l.sign("a", client, clientKey)
	l.sign("a", contractor, contractorKey)

	events := l.events()
	a, err := approval.Read(events[2].Body)
	if err != nil {
		t.Fatalf("reading the approval back: %v", err)
	}
	keys, err := approval.ReadKeys(events[0].Body)
	if err != nil {
		t.Fatalf("reading the contract keys: %v", err)
	}
	if err := approval.Verify(a, events[1].Body, keys); err != nil {
		t.Fatalf("the replayed approval does not verify: %v", err)
	}
	got, err := Fold(events)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got.Total != 240000 || got.WeakTotal != 0 {
		t.Errorf("total = %d and weak total = %d, want 240000 and 0", got.Total, got.WeakTotal)
	}
}
