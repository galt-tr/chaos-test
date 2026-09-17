// Package observe watches the stack (nodes, hub, services, outpoints) and publishes a
// live Snapshot plus an ordered Event stream that the API serves over SSE and the scenario
// engine asserts against.
package observe

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event is one thing that happened, as seen by the harness.
type Event struct {
	ID      uint64         `json:"id"`
	Time    time.Time      `json:"time"`
	Kind    string         `json:"kind"`           // tip, alert_seq, utxo, rejected_tx, invalid_block, chaos, alert, mine, tx, scenario, log, error
	Node    string         `json:"node,omitempty"` // node name when applicable
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// Bus is an in-memory ring of events with fan-out to subscribers.
type Bus struct {
	mu      sync.RWMutex
	seq     atomic.Uint64
	ring    []Event
	max     int
	subs    map[uint64]chan Event
	nextSub uint64
}

// NewBus returns a bus keeping the last max events.
func NewBus(max int) *Bus {
	return &Bus{max: max, subs: map[uint64]chan Event{}}
}

// Publish appends an event and delivers it to subscribers (dropping for slow ones).
func (b *Bus) Publish(kind, node, msg string, data map[string]any) Event {
	e := Event{ID: b.seq.Add(1), Time: time.Now().UTC(), Kind: kind, Node: node, Message: msg, Data: data}
	b.mu.Lock()
	b.ring = append(b.ring, e)
	if len(b.ring) > b.max {
		b.ring = b.ring[len(b.ring)-b.max:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
	b.mu.Unlock()
	return e
}

// Subscribe returns a channel of future events and an unsubscribe func.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// Since returns events with ID > after, oldest first.
func (b *Bus) Since(after uint64) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []Event
	for _, e := range b.ring {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out
}

// Recent returns the last n events.
func (b *Bus) Recent(n int) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if n > len(b.ring) {
		n = len(b.ring)
	}
	return append([]Event(nil), b.ring[len(b.ring)-n:]...)
}
