package sse

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is a single SSE payload.
type Event struct {
	Type    string `json:"type"`    // "token", "done", "error"
	Content string `json:"content"`
}

// Broker manages SSE subscriptions and pending event fallback.
type Broker struct {
	mu   sync.Mutex
	subs map[int64][]chan Event
	db   *sql.DB // nil in tests
}

func NewBroker(db *sql.DB) *Broker {
	return &Broker{
		subs: make(map[int64][]chan Event),
		db:   db,
	}
}

// Publish sends an event to all subscribers for agentID.
// Falls back to pending_events DB table if no active subscribers.
func (b *Broker) Publish(agentID int64, event Event) error {
	b.mu.Lock()
	chans := make([]chan Event, len(b.subs[agentID]))
	copy(chans, b.subs[agentID])
	b.mu.Unlock()

	if len(chans) == 0 {
		return b.persist(agentID, event)
	}
	for _, ch := range chans {
		select {
		case ch <- event:
		default:
		}
	}
	return nil
}

func (b *Broker) persist(agentID int64, event Event) error {
	if b.db == nil {
		return nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = b.db.Exec(
		`INSERT INTO pending_events (agent_id, event_json) VALUES (?, ?)`,
		agentID, string(data))
	return err
}

// Subscribe handles a long-lived SSE connection for agentID.
// Drains pending_events first, then streams live events.
// Call this from a handler registered WITHOUT a request timeout.
func (b *Broker) Subscribe(agentID int64, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan Event, 32)

	b.mu.Lock()
	b.subs[agentID] = append(b.subs[agentID], ch)
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		chans := b.subs[agentID]
		for i, c := range chans {
			if c == ch {
				b.subs[agentID] = append(chans[:i], chans[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		close(ch)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Send headers immediately so the client gets the 200 before any events.
	//
	// iOS Safari buffers SSE responses until it has received ~2KB of data before
	// firing any events. We pad with a comment of that size first, then send a
	// named "ready" event. Safari does not reliably fire EventSource.onopen but
	// does fire named-event listeners, so the UI listens for "ready" instead.
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": %s\n\n", string(make([]byte, 2048))) // 2KB padding for iOS Safari
	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	b.drainPending(agentID, w, flusher)

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// Keep the TCP connection alive through mobile NAT / cellular timeouts.
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case evt, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (b *Broker) drainPending(agentID int64, w http.ResponseWriter, flusher http.Flusher) {
	if b.db == nil {
		return
	}
	rows, err := b.db.Query(
		`SELECT id, event_json FROM pending_events WHERE agent_id = ? ORDER BY id ASC`,
		agentID)
	if err != nil {
		return
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		var eventJSON string
		if rows.Scan(&id, &eventJSON) != nil {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", eventJSON)
		flusher.Flush()
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		b.db.Exec(`DELETE FROM pending_events WHERE id = ?`, id)
	}
}
