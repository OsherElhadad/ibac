package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/huang195/ibac/internal/demo"
)

//go:embed static/*
var staticFiles embed.FS

type observerStore struct {
	mu        sync.RWMutex
	sessions  map[string]*demo.Session
	watchers  map[string]map[chan demo.Event]struct{}
	jobs      map[string]*sparcJob
	jobQueue  []string
}

func newObserverStore() *observerStore {
	return &observerStore{
		sessions: make(map[string]*demo.Session),
		watchers: make(map[string]map[chan demo.Event]struct{}),
		jobs:     make(map[string]*sparcJob),
	}
}

type sparcJob struct {
	ID        string          `json:"id"`
	WorkerID  string          `json:"worker_id,omitempty"`
	Status    string          `json:"status"`
	Request   json.RawMessage `json:"request,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type sparcJobCreateRequest struct {
	Request json.RawMessage `json:"request"`
}

type sparcJobResultRequest struct {
	Status   string          `json:"status"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
}

func (s *observerStore) addEvent(evt demo.Event) demo.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	session, ok := s.sessions[evt.SessionID]
	if !ok {
		session = &demo.Session{
			ID:        evt.SessionID,
			Title:     evt.Title,
			StartedAt: evt.Timestamp,
		}
		s.sessions[evt.SessionID] = session
	}

	if evt.Title != "" && session.Title == "" {
		session.Title = evt.Title
	}

	evt.Sequence = len(session.Events) + 1
	session.Events = append(session.Events, evt)
	session.UpdatedAt = evt.Timestamp
	if session.StartedAt.IsZero() {
		session.StartedAt = evt.Timestamp
	}
	if session.Title == "" {
		session.Title = humanizeTitle(evt.SessionID)
	}

	for ch := range s.watchers[evt.SessionID] {
		select {
		case ch <- evt:
		default:
		}
	}

	return evt
}

func (s *observerStore) listSessions() []demo.SessionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summaries := make([]demo.SessionSummary, 0, len(s.sessions))
	for _, session := range s.sessions {
		summaries = append(summaries, demo.SessionSummary{
			ID:         session.ID,
			Title:      session.Title,
			StartedAt:  session.StartedAt,
			UpdatedAt:  session.UpdatedAt,
			EventCount: len(session.Events),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
	})
	return summaries
}

func (s *observerStore) getSession(id string) (*demo.Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[id]
	if !ok {
		return nil, false
	}

	clone := &demo.Session{
		ID:        session.ID,
		Title:     session.Title,
		StartedAt: session.StartedAt,
		UpdatedAt: session.UpdatedAt,
		Events:    append([]demo.Event(nil), session.Events...),
	}
	return clone, true
}

func (s *observerStore) subscribe(id string) chan demo.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan demo.Event, 32)
	if s.watchers[id] == nil {
		s.watchers[id] = make(map[chan demo.Event]struct{})
	}
	s.watchers[id][ch] = struct{}{}
	return ch
}

func (s *observerStore) unsubscribe(id string, ch chan demo.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if watchers := s.watchers[id]; watchers != nil {
		delete(watchers, ch)
		if len(watchers) == 0 {
			delete(s.watchers, id)
		}
	}
	close(ch)
}

func cloneRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func cloneJob(job *sparcJob) *sparcJob {
	if job == nil {
		return nil
	}
	clone := *job
	clone.Request = cloneRawJSON(job.Request)
	clone.Response = cloneRawJSON(job.Response)
	return &clone
}

