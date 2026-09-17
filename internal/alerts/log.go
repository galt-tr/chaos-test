package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one alert as recorded in the Log.
type Entry struct {
	Sequence  uint32    `json:"sequence"`
	Type      Type      `json:"type"`
	TypeName  string    `json:"typeName"`
	Timestamp time.Time `json:"timestamp"`
	Hash      string    `json:"hash"`
	Wire      []byte    `json:"wire"`
	Text      string    `json:"text,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Log is the publisher's record of every alert it has built, by sequence number. It is
// the source the sync-stream server answers from, and it persists to a JSON file so a
// CLI invocation can continue where the previous one stopped.
type Log struct {
	mu      sync.RWMutex
	path    string
	entries map[uint32]*Entry
}

// OpenLog loads the log at path (creating an empty one if the file does not exist).
// An empty path gives an in-memory log.
func OpenLog(path string) (*Log, error) {
	l := &Log{path: path, entries: map[uint32]*Entry{}}
	if path == "" {
		return l, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read alert log: %w", err)
	}
	var list []*Entry
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode alert log %s: %w", path, err)
	}
	for _, e := range list {
		l.entries[e.Sequence] = e
	}
	return l, nil
}

// Append records a built alert. It refuses to overwrite a different alert at the same
// sequence, since nodes only ever accept one alert per sequence.
func (l *Log) Append(a *Alert, note string) (*Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.entries[a.Sequence]; ok && existing.Hash != a.Hash {
		return nil, fmt.Errorf("sequence %d already holds alert %s", a.Sequence, existing.Hash)
	}
	text, _ := Describe(a.Wire)
	e := &Entry{
		Sequence:  a.Sequence,
		Type:      a.Type,
		TypeName:  a.Type.String(),
		Timestamp: a.Timestamp,
		Hash:      a.Hash,
		Wire:      a.Wire,
		Text:      text,
		Note:      note,
		CreatedAt: time.Now().UTC(),
	}
	l.entries[a.Sequence] = e
	return e, l.saveLocked()
}

// Get returns the entry at seq.
func (l *Log) Get(seq uint32) (*Entry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.entries[seq]
	return e, ok
}

// Wire returns the wire bytes at seq.
func (l *Log) Wire(seq uint32) ([]byte, bool) {
	e, ok := l.Get(seq)
	if !ok {
		return nil, false
	}
	return e.Wire, true
}

// Latest returns the highest sequence recorded (0 when empty, matching genesis).
func (l *Log) Latest() uint32 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var max uint32
	for s := range l.entries {
		if s > max {
			max = s
		}
	}
	return max
}

// Entries returns all entries ordered by sequence.
func (l *Log) Entries() []*Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Entry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

func (l *Log) saveLocked() error {
	if l.path == "" {
		return nil
	}
	list := make([]*Entry, 0, len(l.entries))
	for _, e := range l.entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Sequence < list[j].Sequence })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
