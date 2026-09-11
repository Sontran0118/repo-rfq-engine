package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Event is one change to the book, pushed to every connected blotter.
type Event struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// Broker fans events out to server-sent-event subscribers.
//
// A trading blotter must not poll: a dealer's quote is only useful if the client
// sees it while it is still live. SSE is chosen over WebSockets because the flow
// is strictly one-way — the browser already has REST for writes — and SSE
// reconnects on its own without any client-side retry logic.
type Broker struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]struct{})}
}

// Publish delivers ev to every current subscriber.
//
// Sends are non-blocking. A browser tab that has been throttled to a stop must
// not be able to wedge the trade path — a subscriber whose buffer is full is
// skipped, and its next poll of the REST API will resynchronise it.
func (b *Broker) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (b *Broker) subscribe() chan Event {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
	close(ch)
}

// Subscribers reports the number of connected clients.
func (b *Broker) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Handle serves the SSE stream at GET /api/events.
func (b *Broker) Handle(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the point of streaming.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := b.subscribe()
	defer b.unsubscribe(ch)

	// Heartbeat as an SSE comment. Idle connections are otherwise reaped by
	// intermediaries after ~60s, and the browser would silently stop updating.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case ev := <-ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("event: " + ev.Type + "\ndata: " + string(data) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
