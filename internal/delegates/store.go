package delegates

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store keeps delegate runs under one vault's memory directory.
type Store struct {
	root string
	mu   sync.RWMutex
}

// NewStore creates the run directory.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("delegates: store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("delegates: create store: %w", err)
	}
	return &Store{root: abs}, nil
}

// Save writes one run atomically.
func (s *Store) Save(run *Run) error {
	if run == nil || run.RunID == "" {
		return errors.New("delegates: run has no id")
	}
	run.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAtomic(filepath.Join(s.root, run.RunID+".json"), append(payload, '\n'))
}

// Get loads one run.
func (s *Store) Get(id string) (*Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, err := os.ReadFile(filepath.Join(s.root, id+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("delegates: unknown run %q", id)
		}
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, fmt.Errorf("delegates: run %q is unreadable: %w", id, err)
	}
	return &run, nil
}

// List returns newest runs first.
func (s *Store) List(limit int) ([]*Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var runs []*Run
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.root, entry.Name()))
		if err != nil {
			continue
		}
		var run Run
		if json.Unmarshal(raw, &run) != nil {
			continue
		}
		runs = append(runs, &run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].UpdatedAt > runs[j].UpdatedAt })
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// AppendEvent appends one event and saves the run.
func (s *Store) AppendEvent(run *Run, event Event) error {
	if run == nil {
		return errors.New("delegates: nil run")
	}
	run.Events = append(run.Events, event)
	return s.Save(run)
}

func writeAtomic(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".run-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
