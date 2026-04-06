package demo

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Event is the canonical event schema used across the finance demo pipeline.
type Event struct {
	SessionID string         `json:"session_id"`
	Sequence  int            `json:"sequence,omitempty"`
	Timestamp time.Time      `json:"timestamp,omitempty"`
	Source    string         `json:"source"`
	Stage     string         `json:"stage"`
	Status    string         `json:"status"`
	Title     string         `json:"title"`
	Summary   string         `json:"summary"`
	Data      map[string]any `json:"data,omitempty"`
	RawLog    string         `json:"raw_log,omitempty"`
}

// Session is the stored observer view for a demo run.
type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Events    []Event   `json:"events"`
}

// SessionSummary is the lightweight representation shown in the UI session list.
type SessionSummary struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	EventCount int       `json:"event_count"`
}

// EventEmitter best-effort posts observer events without interrupting the demo.
type EventEmitter struct {
	url    string
	client *http.Client
}

// NewEventEmitterFromEnv builds an emitter from OBSERVER_URL.
func NewEventEmitterFromEnv() *EventEmitter {
	baseURL := strings.TrimSpace(os.Getenv("OBSERVER_URL"))
	if baseURL == "" {
		return nil
	}

	return &EventEmitter{
		url: strings.TrimRight(baseURL, "/") + "/api/events",
		client: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

// Emit sends an event to the observer service on a best-effort basis.
func (e *EventEmitter) Emit(evt Event) {
	if e == nil || e.url == "" || evt.SessionID == "" {
		return
	}

	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	body, err := json.Marshal(evt)
	if err != nil {
		log.Printf("[demo] failed to marshal observer event: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[demo] failed to create observer request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		log.Printf("[demo] failed to send observer event: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices {
		log.Printf("[demo] observer returned status %d", resp.StatusCode)
	}
}
