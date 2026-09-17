package chaos

import "testing"

func frame(typ byte, payload string) []byte {
	n := len(payload)
	return append([]byte{typ, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, payload...)
}

func TestDemuxLogStream(t *testing.T) {
	var b []byte
	b = append(b, frame(1, "line one\n")...)
	b = append(b, frame(2, "err line\n")...)
	b = append(b, frame(1, "line three\n")...)
	if got, want := demuxLogStream(b), "line one\nerr line\nline three\n"; got != want {
		t.Fatalf("demux = %q, want %q", got, want)
	}
	// TTY (raw) output has no headers and must pass through unchanged.
	raw := "2026-09-17T05:48:58Z | INFO | plain text\n"
	if got := demuxLogStream([]byte(raw)); got != raw {
		t.Fatalf("raw = %q, want %q", got, raw)
	}
	// A truncated final frame yields whatever payload is present.
	trunc := append(frame(1, "ok\n"), 1, 0, 0, 0, 0, 0, 0, 10, 'p', 'a', 'r', 't')
	if got := demuxLogStream(trunc); got != "ok\npart" {
		t.Fatalf("truncated = %q", got)
	}
}
