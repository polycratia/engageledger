// Package canon turns Go values into the canonical JSON bytes a journal entry
// is signed over, following RFC 8785: object keys are ordered by UTF-16 code
// units and only quote, backslash and control characters are escaped, so <, >,
// &, U+2028 and U+2029 travel through as plain UTF-8. Values a verifier could
// read back differently - floats, oversized integers, invalid UTF-8 - are
// refused together with the JSON path that carries them.
package canon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const maxSafeInteger = 1<<53 - 1

// Error reports a value canonical JSON cannot carry, naming the JSON path of
// the offending value, for example $.lines[2].label.
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string {
	return "canon: " + e.Msg + " at " + e.Path
}

// Marshal writes v as canonical JSON bytes.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, v, "$"); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(buf *bytes.Buffer, v any, path string) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		if !utf8.ValidString(t) {
			return &Error{Path: path, Msg: "invalid UTF-8 in string"}
		}
		writeString(buf, t)
		return nil
	case int:
		return writeInt(buf, int64(t), path)
	case int8:
		return writeInt(buf, int64(t), path)
	case int16:
		return writeInt(buf, int64(t), path)
	case int32:
		return writeInt(buf, int64(t), path)
	case int64:
		return writeInt(buf, t, path)
	case uint8:
		return writeInt(buf, int64(t), path)
	case uint16:
		return writeInt(buf, int64(t), path)
	case uint32:
		return writeInt(buf, int64(t), path)
	case uint:
		return writeUint(buf, uint64(t), path)
	case uint64:
		return writeUint(buf, t, path)
	case json.Number:
		return writeNumber(buf, string(t), path)
	case float32, float64:
		return &Error{Path: path, Msg: fmt.Sprintf("%T is not allowed, signed amounts must be exact integers", v)}
	case []any:
		return encodeArray(buf, t, path)
	case map[string]any:
		return encodeObject(buf, t, path)
	default:
		return &Error{Path: path, Msg: fmt.Sprintf("unsupported type %T", v)}
	}
}

func encodeArray(buf *bytes.Buffer, items []any, path string) error {
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encode(buf, item, path+"["+strconv.Itoa(i)+"]"); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

func encodeObject(buf *bytes.Buffer, m map[string]any, path string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		if !utf8.ValidString(k) {
			return &Error{Path: path, Msg: "invalid UTF-8 in object key " + strconv.Quote(k)}
		}
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareUTF16)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeString(buf, k)
		buf.WriteByte(':')
		if err := encode(buf, m[k], path+"."+k); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// compareUTF16 orders strings by their UTF-16 code units, which places a
// character above U+FFFF (a surrogate pair) before U+E000..U+FFFF.
func compareUTF16(a, b string) int {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			if ua[i] < ub[i] {
				return -1
			}
			return 1
		}
	}
	return len(ua) - len(ub)
}

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == '\b':
			buf.WriteString(`\b`)
		case c == '\f':
			buf.WriteString(`\f`)
		case c == '\n':
			buf.WriteString(`\n`)
		case c == '\r':
			buf.WriteString(`\r`)
		case c == '\t':
			buf.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(buf, `\u%04x`, c)
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
}

func writeInt(buf *bytes.Buffer, n int64, path string) error {
	if n > maxSafeInteger || n < -maxSafeInteger {
		return &Error{Path: path, Msg: fmt.Sprintf("integer %d is outside +/-(2^53-1)", n)}
	}
	buf.WriteString(strconv.FormatInt(n, 10))
	return nil
}

func writeUint(buf *bytes.Buffer, n uint64, path string) error {
	if n > maxSafeInteger {
		return &Error{Path: path, Msg: fmt.Sprintf("integer %d is outside +/-(2^53-1)", n)}
	}
	buf.WriteString(strconv.FormatUint(n, 10))
	return nil
}

func writeNumber(buf *bytes.Buffer, s, path string) error {
	if strings.ContainsAny(s, ".eE") {
		return &Error{Path: path, Msg: "json.Number " + strconv.Quote(s) + " has a fraction or exponent"}
	}
	if !isJSONInteger(s) {
		return &Error{Path: path, Msg: "json.Number " + strconv.Quote(s) + " is not an integer"}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return &Error{Path: path, Msg: "integer " + s + " is outside +/-(2^53-1)"}
		}
		return &Error{Path: path, Msg: "json.Number " + strconv.Quote(s) + " is not an integer"}
	}
	return writeInt(buf, n, path)
}

func isJSONInteger(s string) bool {
	digits := strings.TrimPrefix(s, "-")
	if digits == "" {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return len(digits) == 1 || digits[0] != '0'
}
