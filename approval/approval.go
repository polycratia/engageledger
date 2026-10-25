// Package approval carries the evidence behind the approval of a change
// order. An ed25519 approval signs the canonical JSON of the body it approves
// and verifies against the key the contract head lists for the party, so the
// journal alone can check it. A recorded approval - an email or an e-sign
// export kept outside the journal - leaves behind only the SHA-256 of that
// evidence, which nothing here can tie back to the body it stands for, so a
// fold that prices the engagement can report it apart.
package approval

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/polycratia/engageledger/canon"
)

// Method is how an approval is evidenced.
type Method string

const (
	// MethodEd25519 signs the canonical JSON of the approved body.
	MethodEd25519 Method = "ed25519"
	// MethodRecorded names the SHA-256 of evidence kept outside the journal.
	MethodRecorded Method = "recorded"
)

// Approval is one party's approval and the evidence behind it. EvidenceHash
// is the hex SHA-256 of the canonical JSON of the approved body for an
// ed25519 approval and of the external evidence for a recorded one, where
// Signature stays empty.
type Approval struct {
	Party        string
	Method       Method
	EvidenceHash string
	Signature    []byte
}

// Keys are the ed25519 public keys a contract head lists, by party.
type Keys map[string]ed25519.PublicKey

// Strong reports whether the journal itself can check the approval.
func (a Approval) Strong() bool { return a.Method == MethodEd25519 }

// Object returns the approval as the object a journal body carries it as.
func (a Approval) Object() map[string]any {
	obj := map[string]any{
		"party":         a.Party,
		"method":        string(a.Method),
		"evidence_hash": a.EvidenceHash,
	}
	if len(a.Signature) > 0 {
		obj["signature"] = hex.EncodeToString(a.Signature)
	}
	return obj
}

// BodyHash is the hex SHA-256 of the canonical JSON of body, which an ed25519
// approval names as its evidence hash.
func BodyHash(body map[string]any) (string, error) {
	b, err := canon.Marshal(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Sign returns the ed25519 approval of party over body.
func Sign(key ed25519.PrivateKey, party string, body map[string]any) (Approval, error) {
	msg, err := canon.Marshal(body)
	if err != nil {
		return Approval{}, err
	}
	sum := sha256.Sum256(msg)
	return Approval{
		Party:        party,
		Method:       MethodEd25519,
		EvidenceHash: hex.EncodeToString(sum[:]),
		Signature:    ed25519.Sign(key, msg),
	}, nil
}

// Recorded returns the recorded approval of party over evidence the journal
// does not hold, of which only the SHA-256 is kept.
func Recorded(party string, evidence []byte) Approval {
	sum := sha256.Sum256(evidence)
	return Approval{Party: party, Method: MethodRecorded, EvidenceHash: hex.EncodeToString(sum[:])}
}

// Read reads the approval object a change_order.approved body carries.
func Read(body map[string]any) (Approval, error) {
	obj, ok := body["approval"].(map[string]any)
	if !ok {
		return Approval{}, errors.New("has no approval object")
	}
	a, err := read(obj)
	if err != nil {
		return Approval{}, fmt.Errorf("carries an approval that %w", err)
	}
	return a, nil
}

func read(obj map[string]any) (Approval, error) {
	party, err := text(obj, "party")
	if err != nil {
		return Approval{}, err
	}
	method, err := text(obj, "method")
	if err != nil {
		return Approval{}, err
	}
	evidence, err := text(obj, "evidence_hash")
	if err != nil {
		return Approval{}, err
	}
	if _, err := decodeHex(evidence, sha256.Size, "an evidence_hash"); err != nil {
		return Approval{}, err
	}
	a := Approval{Party: party, Method: Method(method), EvidenceHash: evidence}
	switch a.Method {
	case MethodEd25519:
		sig, err := text(obj, "signature")
		if err != nil {
			return Approval{}, err
		}
		if a.Signature, err = decodeHex(sig, ed25519.SignatureSize, "a signature"); err != nil {
			return Approval{}, err
		}
		if len(obj) != 4 {
			return Approval{}, errors.New("carries keys beyond party, method, evidence_hash and signature")
		}
	case MethodRecorded:
		if len(obj) != 3 {
			return Approval{}, errors.New("is recorded but carries keys beyond party, method and evidence_hash")
		}
	default:
		return Approval{}, fmt.Errorf("names an unknown method %q", method)
	}
	return a, nil
}

// ReadKeys reads the optional keys object of a contract.opened body, which
// lists one ed25519 public key per party. A head without keys reads as no
// keys, which refuses every ed25519 approval later in the journal.
func ReadKeys(body map[string]any) (Keys, error) {
	v, ok := body["keys"]
	if !ok {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("has a keys value that is not an object")
	}
	keys := make(Keys, len(obj))
	for party, raw := range obj {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("lists a key for %q that is not a string", party)
		}
		pub, err := decodeHex(s, ed25519.PublicKeySize, "a key for "+strconv.Quote(party))
		if err != nil {
			return nil, err
		}
		keys[party] = ed25519.PublicKey(pub)
	}
	return keys, nil
}

// Verify checks an approval against the body it approves and the keys the
// contract head lists. A recorded approval passes on its shape alone: its
// hash is of evidence the journal does not hold, so nothing here can tie it
// to body.
func Verify(a Approval, body map[string]any, keys Keys) error {
	switch a.Method {
	case MethodRecorded:
		return nil
	case MethodEd25519:
	default:
		return fmt.Errorf("names an unknown method %q", a.Method)
	}
	key, ok := keys[a.Party]
	if !ok {
		return fmt.Errorf("signs for %q, for whom the contract head lists no key", a.Party)
	}
	msg, err := canon.Marshal(body)
	if err != nil {
		return fmt.Errorf("approves a body canonical JSON cannot carry: %w", err)
	}
	sum := sha256.Sum256(msg)
	if want := hex.EncodeToString(sum[:]); a.EvidenceHash != want {
		return fmt.Errorf("names body hash %s, but the body it approves hashes to %s", short(a.EvidenceHash), short(want))
	}
	if !ed25519.Verify(key, msg, a.Signature) {
		return fmt.Errorf("carries a signature the key of %q does not verify", a.Party)
	}
	return nil
}

func text(m map[string]any, key string) (string, error) {
	s, ok := m[key].(string)
	if !ok {
		return "", fmt.Errorf("has no %s string", key)
	}
	if s == "" {
		return "", fmt.Errorf("has an empty %s", key)
	}
	return s, nil
}

func decodeHex(s string, n int, what string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != n || s != strings.ToLower(s) {
		return nil, fmt.Errorf("has %s that is not %d bytes of lowercase hex: %q", what, n, s)
	}
	return b, nil
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
