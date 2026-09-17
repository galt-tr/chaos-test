package logs

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// Cursor is an opaque position in a container's raw log stream.
//
// Incremental tailing cannot be driven from the application's own timestamps: they are
// second-granularity while the runtime's `since` filter is nanosecond and inclusive, so at
// teranode's ~30 lines/s any `since` derived from them either re-delivers or loses up to a
// second's worth of lines on every poll.
//
// So the cursor records the runtime timestamp of the last line delivered plus a Mark, a
// hash of that line's content. The next fetch rewinds one second behind it and resumes
// from just after the entry whose timestamp and Mark both match.
//
// Matching on content rather than on a line count is deliberate. A count is only valid if
// the next window starts where the last one did, which is false in the ordinary case: the
// first fetch is seeded with `tail=N` and returns the last N lines, while the follow-up
// asks `since=TS-1s` and can return far more. Counting there silently re-delivers the
// difference. Content matching is correct however the window was seeded, whatever the
// timestamp granularity, and when many lines share one timestamp.
type Cursor struct {
	TS      time.Time // runtime timestamp of the last line delivered
	Mark    uint64    // content hash of that line
	Scanned int64     // raw lines delivered so far; the source of Line.Seq
}

// Rewind is how far behind the cursor the next window starts. It only has to cover clock
// jitter and equal timestamps, so a second is generous.
const Rewind = time.Second

// Encode renders a cursor for the wire. Clients must treat it as opaque.
func (c Cursor) Encode() string {
	return fmt.Sprintf("%d:%d:%d", c.TS.UnixNano(), c.Mark, c.Scanned)
}

// DecodeCursor parses a cursor minted by Encode.
func DecodeCursor(s string) (Cursor, error) {
	var c Cursor
	if s == "" {
		return c, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return c, fmt.Errorf("malformed cursor %q", s)
	}
	ns, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return c, fmt.Errorf("malformed cursor timestamp: %w", err)
	}
	if c.Mark, err = strconv.ParseUint(parts[1], 10, 64); err != nil {
		return c, fmt.Errorf("malformed cursor mark %q", parts[1])
	}
	if c.Scanned, err = strconv.ParseInt(parts[2], 10, 64); err != nil || c.Scanned < 0 {
		return c, fmt.Errorf("malformed cursor position %q", parts[2])
	}
	c.TS = time.Unix(0, ns)
	return c, nil
}

// Since is the window start to request of the runtime for this cursor.
func (c Cursor) Since() time.Time { return c.TS.Add(-Rewind) }

// markOf hashes an entry's identifying content. Continuations are excluded so that an
// entry whose stack trace is still being written does not change its mark between polls.
func markOf(l *Line) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(l.LogAt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(l.Level))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(l.Source))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(l.Service))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(l.Msg))
	return h.Sum64()
}

// locate finds the entry the cursor last delivered and returns its index. It scans
// backwards so that, if an identical line genuinely repeats within the rewind window, the
// most recent occurrence wins and nothing is delivered twice.
func locate(window []Line, c Cursor) (int, bool) {
	for i := len(window) - 1; i >= 0; i-- {
		if window[i].at.Equal(c.TS) && markOf(&window[i]) == c.Mark {
			return i, true
		}
	}
	return -1, false
}
