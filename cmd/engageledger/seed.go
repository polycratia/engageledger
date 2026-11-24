package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"github.com/polycratia/engageledger/acceptance"
	"github.com/polycratia/engageledger/approval"
	"github.com/polycratia/engageledger/bizday"
	"github.com/polycratia/engageledger/journal"
	"github.com/polycratia/engageledger/terms"
)

const (
	exampleClient     = "client-co"
	exampleContractor = "studio-co"
	exampleCurrency   = "EUR"
)

// exampleKey derives the ed25519 key of a demo party from its name, so the
// signatures of the example journal are the same bytes on every machine.
func exampleKey(party string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("engageledger example " + party))
	return ed25519.NewKeyFromSeed(seed[:])
}

func examplePub(party string) string {
	return hex.EncodeToString(exampleKey(party).Public().(ed25519.PublicKey))
}

func seed(path string) error {
	data, err := exampleJournal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// exampleJournal is a seven-line engagement: three milestones opened on a
// business-day calendar, one change order both parties signed, the first
// milestone delivered and accepted, and the second delivered with its
// acceptance window still running.
func exampleJournal() ([]byte, error) {
	cal, err := bizday.New("2026.1", "Europe/Berlin", nil,
		[]string{"2026-04-03", "2026-04-06", "2026-05-01"}, "17:00")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	a := journal.NewAppender(&buf)

	head := map[string]any{
		"parties":  map[string]any{"client": exampleClient, "contractor": exampleContractor},
		"currency": exampleCurrency,
		"milestones": []any{
			map[string]any{"id": "m1", "title": "Discovery", "amount": int64(100000), "criteria": []any{"report delivered"}},
			map[string]any{"id": "m2", "title": "Build", "amount": int64(200000), "criteria": []any{"tests pass", "handbook written"}},
			map[string]any{"id": "m3", "title": "Handover", "amount": int64(50000), "criteria": []any{"environment handed over"}},
		},
		"tm_cap":     int64(300000),
		"acceptance": map[string]any{"window_days": int64(10), "retest_window": "restart"},
		"calendar":   cal.Object(),
		"keys": map[string]any{
			exampleClient:     examplePub(exampleClient),
			exampleContractor: examplePub(exampleContractor),
		},
	}
	if _, err := a.Append(terms.EventOpened, head); err != nil {
		return nil, err
	}

	proposal := map[string]any{
		"currency": exampleCurrency,
		"ops": []any{
			map[string]any{"op": "reprice", "milestone": "m2", "amount": int64(240000)},
		},
	}
	co, err := a.Append(terms.EventProposed, proposal)
	if err != nil {
		return nil, err
	}
	for _, party := range []string{exampleClient, exampleContractor} {
		signed, err := approval.Sign(exampleKey(party), party, proposal)
		if err != nil {
			return nil, err
		}
		if _, err := a.Append(terms.EventApproved, map[string]any{
			"by":           party,
			"change_order": co.Hash,
			"approval":     signed.Object(),
		}); err != nil {
			return nil, err
		}
	}

	if _, err := acceptance.Deliver(a, cal, "m1", time.Date(2026, 4, 7, 8, 0, 0, 0, time.UTC), 10); err != nil {
		return nil, err
	}
	if _, err := a.Append(acceptance.EventAccepted, map[string]any{"milestone": "m1"}); err != nil {
		return nil, err
	}
	if _, err := acceptance.Deliver(a, cal, "m2", time.Date(2026, 4, 20, 9, 30, 0, 0, time.UTC), 10); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
