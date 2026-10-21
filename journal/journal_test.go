package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mkLine(seq int, prev, typ, body string) string {
	return fmt.Sprintf(`{"body":%s,"prev":%q,"seq":%d,"type":%q}`, body, prev, seq, typ)
}

func TestAppendWritesACanonicalChain(t *testing.T) {
	var buf bytes.Buffer
	a := NewAppender(&buf)

	first, err := a.Append("engagement.opened", map[string]any{"payer": "acme", "cents": int64(250000)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	wantFirst := `{"body":{"cents":250000,"payer":"acme"},"prev":"","seq":1,"type":"engagement.opened"}`
	if got, want := buf.String(), wantFirst+"\n"; got != want {
		t.Fatalf("first line = %q, want %q", got, want)
	}
	if first.Seq != 1 || first.Prev != "" {
		t.Errorf("first event = %+v, want seq 1 with an empty prev", first)
	}
	if first.Hash != hashLine([]byte(wantFirst)) {
		t.Errorf("first hash = %q, want the hash of its own line", first.Hash)
	}

	second, err := a.Append("change_order.signed", map[string]any{"delta_cents": int64(-5000)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if second.Seq != 2 || second.Prev != first.Hash {
		t.Errorf("second event = %+v, want seq 2 after %q", second, first.Hash)
	}
	wantSecond := mkLine(2, first.Hash, "change_order.signed", `{"delta_cents":-5000}`)
	if got, want := buf.String(), wantFirst+"\n"+wantSecond+"\n"; got != want {
		t.Errorf("journal =\n%q\nwant\n%q", got, want)
	}
}

func TestReplayReturnsTheEventsAndResumeContinues(t *testing.T) {
	var buf bytes.Buffer
	a := NewAppender(&buf)
	for _, typ := range []string{"engagement.opened", "change_order.signed", "deliverable.accepted"} {
		if _, err := a.Append(typ, map[string]any{"at": "2026-01-05", "cents": int64(100)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	events, err := Replay(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d carries seq %d", i, e.Seq)
		}
		if at, ok := e.Body["at"].(string); !ok || at != "2026-01-05" {
			t.Errorf("event %d body at = %#v", i, e.Body["at"])
		}
		if cents, ok := e.Body["cents"].(json.Number); !ok || cents.String() != "100" {
			t.Errorf("event %d body cents = %#v, want a json.Number", i, e.Body["cents"])
		}
	}
	if events[0].Prev != "" {
		t.Errorf("first prev = %q, want empty", events[0].Prev)
	}
	if events[1].Prev != events[0].Hash || events[2].Prev != events[1].Hash {
		t.Error("replayed events do not hash into a chain")
	}

	fourth, err := Resume(&buf, events[2]).Append("invoice.issued", nil)
	if err != nil {
		t.Fatalf("Append after Resume: %v", err)
	}
	if fourth.Seq != 4 || fourth.Prev != events[2].Hash {
		t.Errorf("resumed event = %+v, want seq 4 after %q", fourth, events[2].Hash)
	}
	again, err := Replay(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Replay after Resume: %v", err)
	}
	if len(again) != 4 {
		t.Fatalf("got %d events, want 4", len(again))
	}
	if len(again[3].Body) != 0 {
		t.Errorf("a nil body replayed as %#v, want an empty object", again[3].Body)
	}
}

func TestReplayAcceptsALastLineWithoutNewline(t *testing.T) {
	var buf bytes.Buffer
	a := NewAppender(&buf)
	for _, typ := range []string{"engagement.opened", "invoice.issued"} {
		if _, err := a.Append(typ, map[string]any{"cents": int64(1)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	events, err := Replay(strings.NewReader(strings.TrimSuffix(buf.String(), "\n")))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("got %d events, want 2", len(events))
	}
}

func TestReplayOfNothingIsAnEmptyJournal(t *testing.T) {
	events, err := Replay(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want none", len(events))
	}
}

func TestReplayNamesTheFirstBadLine(t *testing.T) {
	good1 := mkLine(1, "", "engagement.opened", `{}`)
	h1 := hashLine([]byte(good1))
	good2 := mkLine(2, h1, "change_order.signed", `{"delta_cents":-5000}`)
	h2 := hashLine([]byte(good2))
	second := func(body string) string {
		return good1 + "\n" + body + "\n"
	}

	cases := []struct {
		name string
		in   string
		line int
		want string
	}{
		{"prev on the first line", mkLine(1, h1, "a", `{}`) + "\n", 1, "prev"},
		{"first seq is not one", mkLine(2, "", "a", `{}`) + "\n", 1, "seq 2"},
		{"seq gap", second(mkLine(3, h1, "a", `{}`)), 2, "seq 3"},
		{"seq repeat", second(mkLine(1, h1, "a", `{}`)), 2, "seq 1"},
		{"prev mismatch", second(mkLine(2, h2, "a", `{}`)), 2, "prev"},
		{"invalid UTF-8", second(`{"body":{},"prev":"` + h1 + `","seq":2,"type":"a` + "\xff" + `"}`), 2, "UTF-8"},
		{"duplicate key", second(`{"body":{},"body":{},"prev":"` + h1 + `","seq":2,"type":"a"}`), 2, "canonical"},
		{"float in the body", second(`{"body":{"cents":1.0},"prev":"` + h1 + `","seq":2,"type":"a"}`), 2, "canonical"},
		{"keys out of order", second(`{"seq":2,"prev":"` + h1 + `","type":"a","body":{}}`), 2, "canonical"},
		{"trailing space", second(good2 + " "), 2, "canonical"},
		{"two values on one line", second(good2 + good2), 2, "canonical"},
		{"carriage return", good1 + "\r\n", 1, "canonical"},
		{"seq above the canonical range", `{"body":{},"prev":"","seq":9007199254740992,"type":"a"}` + "\n", 1, "canonical"},
		{"not an object", second("[]"), 2, "object"},
		{"missing key", second(`{"prev":"` + h1 + `","seq":2,"type":"a"}`), 2, "keys"},
		{"extra key", second(`{"body":{},"note":"x","prev":"` + h1 + `","seq":2,"type":"a"}`), 2, "keys"},
		{"body is not an object", second(`{"body":[],"prev":"` + h1 + `","seq":2,"type":"a"}`), 2, "body"},
		{"empty type", second(`{"body":{},"prev":"` + h1 + `","seq":2,"type":""}`), 2, "type"},
		{"type is not a string", second(`{"body":{},"prev":"` + h1 + `","seq":2,"type":1}`), 2, "type"},
		{"seq is not a number", second(`{"body":{},"prev":"` + h1 + `","seq":"2","type":"a"}`), 2, "seq"},
		{"prev is not a string", second(`{"body":{},"prev":2,"seq":2,"type":"a"}`), 2, "prev"},
		{"blank line", good1 + "\n\n" + good2 + "\n", 2, "JSON"},
		{"third line breaks the chain", good1 + "\n" + good2 + "\n" + mkLine(3, "", "a", `{}`) + "\n", 3, "prev"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := Replay(strings.NewReader(c.in))
			if err == nil {
				t.Fatalf("Replay(%q) returned %d events, want an error", c.in, len(events))
			}
			if events != nil {
				t.Errorf("Replay returned %d events together with %v", len(events), err)
			}
			var jerr *Error
			if !errors.As(err, &jerr) {
				t.Fatalf("error %v is not a *journal.Error", err)
			}
			if jerr.Line != c.line {
				t.Errorf("line = %d, want %d (%v)", jerr.Line, c.line, err)
			}
			if !strings.Contains(jerr.Msg, c.want) {
				t.Errorf("message %q should mention %q", jerr.Msg, c.want)
			}
		})
	}
}

func TestErrorStartsWithTheLineNumber(t *testing.T) {
	_, err := Replay(strings.NewReader("{}\n"))
	if err == nil {
		t.Fatal("want an error")
	}
	if got, want := err.Error(), "journal: line 1: "; !strings.HasPrefix(got, want) {
		t.Errorf("error %q should start with %q", got, want)
	}
}

func TestAppendRefusesAnEmptyType(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewAppender(&buf).Append("", nil); err == nil {
		t.Fatal("want an error")
	}
	if buf.Len() != 0 {
		t.Errorf("a refused append wrote %q", buf.String())
	}
}

func TestAppendRefusesABodyCanonicalJSONCannotCarry(t *testing.T) {
	var buf bytes.Buffer
	a := NewAppender(&buf)
	if _, err := a.Append("invoice.issued", map[string]any{"cents": 12.5}); err == nil {
		t.Fatal("want an error for a float amount")
	}
	if buf.Len() != 0 {
		t.Fatalf("a refused append wrote %q", buf.String())
	}
	e, err := a.Append("invoice.issued", map[string]any{"cents": int64(1250)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if e.Seq != 1 || e.Prev != "" {
		t.Errorf("event after a refused append = %+v, want seq 1 with an empty prev", e)
	}
}
