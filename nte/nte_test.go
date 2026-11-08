package nte

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

const (
	client     = "lantern-co"
	contractor = "oakline-studio"
	stranger   = "third-party-ltd"
	eur        = "EUR"
	hour       = int64(60)
)

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

// head is the contract head every case opens on: two parties, their keys and a
// cap of the given minutes.
func head(minutes int64) map[string]any {
	return map[string]any{
		"parties":  map[string]any{"client": client, "contractor": contractor},
		"currency": eur,
		"keys":     map[string]any{client: pubHex(clientKey), contractor: pubHex(contractorKey)},
		"milestones": []any{map[string]any{
			"id": "m1", "title": "Discovery", "amount": int64(100000), "criteria": []any{"c1"},
		}},
		"nte_cap_minutes": minutes,
	}
}

// ledger writes a journal step by step, keeping the body of every labelled
// proposal so an approval can sign the bytes it approves.
type ledger struct {
	t        *testing.T
	buf      bytes.Buffer
	app      *journal.Appender
	contract string
	hash     map[string]string
	body     map[string]map[string]any
}

func emptyLedger(t *testing.T) *ledger {
	t.Helper()
	l := &ledger{t: t, hash: map[string]string{}, body: map[string]map[string]any{}}
	l.app = journal.NewAppender(&l.buf)
	return l
}

func openedLedger(t *testing.T, h map[string]any) *ledger {
	t.Helper()
	l := emptyLedger(t)
	l.contract = l.add("", EventOpened, h).Hash
	return l
}

func newLedger(t *testing.T, minutes int64) *ledger {
	t.Helper()
	return openedLedger(t, head(minutes))
}

func (l *ledger) add(label, typ string, body map[string]any) journal.Event {
	l.t.Helper()
	e, err := l.app.Append(typ, body)
	if err != nil {
		l.t.Fatalf("appending %s: %v", typ, err)
	}
	if label != "" {
		l.hash[label], l.body[label] = e.Hash, body
	}
	return e
}

func (l *ledger) log(date string, minutes int64) {
	l.t.Helper()
	l.add("", EventLogged, map[string]any{"date": date, "minutes": minutes})
}

