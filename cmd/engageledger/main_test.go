package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func call(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func seeded(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engagement.jsonl")
	if code, _, errOut := call(t, "seed", path); code != 0 {
		t.Fatalf("seed exited %d: %s", code, errOut)
	}
	return path
}

func output(t *testing.T, cmd, path string) string {
	t.Helper()
	code, out, errOut := call(t, cmd, path)
	if code != 0 {
		t.Fatalf("%s exited %d: %s", cmd, code, errOut)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("%s wrote %q, which does not end in a newline", cmd, out)
	}
	return out
}

// tamperedCopy is the example with one byte of one line changed, written
// beside the original so the original stays as it was.
func tamperedCopy(t *testing.T, src string, line int, old, replacement string) string {
	t.Helper()
	if len(old) != len(replacement) {
		t.Fatalf("the tamper %q -> %q changes the length of the line", old, replacement)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if line < 1 || line > len(lines) {
		t.Fatalf("the example carries %d lines, so there is no line %d", len(lines), line)
	}
	if !strings.Contains(lines[line-1], old) {
		t.Fatalf("line %d does not carry %q", line, old)
	}
	lines[line-1] = strings.Replace(lines[line-1], old, replacement, 1)
	path := filepath.Join(t.TempDir(), "tampered.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestTheExampleAndItsOutputAreTheSameBytesTwice(t *testing.T) {
	first, second := seeded(t), seeded(t)
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("reading the example: %v", err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("reading the second example: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("seeding the example twice wrote other bytes")
	}
	if lines := bytes.Count(a, []byte{'\n'}); lines != 7 {
		t.Errorf("the example carries %d lines, want 7", lines)
	}
	for _, cmd := range []string{"verify", "fold", "clock"} {
		one := output(t, cmd, first)
		if two := output(t, cmd, second); two != one {
			t.Errorf("%s is not byte-stable:\n got %q\nwant %q", cmd, two, one)
		}
	}
}

func TestVerifyAcceptsTheExample(t *testing.T) {
	if got, want := output(t, "verify", seeded(t)), "{\"lines\":7,\"ok\":true}\n"; got != want {
		t.Errorf("verify wrote %q, want %q", got, want)
	}
}

func TestFoldPricesTheExample(t *testing.T) {
	var got struct {
		Cap        int64  `json:"cap"`
		Currency   string `json:"currency"`
		Total      int64  `json:"total"`
		WeakTotal  int64  `json:"weak_total"`
		Milestones []struct {
			ID       string `json:"id"`
			Amount   int64  `json:"amount"`
			Accepted bool   `json:"accepted"`
		} `json:"milestones"`
		ChangeOrders []struct {
			State    string `json:"state"`
			Evidence string `json:"evidence"`
			Delta    int64  `json:"delta"`
		} `json:"change_orders"`
	}
	out := output(t, "fold", seeded(t))
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("fold wrote %q, which is not JSON: %v", out, err)
	}
	if got.Currency != "EUR" || got.Total != 390000 || got.Cap != 300000 || got.WeakTotal != 0 {
		t.Errorf("fold = %+v, want EUR 390000 under a cap of 300000 with nothing weak", got)
	}
	if len(got.Milestones) != 3 {
		t.Fatalf("fold lists %d milestones, want 3", len(got.Milestones))
	}
	if got.Milestones[1].ID != "m2" || got.Milestones[1].Amount != 240000 {
		t.Errorf("milestone 2 = %+v, want m2 repriced to 240000", got.Milestones[1])
	}
	if !got.Milestones[0].Accepted || got.Milestones[1].Accepted {
		t.Error("the fold does not read m1 as the only accepted milestone")
	}
	if len(got.ChangeOrders) != 1 {
		t.Fatalf("fold lists %d change orders, want 1", len(got.ChangeOrders))
	}
	if co := got.ChangeOrders[0]; co.State != "effective" || co.Evidence != "signed" || co.Delta != 40000 {
		t.Errorf("change order = %+v, want an effective signed move of 40000", co)
	}
}

func TestClockReportsTheOpenWindowInUTCAndInTheClientZone(t *testing.T) {
	var got struct {
		OpenWindows []struct {
			Milestone     string `json:"milestone"`
			Seq           int64  `json:"seq"`
			Days          int64  `json:"days"`
			Zone          string `json:"zone"`
			DeadlineUTC   string `json:"deadline_utc"`
			DeadlineLocal string `json:"deadline_local"`
			DeliveredUTC  string `json:"delivered_utc"`
		} `json:"open_windows"`
	}
	out := output(t, "clock", seeded(t))
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("clock wrote %q, which is not JSON: %v", out, err)
	}
	if len(got.OpenWindows) != 1 {
		t.Fatalf("clock lists %d open windows, want 1: %s", len(got.OpenWindows), out)
	}
	w := got.OpenWindows[0]
	if w.Milestone != "m2" || w.Seq != 7 || w.Days != 10 || w.Zone != "Europe/Berlin" {
		t.Errorf("open window = %+v, want the m2 delivery of line 7 over 10 days in Europe/Berlin", w)
	}
	if !strings.HasSuffix(w.DeadlineUTC, "Z") || !strings.HasSuffix(w.DeliveredUTC, "Z") {
		t.Errorf("window = %+v, want its recorded instants in UTC", w)
	}
	if strings.HasSuffix(w.DeadlineLocal, "Z") {
		t.Errorf("the local deadline %q carries no zone offset", w.DeadlineLocal)
	}
	utc, err := time.Parse(time.RFC3339, w.DeadlineUTC)
	if err != nil {
		t.Fatalf("parsing %q: %v", w.DeadlineUTC, err)
	}
	local, err := time.Parse(time.RFC3339, w.DeadlineLocal)
	if err != nil {
		t.Fatalf("parsing %q: %v", w.DeadlineLocal, err)
	}
	if !local.Equal(utc) {
		t.Errorf("the local deadline %s is another instant than %s", w.DeadlineLocal, w.DeadlineUTC)
	}
	if local.Format("15:04") != "17:00" {
		t.Errorf("the local deadline %s is not the close of a business day", w.DeadlineLocal)
	}
}

func TestOneTamperedByteIsRefusedOnItsLine(t *testing.T) {
	cases := []struct {
		name        string
		line        int
		old, change string
		want        string
	}{
		{
			name: "an amount in the contract head breaks the chain on the next line",
			line: 1, old: `"amount":100000`, change: `"amount":100001`,
			want: "line 2: ",
		},
		{
			name: "a delivery of a milestone the contract does not carry",
			line: 7, old: `"milestone":"m2"`, change: `"milestone":"m9"`,
			want: "line 7: ",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := tamperedCopy(t, seeded(t), c.line, c.old, c.change)
			code, out, errOut := call(t, "verify", path)
			if code != 1 {
				t.Fatalf("verify exited %d with %q, want 1", code, out+errOut)
			}
			if out != "" {
				t.Errorf("a refused verify wrote %q to stdout", out)
			}
			if !strings.Contains(errOut, c.want) {
				t.Errorf("verify said %q, want it to name %q", errOut, c.want)
			}
		})
	}
}

func TestUsageAndUnknownCommands(t *testing.T) {
	if code, _, errOut := call(t); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("no arguments exited %d with %q, want 2 and the usage", code, errOut)
	}
	if code, _, errOut := call(t, "invoice", seeded(t)); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("an unknown command exited %d with %q, want 2", code, errOut)
	}
}
