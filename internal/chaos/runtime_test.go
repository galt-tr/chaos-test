package chaos

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSocketCandidates(t *testing.T) {
	got := socketCandidates("/explicit.sock", fakeEnv(map[string]string{
		"DOCKER_HOST": "unix:///x/docker.sock", "XDG_RUNTIME_DIR": "/run/user/7"}))
	want := []string{"/explicit.sock", "/x/docker.sock", "/var/run/docker.sock",
		"/run/user/7/docker.sock", "/run/user/7/podman/podman.sock", "/run/podman/podman.sock"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
	// A non-unix DOCKER_HOST is skipped, an unset XDG_RUNTIME_DIR yields no relative paths,
	// and a path given twice is listed once.
	got = socketCandidates("/var/run/docker.sock", fakeEnv(map[string]string{"DOCKER_HOST": "tcp://h:2375"}))
	want = []string{"/var/run/docker.sock", "/run/podman/podman.sock"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
}

func TestFirstSocket(t *testing.T) {
	// Unix socket paths are capped near 108 bytes and t.TempDir can be long: keep names short.
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix listen: %v", err)
	}
	defer l.Close()
	notSock := filepath.Join(dir, "docker.sock")
	if err := os.Mkdir(notSock, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.sock")

	if p, reason := firstSocket([]string{missing, notSock, sock}); p != sock || reason != "" {
		t.Fatalf("got %q / %q, want the socket", p, reason)
	}
	// Docker's bind mount of a source missing on the host creates a directory: that must not
	// pass for a socket, and the reason must say what was found there.
	if p, reason := firstSocket([]string{notSock}); p != "" || !strings.Contains(reason, "is a directory") {
		t.Fatalf("got %q / %q, want no path and a directory reason", p, reason)
	}
	if p, reason := firstSocket([]string{missing}); p != "" || !strings.Contains(reason, missing) {
		t.Fatalf("got %q / %q, want no path and the missing path named", p, reason)
	}
}

func TestNoRuntimeWrapsSentinel(t *testing.T) {
	var rt Runtime = &noRuntime{reason: "x is a directory"}
	err := rt.Pause(context.Background(), "c")
	if !errors.Is(err, ErrNoRuntime) || !strings.Contains(err.Error(), "x is a directory") {
		t.Fatalf("err = %v", err)
	}
	if rt.Kind() != "none" || Reason(rt) != "x is a directory" {
		t.Fatalf("kind=%q reason=%q", rt.Kind(), Reason(rt))
	}
	if Reason(&cliRuntime{bin: "docker"}) != "" {
		t.Fatal("a working runtime has no reason")
	}
}

func TestCLIOrder(t *testing.T) {
	if got := cliOrder("docker"); got[0] != "docker" {
		t.Fatalf("RUNTIME=docker: %v", got)
	}
	if got := cliOrder(""); got[0] != "podman" {
		t.Fatalf("default: %v", got)
	}
}
