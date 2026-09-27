package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Poison records a self-update that failed for a specific commit, so the retry
// logic cannot loop on it. A new commit clears the block by not matching.
type Poison struct {
	Context     string    `json:"context"`
	Stack       string    `json:"stack"`
	CommitSHA   string    `json:"commit_sha"`
	ProjectHash string    `json:"project_hash"`
	Reason      string    `json:"reason"`
	FailedAt    time.Time `json:"failed_at"`
	Attempts    int       `json:"attempts"`
}

// poisonFileName is reserved: [Store.Active] must not read it as a record.
const poisonFileName = "poison.json"

func poisonKey(contextName, stack string) string {
	return contextName + ":" + stack
}

func (s *Store) poisonPath() string {
	return filepath.Join(s.root, poisonFileName)
}

func (s *Store) loadPoison() (map[string]Poison, error) {
	data, err := os.ReadFile(s.poisonPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Poison{}, nil
		}

		return nil, fmt.Errorf("read self-update poison file: %w", err)
	}

	entries := map[string]Poison{}
	if err = json.Unmarshal(data, &entries); err != nil {
		// A corrupt poison file must not block deployments forever: treat it as
		// empty and let the next failure rewrite it.
		return map[string]Poison{}, nil // nolint:nilerr
	}

	return entries, nil
}

// AddPoison blocks further attempts for the same commit and project hash.
// Re-adding the same key increments the attempt counter.
func (s *Store) AddPoison(p Poison) error {
	entries, err := s.loadPoison()
	if err != nil {
		return err
	}

	key := poisonKey(p.Context, p.Stack)

	if existing, ok := entries[key]; ok && existing.CommitSHA == p.CommitSHA && existing.ProjectHash == p.ProjectHash {
		p.Attempts = existing.Attempts + 1
	} else {
		p.Attempts = 1
	}

	p.FailedAt = time.Now().UTC()
	entries[key] = p

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode self-update poison file: %w", err)
	}

	return s.atomicWrite(s.poisonPath(), data)
}

// IsPoisoned reports whether this exact commit and project hash already failed.
func (s *Store) IsPoisoned(contextName, stack, commitSHA, projectHash string) (Poison, bool, error) {
	entries, err := s.loadPoison()
	if err != nil {
		return Poison{}, false, err
	}

	p, ok := entries[poisonKey(contextName, stack)]
	if !ok || p.CommitSHA != commitSHA || p.ProjectHash != projectHash {
		return Poison{}, false, nil
	}

	return p, true, nil
}

// ClearPoison removes the block for a stack.
func (s *Store) ClearPoison(contextName, stack string) error {
	entries, err := s.loadPoison()
	if err != nil {
		return err
	}

	key := poisonKey(contextName, stack)
	if _, ok := entries[key]; !ok {
		return nil
	}

	delete(entries, key)

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode self-update poison file: %w", err)
	}

	return s.atomicWrite(s.poisonPath(), data)
}
