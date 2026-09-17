package logs

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeLog stands in for the container runtime's log store so the cursor can be tested
// without a container: Since replays what a `since=` fetch would return, Tail what a
// `tail=` fetch would.
type fakeLog struct {
	at   []time.Time
	text []string
}

func (f *fakeLog) add(at time.Time, text string) {
	f.at = append(f.at, at)
	f.text = append(f.text, text)
}

func (f *fakeLog) render(from int) string {
	var b strings.Builder
	for i := from; i < len(f.text); i++ {
		// Podman prefixes its own clock, with a local offset rather than a Z.
		b.WriteString(f.at[i].Format(time.RFC3339Nano) + " " + f.text[i] + "\n")
	}
	return b.String()
}

// Since is inclusive, matching Docker/podman's `!created.Before(since)`.
func (f *fakeLog) Since(t time.Time) string {
	for i, at := range f.at {
		if !at.Before(t) {
			return f.render(i)
		}
	}
	return ""
}

func (f *fakeLog) Tail(n int) string {
	from := len(f.text) - n
	if from < 0 {
		from = 0
	}
	return f.render(from)
}

func app(ts time.Time, level, src, service, msg string) string {
	return fmt.Sprintf("%s | %-5s | %s | %s | %s", ts.UTC().Format(time.RFC3339), level, src, service, msg)
}

func TestParseFields(t *testing.T) {
	base := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name, in                    string
		level, service, source, msg string
	}{
		{"plain", app(base, "INFO", "rpc/h.go:1", "rpc", "hello"), "INFO", "rpc", "rpc/h.go:1", "hello"},
		{"literal pipe in message", app(base, "INFO", "a.go:1", "p2p", "cmd a|b"), "INFO", "p2p", "a.go:1", "cmd a|b"},
		// The whole reason SplitN(…,5) is used instead of a regexp.
		{"separator inside message", app(base, "WARN", "a.go:1", "bval", "peers: a | b | c"), "WARN", "bval", "a.go:1", "peers: a | b | c"},
		{"padded level trimmed", "2026-09-17T15:00:00Z | INFO  | a.go:1 | asset | x", "INFO", "asset", "a.go:1", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, raw := Scan("2026-09-17T11:00:00.000000000-04:00 "+c.in, ScanOptions{})
			if len(got) != 1 || raw != 1 {
				t.Fatalf("want 1 line/1 raw, got %d/%d", len(got), raw)
			}
			l := got[0]
			if l.Level != c.level || l.Service != c.service || l.Source != c.source || l.Msg != c.msg {
				t.Fatalf("got level=%q service=%q source=%q msg=%q", l.Level, l.Service, l.Source, l.Msg)
			}
			if l.Time().IsZero() {
				t.Fatal("runtime timestamp not parsed")
			}
		})
	}
}