func (l *ledger) raise(label string, minutes int64) {
	l.t.Helper()
	l.add(label, EventProposed, map[string]any{
		"currency": eur,
		"ops":      []any{map[string]any{"op": OpSetCap, "minutes": minutes}},
	})
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

func (l *ledger) decline(label, by string) {
	l.t.Helper()
	l.add("", EventDeclined, map[string]any{"by": by, "change_order": l.hash[label]})
}

func (l *ledger) notice(contract string, version, threshold int64) {
	l.t.Helper()
	key, err := NoticeKey(contract, version, threshold)
	if err != nil {
		l.t.Fatalf("NoticeKey: %v", err)
	}
	l.add("", EventNotice, map[string]any{
		"contract": contract, "cap_version": version, "threshold": threshold, "key": key,
	})
}

func (l *ledger) events() []journal.Event {
	l.t.Helper()
	events, err := journal.Replay(bytes.NewReader(l.buf.Bytes()))
	if err != nil {
		l.t.Fatalf("replaying the built journal: %v", err)
	}
	return events
}

func (l *ledger) fold() NTE {
	l.t.Helper()
	got, err := Fold(l.events())
	if err != nil {
		l.t.Fatalf("Fold: %v", err)
	}
	return got
}

func signObject(t *testing.T, key ed25519.PrivateKey, party string, body map[string]any) map[string]any {
	t.Helper()
	a, err := approval.Sign(key, party, body)
	if err != nil {
		t.Fatalf("signing for %s: %v", party, err)
	}
	return a.Object()
}

func TestTheEntryThatCrossesTheCapIsRefusedInFull(t *testing.T) {
	l := newLedger(t, 40*hour)
	for _, date := range []string{"2026-04-06", "2026-04-07", "2026-04-08", "2026-04-09", "2026-04-10"} {
		l.log(date, 8*hour)
	}
	got := l.fold()
	if got.Logged != 40*hour || got.Remaining() != 0 {
		t.Fatalf("logged %d minutes with %d remaining, want the 2400-minute cap filled", got.Logged, got.Remaining())
	}
	if len(got.Entries) != 5 || got.Cap.Version != 1 {
		t.Errorf("fold = %+v, want five entries under cap version 1", got)
	}

	before := l.buf.Len()
	if _, err := got.Log(l.app, "2026-04-13", hour); err == nil {
		t.Fatal("Log appended the hour that crosses the cap")
	} else if !strings.Contains(err.Error(), "0 minutes remain") {
		t.Errorf("error %q should name the minutes that remain", err)
	}
	if l.buf.Len() != before {
		t.Errorf("a refused entry wrote %d bytes to the journal", l.buf.Len()-before)
	}

	l.log("2026-04-13", hour)
	_, err := Fold(l.events())
	if err == nil {
		t.Fatal("Fold accepted the hour that crosses the cap")
	}
	var nerr *Error
	if !errors.As(err, &nerr) {
		t.Fatalf("error %v is not a *nte.Error", err)
	}
	if nerr.Seq != 7 {
		t.Errorf("seq = %d, want 7 (%v)", nerr.Seq, err)
	}
	if !strings.Contains(nerr.Msg, "0 minutes remain") {
		t.Errorf("message %q should name the minutes that remain", nerr.Msg)
	}
}

func TestAChangeOrderRaisingTheCapAcceptsTheEntry(t *testing.T) {
	l := newLedger(t, 40*hour)
	l.log("2026-04-06", 40*hour)
	l.raise("up", 60*hour)
	l.sign("up", client, clientKey)
	l.sign("up", contractor, contractorKey)

	got := l.fold()
	if got.Cap.Minutes != 60*hour || got.Cap.Version != 2 {
		t.Fatalf("cap = %+v, want 3600 minutes at version 2", got.Cap)
	}
	if got.Remaining() != 20*hour {
		t.Errorf("remaining = %d, want 1200", got.Remaining())
	}
	e, err := got.Log(l.app, "2026-04-13", hour)
	if err != nil {
		t.Fatalf("Log of hour 41 under the raised cap: %v", err)
	}
	if e.Seq != 6 {
		t.Errorf("entry seq = %d, want 6", e.Seq)
	}
	got = l.fold()
	if got.Logged != 41*hour || len(got.Entries) != 2 {
		t.Errorf("logged %d minutes in %d entries, want 2460 in two", got.Logged, len(got.Entries))
	}
}

func TestTheNoticeFiresOnceAcrossTwoReplays(t *testing.T) {
	if got := Thresholds(); len(got) != 2 || got[0] != 75 || got[1] != 90 {
		t.Fatalf("thresholds = %v, want 75 and 90", got)
	}
	l := newLedger(t, 40*hour)
	l.log("2026-04-06", 30*hour)

	got := l.fold()
	if pending := got.Pending(); len(pending) != 1 || pending[0] != 75 {
		t.Fatalf("pending = %v, want the 75%% threshold alone", pending)
	}
	fired, err := got.Notify(l.app)
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(fired) != 1 || fired[0].Type != EventNotice {
		t.Fatalf("Notify appended %+v, want one nte.notice", fired)
	}

	again := l.fold()
	if len(again.Notices) != 1 {
		t.Fatalf("got %d notices, want one", len(again.Notices))
	}
	key, err := NoticeKey(l.contract, 1, 75)
	if err != nil {
		t.Fatalf("NoticeKey: %v", err)
	}
	if n := again.Notices[0]; n.Threshold != 75 || n.CapVersion != 1 || n.Key != key || n.Logged != 30*hour {
		t.Errorf("notice = %+v, want the 75%% notice of cap version 1 keyed %s", n, key)
	}
	if pending := again.Pending(); len(pending) != 0 {
		t.Errorf("the second replay left %v pending", pending)
	}

	before := l.buf.Len()
	fired, err = again.Notify(l.app)
	if err != nil {
		t.Fatalf("Notify on the second replay: %v", err)
	}
	if len(fired) != 0 {
		t.Errorf("the second run appended %d notices", len(fired))
	}
	if l.buf.Len() != before {
		t.Errorf("the second run wrote %d bytes to the journal", l.buf.Len()-before)
	}
	if third := l.fold(); len(third.Notices) != 1 {
		t.Errorf("got %d notices after a third replay, want one", len(third.Notices))
	}
}

func TestRaisingTheCapReArmsBothThresholds(t *testing.T) {
	l := newLedger(t, 40*hour)
	l.log("2026-04-06", 36*hour)

	got := l.fold()
	if pending := got.Pending(); len(pending) != 2 {
		t.Fatalf("pending = %v, want both thresholds at 90%% of the cap", pending)
	}
	if _, err := got.Notify(l.app); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	l.raise("up", 60*hour)
	l.sign("up", client, clientKey)
	l.sign("up", contractor, contractorKey)
	if pending := l.fold().Pending(); len(pending) != 0 {
		t.Fatalf("36 hours of a 60-hour cap left %v pending", pending)
	}

	l.log("2026-04-13", 9*hour)
	got = l.fold()
	if pending := got.Pending(); len(pending) != 1 || pending[0] != 75 {
		t.Fatalf("pending = %v, want the 75%% threshold under the raised cap", pending)
	}
	if fired, err := got.Notify(l.app); err != nil || len(fired) != 1 {
		t.Fatalf("Notify appended %d notices (%v), want one", len(fired), err)
	}
	final := l.fold()
	if len(final.Notices) != 3 {
		t.Fatalf("got %d notices, want three", len(final.Notices))
	}
	first, last := final.Notices[0], final.Notices[2]
	if last.Threshold != 75 || last.CapVersion != 2 {
		t.Errorf("the re-armed notice = %+v, want 75%% of cap version 2", last)
	}
	if last.Key == first.Key {
		t.Error("the re-armed notice carries the key of the notice under the old cap")
	}
}

func TestFoldRefuses(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) []journal.Event
		seq   int64
		want  string
	}{
		{
			name: "an entry on a head that names no cap",
			build: func(t *testing.T) []journal.Event {
				h := head(0)
				delete(h, "nte_cap_minutes")
				l := openedLedger(t, h)
				l.log("2026-04-06", hour)
				return l.events()
			},
			seq: 2, want: "0 minutes remain",
		},
		{
			name: "an entry before the contract is open",
			build: func(t *testing.T) []journal.Event {
				l := emptyLedger(t)
				l.log("2026-04-06", hour)
				return l.events()
			},
			seq: 1, want: "before contract.opened",
		},
		{
			name: "an entry on a loose date",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-4-6", hour)
				return l.events()
			},
			seq: 2, want: "YYYY-MM-DD",
		},
		{
			name: "an entry of no minutes",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", int64(0))
				return l.events()
			},
			seq: 2, want: "logs no minutes",
		},
		{
			name: "a change order that lowers the cap",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.raise("down", 20*hour)
				l.sign("down", client, clientKey)
				l.sign("down", contractor, contractorKey)
				return l.events()
			},
			seq: 4, want: "at or below",
		},
		{
			name: "a change order carrying two cap ops",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.add("", EventProposed, map[string]any{"currency": eur, "ops": []any{
					map[string]any{"op": OpSetCap, "minutes": 50 * hour},
					map[string]any{"op": OpSetCap, "minutes": 60 * hour},
				}})
				return l.events()
			},
			seq: 2, want: "more than one",
		},
		{
			name: "an approval by someone who is not a party",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.raise("up", 60*hour)
				l.sign("up", stranger, strangerKey)
				return l.events()
			},
			seq: 3, want: "not a party",
		},
		{
			name: "the same party approving twice",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.raise("up", 60*hour)
				l.sign("up", client, clientKey)
				l.sign("up", client, clientKey)
				return l.events()
			},
			seq: 4, want: "repeats the approval",
		},
		{
			name: "an approval after a decline",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.raise("up", 60*hour)
				l.decline("up", client)
				l.sign("up", contractor, contractorKey)
				return l.events()
			},
			seq: 4, want: "already declined",
		},
		{
			name: "an approval signing another body",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.raise("up", 60*hour)
				l.approve("up", client, signObject(t, clientKey, client, map[string]any{
					"currency": eur, "ops": []any{},
				}))
				return l.events()
			},
			seq: 3, want: "names body hash",
		},
		{
			name: "a notice repeated in the journal",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", 30*hour)
				l.notice(l.contract, 1, 75)
				l.notice(l.contract, 1, 75)
				return l.events()
			},
			seq: 4, want: "repeats the notice",
		},
		{
			name: "a notice naming a cap version the journal left behind",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", 30*hour)
				l.notice(l.contract, 7, 75)
				return l.events()
			},
			seq: 3, want: "stands at version 1",
		},
		{
			name: "a notice carrying the key of another threshold",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", 30*hour)
				key, err := NoticeKey(l.contract, 1, 90)
				if err != nil {
					t.Fatalf("NoticeKey: %v", err)
				}
				l.add("", EventNotice, map[string]any{
					"contract": l.contract, "cap_version": int64(1), "threshold": int64(75), "key": key,
				})
				return l.events()
			},
			seq: 3, want: "idempotency key",
		},
		{
			name: "a notice at a threshold the minutes have not reached",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", 10*hour)
				l.notice(l.contract, 1, 75)
				return l.events()
			},
			seq: 3, want: "of 2400 minutes logged",
		},
		{
			name: "a notice at a threshold the policy does not carry",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", 30*hour)
				l.notice(l.contract, 1, 50)
				return l.events()
			},
			seq: 3, want: "not a notice threshold",
		},
		{
			name: "a notice naming another contract",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", 30*hour)
				l.notice(strings.Repeat("ab", sha256.Size), 1, 75)
				return l.events()
			},
			seq: 3, want: "names contract",
		},
		{
			name: "a notice carrying a key beyond the four",
			build: func(t *testing.T) []journal.Event {
				l := newLedger(t, 40*hour)
				l.log("2026-04-06", 30*hour)
				key, err := NoticeKey(l.contract, 1, 75)
				if err != nil {
					t.Fatalf("NoticeKey: %v", err)
				}
				l.add("", EventNotice, map[string]any{
					"contract": l.contract, "cap_version": int64(1), "threshold": int64(75),
					"key": key, "note": "x",
				})
				return l.events()
			},
			seq: 3, want: "want contract, cap_version",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Fold(c.build(t))
			if err == nil {
				t.Fatalf("Fold returned %+v, want an error", got)
			}
			if got.Logged != 0 || len(got.Entries) != 0 {
				t.Errorf("a refused fold returned %+v", got)
			}
			var nerr *Error
			if !errors.As(err, &nerr) {
				t.Fatalf("error %v is not a *nte.Error", err)
			}
			if nerr.Seq != c.seq {
				t.Errorf("seq = %d, want %d (%v)", nerr.Seq, c.seq, err)
			}
			if !strings.Contains(nerr.Msg, c.want) {
				t.Errorf("message %q should mention %q", nerr.Msg, c.want)
			}
		})
	}
}

func TestErrorNamesTheRefusedEvent(t *testing.T) {
	l := newLedger(t, 40*hour)
	l.log("2026-04-06", 41*hour)
	_, err := Fold(l.events())
	if err == nil {
		t.Fatal("want an error")
	}
	if want := "nte: event 2 time.logged: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q should start with %q", err.Error(), want)
	}
}
