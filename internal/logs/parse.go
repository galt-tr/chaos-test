// Package logs turns a teranode container's raw log stream into filtered, paginated,
// structured lines.
//
// Teranode writes plain text, not JSON:
//
//	<RFC3339 UTC> | <LEVEL padded to 5> | <file:line> | <service> | <message>
//
// and when the container runtime is asked for timestamps each line is additionally
// prefixed with the runtime's own clock, e.g.
//
//	2026-09-17T11:27:41.709533000-04:00 2026-09-17T15:27:41Z | INFO  | rpc/…:1392 | rpc | …
//
// The two clocks matter: the runtime's is nanosecond-precision and is what the `since`
// filter compares against, while the application's is only second-granularity. Cursors
// and ordering therefore use the runtime clock exclusively (see cursor.go).
package logs

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Levels in increasing severity. Anything else a line might carry is treated as unparsed.
var Levels = []string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"}

var levelRank = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3, "FATAL": 4}

// Rank orders a level for the minimum-level filter. Unknown levels rank below DEBUG so a
// line we could not classify is never hidden by a level filter (see Filter.Match).
func Rank(level string) int {
	if r, ok := levelRank[level]; ok {
		return r
	}
	return -1
}

// Line is one parsed log line. A line that could not be parsed keeps its text in Msg with
// an empty Level, and is either attached to the previous line as a continuation or, when
// it has no parent in this window, emitted with Continuation set.
type Line struct {
	Seq int64 `json:"seq"` // position in the raw stream; gaps mean the filter hid lines

	At      string `json:"at"`              // container runtime clock, RFC3339Nano — the ordering key
	LogAt   string `json:"logAt,omitempty"` // teranode's own timestamp, verbatim (second granularity)
	Level   string `json:"level"`
	Service string `json:"service,omitempty"`
	Source  string `json:"source,omitempty"` // file:line
	Msg     string `json:"msg"`

	Cont         []string `json:"cont,omitempty"`         // continuation lines belonging to this one
	Continuation bool     `json:"continuation,omitempty"` // this line IS an orphan continuation

	RootCause string `json:"rootCause,omitempty"` // terminal element of a " -> " chain
	Code      string `json:"code,omitempty"`      // typed code lifted from the root cause
	CodeNum   int    `json:"codeNum,omitempty"`

	Raw string `json:"raw,omitempty"` // original text, only when requested

	at time.Time // parsed form of At, kept unexported so it stays off the wire
}

// At returns the runtime timestamp. Zero when the line carried no parseable prefix.
func (l *Line) Time() time.Time { return l.at }

// ScanOptions controls Scan.
type ScanOptions struct {
	StartSeq int64 // Seq of the first RAW line in this window
	Raw      bool  // populate Line.Raw
}

// Scan parses a raw log window into lines, attaching continuation lines to their parent.
// It returns the lines and the number of RAW physical lines consumed — the two differ
// because continuations are folded into their parent, and cursor arithmetic must count
// raw lines (see cursor.go).
//
// Discrimination is deliberately on the first field parsing as a timestamp rather than on
// the field count: a continuation line of a wrapped error can itself contain " | ", so
// counting fields alone misclassifies it as a fresh entry.
func Scan(raw string, opt ScanOptions) (lines []Line, rawTotal int) {
	raw = strings.TrimSuffix(raw, "\n")
	if raw == "" {
		return nil, 0
	}
	for _, text := range strings.Split(raw, "\n") {
		text = strings.TrimSuffix(text, "\r")
		ts, rest := splitRuntimeStamp(text)
		ln, ok := parseStructured(rest)
		if !ok {
			// A continuation of the line before it, when there is one in this window.
			if n := len(lines); n > 0 {
				lines[n-1].Cont = append(lines[n-1].Cont, rest)
				if opt.Raw {
					lines[n-1].Raw += "\n" + text
				}
				rawTotal++
				continue
			}
			ln = Line{Msg: rest, Continuation: true}
		}
		ln.Seq = opt.StartSeq + int64(rawTotal)
		rawTotal++
		ln.at = ts
		if !ts.IsZero() {
			ln.At = ts.Format(time.RFC3339Nano)
		}
		if opt.Raw {
			ln.Raw = text
		}
		lines = append(lines, ln)
	}
	// Root causes are lifted only once continuations are attached, because the chain
	// frequently spans them.
	for i := range lines {
		if r := Rank(lines[i].Level); r >= 2 || lines[i].Level == "" {
			code, num, cause := RootCause(lines[i].joined())
			if cause != "" && (code != "" || strings.Contains(lines[i].joined(), " -> ")) {
				lines[i].RootCause, lines[i].Code, lines[i].CodeNum = cause, code, num
			}
		}
	}
	return lines, rawTotal
}

// rawCount is how many physical lines this entry consumed.
func (l *Line) rawCount() int { return 1 + len(l.Cont) }

func (l *Line) joined() string {
	if len(l.Cont) == 0 {
		return l.Msg
	}
	return l.Msg + "\n" + strings.Join(l.Cont, "\n")
}

// splitRuntimeStamp peels the container runtime's timestamp prefix. Podman emits it with a
// local UTC offset (…-04:00), not a Z, so RFC3339Nano is the only safe layout.
func splitRuntimeStamp(text string) (time.Time, string) {
	i := strings.IndexByte(text, ' ')
	if i <= 0 {
		return time.Time{}, text
	}
	ts, err := time.Parse(time.RFC3339Nano, text[:i])
	if err != nil {
		return time.Time{}, text
	}
	return ts, text[i+1:]
}

// parseStructured splits the five pipe-delimited fields. SplitN with a limit of 5 is used
// rather than a regexp so that a message containing " | " keeps it verbatim instead of
// being re-split or truncated.
func parseStructured(rest string) (Line, bool) {
	parts := strings.SplitN(rest, " | ", 5)
	if len(parts) < 5 {
		return Line{}, false
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0])); err != nil {
		return Line{}, false
	}
	level := strings.TrimSpace(parts[1])
	if _, ok := levelRank[level]; !ok {
		return Line{}, false
	}
	return Line{
		LogAt:   strings.TrimSpace(parts[0]),
		Level:   level,
		Source:  strings.TrimSpace(parts[2]),
		Service: strings.TrimSpace(parts[3]),
		Msg:     parts[4],
	}, true
}

// codeRe matches a typed teranode error, e.g. "UTXO_CONSENSUS_FROZEN (78): utxo is frozen".
var codeRe = regexp.MustCompile(`^([A-Z][A-Z0-9_]{2,}) \((\d+)\): ([\s\S]*)$`)

// RootCause returns the terminal element of a teranode " -> " causal chain along with its
// typed code when it has one. Teranode wraps errors outward-in, so the last element is the
// actual cause:
//
//	"[validateSubtree] failed -> ... -> UTXO_CONSENSUS_FROZEN (78): [Spend] utxo is frozen"
//
// returns ("UTXO_CONSENSUS_FROZEN", 78, "[Spend] utxo is frozen").
func RootCause(msg string) (code string, num int, cause string) {
	cause = strings.TrimSpace(msg)
	if i := strings.LastIndex(cause, " -> "); i >= 0 {
		cause = strings.TrimSpace(cause[i+4:])
	}
	if m := codeRe.FindStringSubmatch(cause); m != nil {
		n, _ := strconv.Atoi(m[2])
		return m[1], n, strings.TrimSpace(m[3])
	}
	return "", 0, cause
}