func TestContinuationAttachesAndInheritsLevel(t *testing.T) {
	base := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	raw := strings.Join([]string{
		"2026-09-17T11:00:00.000000000-04:00 " + app(base, "ERROR", "stval/S.go:1174", "stval", "processing 1 missing txs with error:"),
		"2026-09-17T11:00:00.000000001-04:00   PROCESSING (4): [validateSubtree] found 1 errors",
		// A continuation that itself contains " | " — field counting would misread this
		// as a fresh entry, which is why the timestamp discriminator is load-bearing.
		"2026-09-17T11:00:00.000000002-04:00   -> UTXO_CONSENSUS_FROZEN (78): [Spend] utxo is frozen for a | b at height 117",
	}, "\n")

	lines, rawTotal := Scan(raw, ScanOptions{})
	if len(lines) != 1 {
		t.Fatalf("want 1 entry (parent + 2 continuations), got %d", len(lines))
	}
	if rawTotal != 3 {
		t.Fatalf("want rawTotal 3, got %d", rawTotal)
	}
	if len(lines[0].Cont) != 2 {
		t.Fatalf("want 2 continuations, got %d", len(lines[0].Cont))
	}
	if lines[0].Code != "UTXO_CONSENSUS_FROZEN" || lines[0].CodeNum != 78 {
		t.Fatalf("root cause not lifted: code=%q num=%d", lines[0].Code, lines[0].CodeNum)
	}

	// The regression this guards: a level filter must not delete stack traces. The
	// continuation carries no level of its own, so it survives via its parent.
	f, err := ParseFilter("INFO", "", "utxo is frozen", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Match(&lines[0]) {
		t.Fatal("grep over continuations failed: root cause text is not reachable")
	}
}

func TestUnparsedLineAlwaysSurvivesLevelFilter(t *testing.T) {
	raw := "2026-09-17T11:00:00.000000000-04:00 panic: runtime error: index out of range"
	lines, _ := Scan(raw, ScanOptions{})
	f, _ := ParseFilter("ERROR", "", "", false, false)
	if !f.Match(&lines[0]) {
		t.Fatal("an unparsed line was hidden by a level filter; panics must always show")
	}
}

func TestRootCause(t *testing.T) {
	code, num, cause := RootCause("a -> b -> UTXO_CONSENSUS_FROZEN (78): utxo is frozen")
	if code != "UTXO_CONSENSUS_FROZEN" || num != 78 || cause != "utxo is frozen" {
		t.Fatalf("got %q %d %q", code, num, cause)
	}
	if code, _, cause := RootCause("no chain here"); code != "" || cause != "no chain here" {
		t.Fatalf("got %q %q", code, cause)
	}
}

// tailThenFollow is the core correctness property: seeding with a tail and then following
// with the cursor must deliver every new line exactly once — no duplicates, no gaps.
func TestCursorNoDuplicatesNoGaps(t *testing.T) {
	for _, gran := range []struct {
		name string
		step time.Duration
	}{
		{"nanosecond", time.Millisecond},
		// Several lines sharing one timestamp is where a `ts > cursor` comparison silently
		// drops lines; second granularity is what podman gives on a busy container.
		{"second (many lines share a timestamp)", 0},
	} {
		t.Run(gran.name, func(t *testing.T) {
			f := &fakeLog{}
			base := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
			at := base
			emit := func(n int, tag string) []string {
				var want []string
				for i := 0; i < n; i++ {
					if gran.step > 0 {
						at = at.Add(gran.step)
					}
					msg := fmt.Sprintf("%s-%d", tag, i)
					f.add(at, app(at, "INFO", "a.go:1", "rpc", msg))
					want = append(want, msg)
				}
				return want
			}
			emit(40, "old")

			seed := Build(f.Tail(10), Request{Tail: 10, Limit: 100})
			if len(seed.Lines) != 10 {
				t.Fatalf("seed: want 10 lines, got %d", len(seed.Lines))
			}
			cur, err := DecodeCursor(seed.NextCursor)
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for round := 0; round < 4; round++ {
				want := emit(7, fmt.Sprintf("r%d", round))
				page := Build(f.Since(cur.Since()), Request{Cursor: cur, HasCursor: true, Limit: 100})
				if page.Reset {
					t.Fatalf("round %d: unexpected reset (%s)", round, page.ResetReason)
				}
				for _, l := range page.Lines {
					got = append(got, l.Msg)
				}
				if cur, err = DecodeCursor(page.NextCursor); err != nil {
					t.Fatal(err)
				}
				_ = want
			}

			var want []string
			for round := 0; round < 4; round++ {
				for i := 0; i < 7; i++ {
					want = append(want, fmt.Sprintf("r%d-%d", round, i))
				}
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("stream mismatch\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

func TestCursorIdleContainerDeliversNothingTwice(t *testing.T) {
	f := &fakeLog{}
	at := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		f.add(at, app(at, "INFO", "a.go:1", "rpc", fmt.Sprintf("m%d", i)))
	}
	seed := Build(f.Tail(5), Request{Tail: 5, Limit: 100})
	cur, _ := DecodeCursor(seed.NextCursor)
	for i := 0; i < 3; i++ {
		page := Build(f.Since(cur.Since()), Request{Cursor: cur, HasCursor: true, Limit: 100})
		if len(page.Lines) != 0 {
			t.Fatalf("idle poll %d re-delivered %d lines", i, len(page.Lines))
		}
		cur, _ = DecodeCursor(page.NextCursor)
	}
}

func TestCursorResetOnRestart(t *testing.T) {
	f := &fakeLog{}
	at := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		at = at.Add(time.Millisecond)
		f.add(at, app(at, "INFO", "a.go:1", "rpc", fmt.Sprintf("m%d", i)))
	}
	seed := Build(f.Tail(20), Request{Tail: 20, Limit: 100})
	cur, _ := DecodeCursor(seed.NextCursor)

	// The container restarts: a brand new, much shorter log.
	fresh := &fakeLog{}
	at2 := at.Add(time.Hour)
	fresh.add(at2, app(at2, "INFO", "a.go:1", "rpc", "restarted"))

	page := Build(fresh.Since(cur.Since()), Request{Cursor: cur, HasCursor: true, Limit: 100})
	if !page.Reset {
		t.Fatal("want reset after a restart")
	}
	if len(page.Lines) != 1 || page.Lines[0].Msg != "restarted" {
		t.Fatalf("want the fresh window, got %d lines", len(page.Lines))
	}
	if page.Lines[0].Seq != 0 {
		t.Fatalf("reset must renumber from 0, got seq %d", page.Lines[0].Seq)
	}
}

func TestLimitKeepsNewestAndReportsDropped(t *testing.T) {
	var b strings.Builder
	at := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		at = at.Add(time.Millisecond)
		b.WriteString(at.Format(time.RFC3339Nano) + " " + app(at, "INFO", "a.go:1", "rpc", fmt.Sprintf("m%d", i)) + "\n")
	}
	p := Build(b.String(), Request{Limit: 100})
	if len(p.Lines) != 100 {
		t.Fatalf("want 100 lines, got %d", len(p.Lines))
	}
	if p.Dropped != 900 {
		t.Fatalf("want dropped 900, got %d", p.Dropped)
	}
	if p.Lines[len(p.Lines)-1].Msg != "m999" {
		t.Fatalf("want the newest kept, got %q", p.Lines[len(p.Lines)-1].Msg)
	}
}

func TestFacetsArePreFilter(t *testing.T) {
	at := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(at.Format(time.RFC3339Nano) + " " + app(at, "DEBUG", "a.go:1", "asset", "noise") + "\n")
	}
	b.WriteString(at.Format(time.RFC3339Nano) + " " + app(at, "ERROR", "a.go:1", "bval", "boom") + "\n")

	f, _ := ParseFilter("ERROR", "", "", false, false)
	p := Build(b.String(), Request{Limit: 100, Filter: f})
	if len(p.Lines) != 1 {
		t.Fatalf("want 1 line past the filter, got %d", len(p.Lines))
	}
	// The point of the facets: "10 DEBUG hidden" must remain visible while filtered.
	if p.Levels["DEBUG"] != 10 || p.Services["asset"] != 10 {
		t.Fatalf("facets must count the pre-filter window, got %v / %v", p.Levels, p.Services)
	}
}

func TestServiceIncludeExclude(t *testing.T) {
	at := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	mk := func(service string) Line {
		l, _ := parseStructured(app(at, "INFO", "a.go:1", service, "x"))
		return l
	}
	inc, _ := ParseFilter("", "bval,p2p", "", false, false)
	if !inc.Match(ptr(mk("bval"))) || inc.Match(ptr(mk("asset"))) {
		t.Fatal("include list wrong")
	}
	exc, _ := ParseFilter("", "!alert", "", false, false)
	if exc.Match(ptr(mk("alert"))) || !exc.Match(ptr(mk("bval"))) {
		t.Fatal("exclude list wrong")
	}
}

func TestBadRegexRejected(t *testing.T) {
	if _, err := ParseFilter("", "", "a(", true, false); err == nil {
		t.Fatal("want an error for an invalid regex")
	}
	if _, err := ParseFilter("NOPE", "", "", false, false); err == nil {
		t.Fatal("want an error for an unknown level")
	}
}

func ptr(l Line) *Line { return &l }
