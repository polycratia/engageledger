// Package journal keeps the engagement as an append-only JSONL file. Every
// line is the canonical JSON of {seq, prev, type, body}, where prev is the hex
// SHA-256 of the previous line's bytes, so the lines hash into a chain the
// later fold can lean on. Replay rebuilds the events and, when the file is
// broken, names the first line it refuses: a seq that gaps or repeats, a prev
// that does not match, bytes that are not valid UTF-8, or a line that is not
// byte-identical to its own canonical re-encoding, which is what catches the
// duplicate keys and floats encoding/json would otherwise accept in silence.
package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/polycratia/engageledger/canon"
)

// Event is one journal line. Hash is the hex SHA-256 of the line's bytes,
// which the next line carries as its prev; it is not part of the line itself.
type Event struct {
	Seq  int64
	Prev string
	Type string
	Body map[string]any
	Hash string
}

// Error reports the first line Replay refuses, counting lines from 1.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string {
	return "journal: line " + strconv.Itoa(e.Line) + ": " + e.Msg
}

// Appender writes lines that continue a chain.
type Appender struct {
	w    io.Writer
	seq  int64
	prev string
}

// NewAppender starts a chain: its first line gets seq 1 and an empty prev.
func NewAppender(w io.Writer) *Appender {
	return &Appender{w: w}
}

// Resume continues the chain after tail, the last event Replay returned.
func Resume(w io.Writer, tail Event) *Appender {
	return &Appender{w: w, seq: tail.Seq, prev: tail.Hash}
}

// Append writes one line for typ and body and returns the event it wrote. A
// body canonical JSON cannot carry leaves the chain untouched.
func (a *Appender) Append(typ string, body map[string]any) (Event, error) {
	if typ == "" {
		return Event{}, errors.New("journal: event type must not be empty")
	}
	if body == nil {
		body = map[string]any{}
	}
	e := Event{Seq: a.seq + 1, Prev: a.prev, Type: typ, Body: body}
	line, err := canon.Marshal(map[string]any{
		"seq":  e.Seq,
		"prev": e.Prev,
		"type": e.Type,
		"body": e.Body,
	})
	if err != nil {
		return Event{}, err
	}
	e.Hash = hashLine(line)
	if _, err := a.w.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	a.seq, a.prev = e.Seq, e.Hash
	return e, nil
}

// Replay reads every line of the journal in order and returns its events, or
// an *Error naming the first line it refuses.
func Replay(r io.Reader) ([]Event, error) {
	br := bufio.NewReader(r)
	var events []Event
	prev := ""
	for n := 1; ; n++ {
		line, readErr := br.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if len(line) == 0 {
			return events, nil
		}
		e, err := parseLine(bytes.TrimSuffix(line, []byte{'\n'}), n, prev)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
		prev = e.Hash
		if errors.Is(readErr, io.EOF) {
			return events, nil
		}
	}
}

func parseLine(line []byte, n int, wantPrev string) (Event, error) {
	if !utf8.Valid(line) {
		return Event{}, &Error{Line: n, Msg: "is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Event{}, &Error{Line: n, Msg: "is not JSON: " + err.Error()}
	}
	canonical, err := canon.Marshal(v)
	if err != nil {
		return Event{}, &Error{Line: n, Msg: "is not canonical JSON: " + err.Error()}
	}
	if !bytes.Equal(line, canonical) {
		return Event{}, &Error{Line: n, Msg: "is not byte-identical to its own canonical JSON"}
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return Event{}, &Error{Line: n, Msg: "is not a JSON object"}
	}
	if len(obj) != 4 {
		return Event{}, &Error{Line: n, Msg: fmt.Sprintf("has %d keys, want exactly seq, prev, type and body", len(obj))}
	}
	num, ok := obj["seq"].(json.Number)
	if !ok {
		return Event{}, &Error{Line: n, Msg: "has no seq number"}
	}
	seq, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		return Event{}, &Error{Line: n, Msg: "has a seq that is not an integer: " + num.String()}
	}
	if seq != int64(n) {
		return Event{}, &Error{Line: n, Msg: fmt.Sprintf("carries seq %d, want %d", seq, n)}
	}
	prev, ok := obj["prev"].(string)
	if !ok {
		return Event{}, &Error{Line: n, Msg: "has no prev string"}
	}
	if prev != wantPrev {
		if n == 1 {
			return Event{}, &Error{Line: n, Msg: "carries prev " + strconv.Quote(prev) + ", want an empty prev on the first line"}
		}
		return Event{}, &Error{Line: n, Msg: "carries prev " + strconv.Quote(prev) + ", want " + strconv.Quote(wantPrev)}
	}
	typ, ok := obj["type"].(string)
	if !ok {
		return Event{}, &Error{Line: n, Msg: "has no type string"}
	}
	if typ == "" {
		return Event{}, &Error{Line: n, Msg: "has an empty type"}
	}
	body, ok := obj["body"].(map[string]any)
	if !ok {
		return Event{}, &Error{Line: n, Msg: "has no body object"}
	}
	return Event{Seq: seq, Prev: prev, Type: typ, Body: body, Hash: hashLine(line)}, nil
}

func hashLine(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}
