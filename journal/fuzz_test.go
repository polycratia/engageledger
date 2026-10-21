package journal

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func FuzzReplay(f *testing.F) {
	var seed bytes.Buffer
	a := NewAppender(&seed)
	for _, e := range []struct {
		typ  string
		body map[string]any
	}{
		{"engagement.opened", map[string]any{"payer": "acme", "cents": int64(250000)}},
		{"change_order.signed", map[string]any{"delta_cents": int64(-5000), "signed_by": []any{"acme", "studio"}}},
		{"deliverable.accepted", map[string]any{"business_days": int64(10)}},
	} {
		if _, err := a.Append(e.typ, e.body); err != nil {
			f.Fatalf("seeding the journal: %v", err)
		}
	}
	f.Add(seed.String())
	f.Add(strings.TrimSuffix(seed.String(), "\n"))
	f.Add(seed.String() + "not a line\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, in string) {
		events, err := Replay(strings.NewReader(in))
		if err != nil {
			var jerr *Error
			if !errors.As(err, &jerr) {
				t.Fatalf("Replay failed with %T, want a *journal.Error: %v", err, err)
			}
			if jerr.Line < 1 {
				t.Fatalf("error names no line: %v", err)
			}
			if events != nil {
				t.Fatalf("Replay returned %d events together with %v", len(events), err)
			}
			return
		}
		var rebuilt bytes.Buffer
		w := NewAppender(&rebuilt)
		for _, e := range events {
			if _, err := w.Append(e.Type, e.Body); err != nil {
				t.Fatalf("re-appending accepted event %d: %v", e.Seq, err)
			}
		}
		want := in
		if want != "" && !strings.HasSuffix(want, "\n") {
			want += "\n"
		}
		if rebuilt.String() != want {
			t.Fatalf("an accepted journal does not round-trip:\n got %q\nwant %q", rebuilt.String(), want)
		}
	})
}
