package main

import (
	"testing"
	"time"

	"github.com/huang195/ibac/internal/demo"
)

func TestObserverStoreAddsSequencedEvents(t *testing.T) {
	store := newObserverStore()
	first := store.addEvent(demo.Event{
		SessionID: "session-1",
		Source:    "finance-agent",
		Stage:     "user_turn",
		Status:    "info",
		Title:     "First",
		Timestamp: time.Now().UTC(),
	})
	second := store.addEvent(demo.Event{
		SessionID: "session-1",
		Source:    "sparc",
		Stage:     "reflection",
		Status:    "blocked",
		Title:     "Second",
		Timestamp: time.Now().UTC().Add(time.Second),
	})

	if first.Sequence != 1 {
		t.Fatalf("expected first event sequence 1, got %d", first.Sequence)
	}
	if second.Sequence != 2 {
		t.Fatalf("expected second event sequence 2, got %d", second.Sequence)
	}

	session, ok := store.getSession("session-1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if len(session.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(session.Events))
	}
}

func TestObserverStoreListsNewestSessionFirst(t *testing.T) {
	store := newObserverStore()
	store.addEvent(demo.Event{
		SessionID: "older",
		Source:    "finance-agent",
		Stage:     "user_turn",
		Status:    "info",
		Title:     "Older",
		Timestamp: time.Now().UTC(),
	})
	store.addEvent(demo.Event{
		SessionID: "newer",
		Source:    "finance-agent",
		Stage:     "user_turn",
		Status:    "info",
		Title:     "Newer",
		Timestamp: time.Now().UTC().Add(time.Minute),
	})

	sessions := store.listSessions()
	if len(sessions) != 2 {
		t.Fatalf("expected 2 session summaries, got %d", len(sessions))
	}
	if sessions[0].ID != "newer" {
		t.Fatalf("expected newest session first, got %s", sessions[0].ID)
	}
}
