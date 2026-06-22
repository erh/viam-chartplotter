package vc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
)

// diskRoutesStore persists robot-local routes to a JSON file. These are the
// fallback used when the location metadata can't be written (no cloud access /
// insufficient permission). A route saved here lives only on this machine until
// it's promoted to the location (see doRoutesPromote). Mirrors diskNavStore's
// tmp+rename write so a crash never leaves a half-written file.
type diskRoutesStore struct {
	mu     sync.Mutex
	path   string
	routes []routeRec
}

func newDiskRoutesStore(path string) (*diskRoutesStore, error) {
	s := &diskRoutesStore{path: path}
	if err := s.load(); err != nil {
		return nil, errors.Wrapf(err, "loading robot routes from %q", path)
	}
	return s, nil
}

func (s *diskRoutesStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var routes []routeRec
	if err := json.Unmarshal(data, &routes); err != nil {
		return err
	}
	s.routes = routes
	return nil
}

// save flushes to disk. Caller must hold s.mu.
func (s *diskRoutesStore) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.routes, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *diskRoutesStore) list() []routeRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]routeRec, len(s.routes))
	copy(out, s.routes)
	return out
}

func (s *diskRoutesStore) get(id string) (routeRec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.routes {
		if r.ID == id {
			return r, true
		}
	}
	return routeRec{}, false
}

func (s *diskRoutesStore) upsert(r routeRec) error {
	r.Scope = "" // scope is derived at list time, not stored
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.routes {
		if s.routes[i].ID == r.ID {
			s.routes[i] = r
			return s.save()
		}
	}
	s.routes = append(s.routes, r)
	return s.save()
}

func (s *diskRoutesStore) delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.routes[:0]
	found := false
	for _, r := range s.routes {
		if r.ID == id {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return false, nil
	}
	s.routes = out
	return true, s.save()
}

func (s *diskRoutesStore) rename(id string, fields map[string]interface{}) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.routes {
		if s.routes[i].ID != id {
			continue
		}
		applyRouteFields(&s.routes[i], fields)
		return true, s.save()
	}
	return false, nil
}
