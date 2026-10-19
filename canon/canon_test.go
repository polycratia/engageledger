package canon

import (
	"encoding/json"
	"errors"
	"testing"
)

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%#v): unexpected error %v", v, err)
	}
	return string(b)
}

func TestRFC8785SortingExample(t *testing.T) {
	v := map[string]any{
		"\u20ac":     "Euro Sign",
		"\r":         "Carriage Return",
		"\ufb33":     "Hebrew Letter Dalet With Dagesh",
		"1":          "One",
		"\U0001f600": "Emoji: Grinning Face",
		"\u0080":     "Control\u007f",
		"\u00f6":     "Latin Small Letter O With Diaeresis",
	}
	want := `{"\r":"Carriage Return","1":"One",` +
		"\"\u0080\":\"Control\u007f\"," +
		"\"\u00f6\":\"Latin Small Letter O With Diaeresis\"," +
		"\"\u20ac\":\"Euro Sign\"," +
		"\"\U0001f600\":\"Emoji: Grinning Face\"," +
		"\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"
	if got := mustMarshal(t, v); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestHTMLAndLineSeparatorsPassThrough(t *testing.T) {
	if got, want := mustMarshal(t, "<a&b>"), `"<a&b>"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	v := map[string]any{"html": "<a&b>", "sep": "a\u2028b\u2029c"}
	want := "{\"html\":\"<a&b>\",\"sep\":\"a\u2028b\u2029c\"}"
	if got := mustMarshal(t, v); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestKeyAboveBMPSortsBeforeFF61(t *testing.T) {
	v := map[string]any{"\uff61": 1, "\U0001f600": 2}
	want := "{\"\U0001f600\":2,\"\uff61\":1}"
	if got := mustMarshal(t, v); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapesOnlyQuoteBackslashAndControls(t *testing.T) {
	in := "q\"b\\s\n\t\r\b\f\u0001\u007f\u2028"
	want := `"q\"b\\s\n\t\r\b\f\u0001` + "\u007f\u2028" + `"`
	if got := mustMarshal(t, in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAcceptedValues(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "null"},
		{true, "true"},
		{false, "false"},
		{"", `""`},
		{int(-7), "-7"},
		{int8(-8), "-8"},
		{int16(300), "300"},
		{int32(-70000), "-70000"},
		{int64(maxSafeInteger), "9007199254740991"},
		{int64(-maxSafeInteger), "-9007199254740991"},
		{uint64(maxSafeInteger), "9007199254740991"},
		{uint8(5), "5"},
		{json.Number("-0"), "0"},
		{json.Number("9007199254740991"), "9007199254740991"},
		{[]any{}, "[]"},
		{map[string]any{}, "{}"},
		{
			[]any{nil, true, int64(1), []any{"x"}, map[string]any{"b": 1, "a": 2}},
			`[null,true,1,["x"],{"a":2,"b":1}]`,
		},
	}
	for _, c := range cases {
		if got := mustMarshal(t, c.in); got != c.want {
			t.Errorf("Marshal(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name string
		in   any
		path string
	}{
		{"float64", 1.5, "$"},
		{"float32", float32(1), "$"},
		{"whole float", 2.0, "$"},
		{
			"nested float names its path",
			map[string]any{"lines": []any{int64(1), int64(2), map[string]any{"label": 1.5}}},
			"$.lines[2].label",
		},
		{"number with fraction", json.Number("1.5"), "$"},
		{"number with exponent", json.Number("1e3"), "$"},
		{"number not a number", json.Number("twelve"), "$"},
		{"empty number", json.Number(""), "$"},
		{"int64 above range", int64(1) << 53, "$"},
		{"int64 below range", -(int64(1) << 53), "$"},
		{"uint64 above range", uint64(1) << 53, "$"},
		{"number above range", json.Number("9007199254740992"), "$"},
		{"number far above range", json.Number("99999999999999999999"), "$"},
		{"invalid UTF-8 in string", []any{"ok", "bad\xff"}, "$[1]"},
		{"invalid UTF-8 in key", map[string]any{"bad\xff": 1}, "$"},
		{"unsupported type", struct{ A int }{1}, "$"},
		{"unsupported type in array", []any{[]string{"x"}}, "$[0]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := Marshal(c.in)
			if err == nil {
				t.Fatalf("Marshal(%#v) = %q, want an error", c.in, b)
			}
			var cerr *Error
			if !errors.As(err, &cerr) {
				t.Fatalf("error %v is not a *canon.Error", err)
			}
			if cerr.Path != c.path {
				t.Errorf("path = %q, want %q (%v)", cerr.Path, c.path, err)
			}
		})
	}
}

func TestErrorMessageCarriesPath(t *testing.T) {
	_, err := Marshal(map[string]any{"lines": []any{map[string]any{"qty": 0.5}}})
	if err == nil {
		t.Fatal("want an error")
	}
	if got, want := err.Error(), "at $.lines[0].qty"; len(got) < len(want) || got[len(got)-len(want):] != want {
		t.Errorf("error %q should end with %q", got, want)
	}
}
