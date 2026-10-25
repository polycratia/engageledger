package approval

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const party = "lantern-co"

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 key: %v", err)
	}
	return pub, priv
}

func body() map[string]any {
	return map[string]any{
		"currency": "EUR",
		"ops": []any{map[string]any{
			"op":        "reprice",
			"milestone": "m2",
			"amount":    int64(90000),
		}},
	}
}

func TestSignedApprovalVerifiesAndRoundTrips(t *testing.T) {
	pub, priv := keypair(t)
	b := body()
	a, err := Sign(priv, party, b)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !a.Strong() {
		t.Error("an ed25519 approval reports itself weak")
	}
	if err := Verify(a, b, Keys{party: pub}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want, err := BodyHash(b)
	if err != nil {
		t.Fatalf("BodyHash: %v", err)
	}
	if a.EvidenceHash != want {
		t.Errorf("evidence hash = %q, want the body hash %q", a.EvidenceHash, want)
	}

	read, err := Read(map[string]any{"approval": a.Object()})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.Party != a.Party || read.Method != a.Method || read.EvidenceHash != a.EvidenceHash {
		t.Errorf("read approval = %+v, want %+v", read, a)
	}
	if hex.EncodeToString(read.Signature) != hex.EncodeToString(a.Signature) {
		t.Error("the signature did not survive the journal object")
	}
	if err := Verify(read, b, Keys{party: pub}); err != nil {
		t.Fatalf("Verify of the read approval: %v", err)
	}
}

func TestFlippingOneByteOfTheBodyRefusesTheApproval(t *testing.T) {
	pub, priv := keypair(t)
	keys := Keys{party: pub}
	a, err := Sign(priv, party, body())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	flipped := body()
	flipped["ops"].([]any)[0].(map[string]any)["milestone"] = "m3"

	err = Verify(a, flipped, keys)
	if err == nil {
		t.Fatal("Verify accepted a body with one byte flipped")
	}
	if !strings.Contains(err.Error(), "names body hash") {
		t.Errorf("error %q should say the approval names another body hash", err)
	}

	// Moving the evidence hash onto the flipped body leaves the signature.
	if a.EvidenceHash, err = BodyHash(flipped); err != nil {
		t.Fatalf("BodyHash: %v", err)
	}
	err = Verify(a, flipped, keys)
	if err == nil {
		t.Fatal("Verify accepted a signature over another body")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("error %q should say the key does not verify the signature", err)
	}
}

func TestVerifyRefusesAKeyTheHeadDoesNotList(t *testing.T) {
	pub, priv := keypair(t)
	a, err := Sign(priv, party, body())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	err = Verify(a, body(), Keys{"oakline-studio": pub})
	if err == nil {
		t.Fatal("Verify accepted an approval the head lists no key for")
	}
	if !strings.Contains(err.Error(), "lists no key") {
		t.Errorf("error %q should say the head lists no key for the party", err)
	}
	if err := Verify(a, body(), nil); err == nil {
		t.Fatal("Verify accepted an approval under a head with no keys")
	}
}

func TestRecordedApprovalKeepsOnlyTheEvidenceHash(t *testing.T) {
	evidence := []byte("From: lantern-co, Subject: approved\n")
	a := Recorded(party, evidence)
	sum := sha256.Sum256(evidence)
	if a.EvidenceHash != hex.EncodeToString(sum[:]) {
		t.Errorf("evidence hash = %q, want the SHA-256 of the external evidence", a.EvidenceHash)
	}
	if len(a.Signature) != 0 || a.Strong() {
		t.Errorf("a recorded approval reports as strong evidence: %+v", a)
	}
	if _, ok := a.Object()["signature"]; ok {
		t.Error("the recorded approval object carries a signature key")
	}
	if err := Verify(a, body(), nil); err != nil {
		t.Errorf("Verify of a recorded approval: %v", err)
	}
}

func TestReadRefuses(t *testing.T) {
	hash := strings.Repeat("ab", sha256.Size)
	sig := strings.Repeat("cd", ed25519.SignatureSize)
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"no approval object", nil, "no approval object"},
		{"an approval that is not an object", "signed", "no approval object"},
		{"no party", map[string]any{"method": "recorded", "evidence_hash": hash}, "no party string"},
		{"an unknown method", map[string]any{"party": party, "method": "fax", "evidence_hash": hash}, "unknown method"},
		{"a short evidence hash", map[string]any{"party": party, "method": "recorded", "evidence_hash": "deadbeef"}, "lowercase hex"},
		{"an uppercase evidence hash", map[string]any{"party": party, "method": "recorded", "evidence_hash": strings.ToUpper(hash)}, "lowercase hex"},
		{"no signature", map[string]any{"party": party, "method": "ed25519", "evidence_hash": hash}, "no signature string"},
		{"a short signature", map[string]any{"party": party, "method": "ed25519", "evidence_hash": hash, "signature": "ab"}, "lowercase hex"},
		{"a recorded approval with a signature", map[string]any{"party": party, "method": "recorded", "evidence_hash": hash, "signature": sig}, "beyond party, method and evidence_hash"},
		{"an extra key", map[string]any{"party": party, "method": "ed25519", "evidence_hash": hash, "signature": sig, "note": "x"}, "beyond party, method, evidence_hash and signature"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{}
			if c.in != nil {
				body["approval"] = c.in
			}
			a, err := Read(body)
			if err == nil {
				t.Fatalf("Read returned %+v, want an error", a)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err, c.want)
			}
		})
	}
}

func TestReadKeys(t *testing.T) {
	pub, _ := keypair(t)
	keys, err := ReadKeys(map[string]any{"keys": map[string]any{party: hex.EncodeToString(pub)}})
	if err != nil {
		t.Fatalf("ReadKeys: %v", err)
	}
	if got, ok := keys[party]; !ok || !got.Equal(pub) {
		t.Errorf("keys[%q] = %x, want %x", party, got, pub)
	}
	if keys, err := ReadKeys(map[string]any{}); err != nil || keys != nil {
		t.Errorf("a head with no keys read as %v, %v", keys, err)
	}
	for name, head := range map[string]map[string]any{
		"keys that are not an object": {"keys": []any{}},
		"a key that is not a string":  {"keys": map[string]any{party: int64(1)}},
		"a key that is not hex":       {"keys": map[string]any{party: "zz"}},
		"a key of the wrong length":   {"keys": map[string]any{party: "abcd"}},
	} {
		if _, err := ReadKeys(head); err == nil {
			t.Errorf("ReadKeys accepted %s", name)
		}
	}
}
