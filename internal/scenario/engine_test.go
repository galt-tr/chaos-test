package scenario

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// newTestEngine builds an Engine with just enough wiring for LoadDirs: a logger and the maps.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{
		d:    Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		defs: map[string]*Definition{},
		runs: map[string]*Run{},
	}
}

func writeScenario(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirs_MergeAndOverride(t *testing.T) {
	base := t.TempDir()
	user := t.TempDir()

	// base: two scenarios, one of which the user dir will override by id.
	writeScenario(t, base, "alpha.yaml", "id: alpha\nname: base alpha\nsteps: []\n")
	writeScenario(t, base, "beta.yaml", "name: base beta\nsteps: []\n") // no id -> defaults to "beta"
	// user: overrides alpha, adds gamma.
	writeScenario(t, user, "alpha.yaml", "id: alpha\nname: user alpha\nsteps: []\n")
	writeScenario(t, user, "gamma.yaml", "id: gamma\nname: user gamma\nsteps: []\n")

	e := newTestEngine(t)
	if err := e.LoadDirs(base, user); err != nil {
		t.Fatalf("LoadDirs: %v", err)
	}

	if got := len(e.defs); got != 3 {
		t.Fatalf("want 3 scenarios (alpha, beta, gamma), got %d: %v", got, keys(e.defs))
	}
	// Later directory wins on an id clash.
	if e.defs["alpha"].Name != "user alpha" {
		t.Errorf("alpha not overridden by user dir: name=%q", e.defs["alpha"].Name)
	}
	// id defaults to the filename stem when the file omits it.
	if _, ok := e.defs["beta"]; !ok {
		t.Errorf("beta (id from filename) missing")
	}
	// Source records the file it came from.
	if e.defs["gamma"].Source != "gamma.yaml" {
		t.Errorf("gamma source = %q, want gamma.yaml", e.defs["gamma"].Source)
	}
}

func TestLoadDirs_SkipsBadFileAndMissingDir(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "good.yaml", "id: good\nname: good\nsteps: []\n")
	writeScenario(t, dir, "bad.yaml", "id: bad\nname: [unterminated\n") // invalid YAML

	e := newTestEngine(t)
	// A missing directory must not fail the whole load; a bad file is skipped, good ones remain.
	if err := e.LoadDirs(dir, filepath.Join(dir, "does-not-exist")); err != nil {
		t.Fatalf("LoadDirs should tolerate a missing dir: %v", err)
	}
	if _, ok := e.defs["good"]; !ok {
		t.Errorf("good scenario missing after a sibling failed to parse")
	}
	if _, ok := e.defs["bad"]; ok {
		t.Errorf("bad (unparseable) scenario should have been skipped")
	}
}

func keys(m map[string]*Definition) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