func (s *observerStore) createJob(request json.RawMessage) *sparcJob {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	job := &sparcJob{
		ID:        fmt.Sprintf("sparc-job-%d", now.UnixNano()),
		Status:    "queued",
		Request:   cloneRawJSON(request),
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.jobs[job.ID] = job
	s.jobQueue = append(s.jobQueue, job.ID)
	return cloneJob(job)
}

func (s *observerStore) claimJob(workerID string, wait time.Duration) (*sparcJob, bool) {
	deadline := time.Now().Add(wait)
	for {
		s.mu.Lock()
		if len(s.jobQueue) > 0 {
			jobID := s.jobQueue[0]
			s.jobQueue = s.jobQueue[1:]
			job := s.jobs[jobID]
			if job != nil {
				job.Status = "claimed"
				job.WorkerID = workerID
				job.UpdatedAt = time.Now().UTC()
				claimed := cloneJob(job)
				s.mu.Unlock()
				return claimed, true
			}
		}
		s.mu.Unlock()

		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (s *observerStore) getJob(id string) (*sparcJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	return cloneJob(job), true
}

func (s *observerStore) completeJob(id, status string, response json.RawMessage, errText string) (*sparcJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}

	job.Status = status
	job.Response = cloneRawJSON(response)
	job.Error = errText
	job.UpdatedAt = time.Now().UTC()
	return cloneJob(job), true
}

func humanizeTitle(id string) string {
	if strings.TrimSpace(id) == "" {
		return "Finance Demo Session"
	}
	return fmt.Sprintf("Finance Demo %s", id)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[observer] failed to write JSON: %v", err)
	}
}

func main() {
	log.Println("[observer] starting on :7070")

	store := newObserverStore()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var evt demo.Event
		if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(evt.SessionID) == "" {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(evt.Source) == "" {
			http.Error(w, "source is required", http.StatusBadRequest)
			return
		}

		evt = store.addEvent(evt)
		writeJSON(w, http.StatusAccepted, evt)
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, store.listSessions())
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		if path == "" {
			http.NotFound(w, r)
			return
		}

		if strings.HasSuffix(path, "/stream") {
			sessionID := strings.TrimSuffix(path, "/stream")
			sessionID = strings.TrimSuffix(sessionID, "/")
			handleStream(store, sessionID, w, r)
			return
		}

		sessionID := strings.TrimSuffix(path, "/")
		session, ok := store.getSession(sessionID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, session)
	})

	mux.HandleFunc("/api/sparc-jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req sparcJobCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if len(req.Request) == 0 {
			http.Error(w, "request is required", http.StatusBadRequest)
			return
		}

		job := store.createJob(req.Request)
		writeJSON(w, http.StatusAccepted, job)
	})

	mux.HandleFunc("/api/sparc-jobs/claim", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
		if workerID == "" {
			workerID = "host-sparc"
		}

		waitSeconds := 25
		if raw := strings.TrimSpace(r.URL.Query().Get("wait_seconds")); raw != "" {
			if parsed, err := time.ParseDuration(raw + "s"); err == nil {
				waitSeconds = int(parsed.Seconds())
			}
		}

		job, ok := store.claimJob(workerID, time.Duration(waitSeconds)*time.Second)
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, job)
	})

	mux.HandleFunc("/api/sparc-jobs/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/sparc-jobs/")
		path = strings.TrimSuffix(path, "/")
		if path == "" {
			http.NotFound(w, r)
			return
		}

		if strings.HasSuffix(path, "result") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			jobID := strings.TrimSuffix(path, "/result")
			jobID = strings.TrimSuffix(jobID, "/")

			var req sparcJobResultRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if req.Status == "" {
				req.Status = "completed"
			}

			job, ok := store.completeJob(jobID, req.Status, req.Response, req.Error)
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, job)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		job, ok := store.getJob(path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, job)
	})

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("[observer] failed to load static files: %v", err)
	}

	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/assets/", http.StripPrefix("/assets/", fileServer))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, staticFS, "index.html")
	})

	if err := http.ListenAndServe(":7070", mux); err != nil {
		log.Fatalf("[observer] failed to serve: %v", err)
	}
}

func handleStream(store *observerStore, sessionID string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := store.subscribe(sessionID)
	defer store.unsubscribe(sessionID, ch)

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-ch:
			payload, err := json.Marshal(evt)
			if err != nil {
				log.Printf("[observer] failed to marshal stream event: %v", err)
				continue
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}
