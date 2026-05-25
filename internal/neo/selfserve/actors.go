package selfserve

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ActorStore struct {
	mu     sync.RWMutex
	actors map[string]actorState
}

type actorState struct {
	ActorID   string    `json:"actorId"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
}

func NewActorStore() *ActorStore {
	return &ActorStore{actors: map[string]actorState{}}
}

func ActorsHandler() http.Handler {
	return NewActorStore()
}

func (s *ActorStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && (path == "/actors" || path == "/actors/create"):
		id := uuidV4()
		state := actorState{ActorID: id, State: "connected", CreatedAt: time.Now().UTC()}
		s.mu.Lock()
		s.actors[id] = state
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"actorId": id, "state": state.State, "region": "local"})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/actors/"):
		id := strings.TrimPrefix(path, "/actors/")
		s.mu.RLock()
		state, ok := s.actors[id]
		s.mu.RUnlock()
		if !ok {
			state = actorState{ActorID: id, State: "connected", CreatedAt: time.Now().UTC()}
		}
		writeJSON(w, http.StatusOK, state)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/actors/"):
		id := strings.TrimPrefix(path, "/actors/")
		s.mu.Lock()
		delete(s.actors, id)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "actorId": id})
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "not_implemented", "message": fmt.Sprintf("self-serve actors does not implement %s %s", r.Method, r.URL.Path)})
	}
}
