package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// RecordVersion is bumped when the on-disk record layout changes.
const RecordVersion = 1

// State is a step in the self-update handover.
type State string

// Actor is the process allowed to advance a state.
type Actor string

const (
	StateStaged     State = "staged"      // predecessor: nothing created yet
	StateStarted    State = "started"     // predecessor (scale-out): successor created and started, health pending
	StateHandover   State = "handover"    // predecessor (scale-out): successor healthy
	StateDrained    State = "drained"     // predecessor (scale-out): no in-flight work, safe to stop me
	StateApplying   State = "applying"    // predecessor then applier: applier running
	StateApplied    State = "applied"     // applier: successor healthy, predecessor gone
	StateRolledBack State = "rolled_back" // applier: successor unhealthy, predecessor restored
	StateFailed     State = "failed"      // applier: error, predecessor restored or still alive
	StateFinalising State = "finalising"  // successor: predecessor removed, reporting pending
	StateAborted    State = "aborted"     // predecessor: gave up on the successor
)

const (
	ActorPredecessor Actor = "predecessor"
	ActorApplier     Actor = "applier"
	ActorSuccessor   Actor = "successor"
	ActorRestored    Actor = "restored"
)

// transitions maps every allowed state change to the actor allowed to make it.
var transitions = map[State]map[State][]Actor{
	StateStaged:   {StateStarted: {ActorPredecessor}, StateApplying: {ActorPredecessor}, StateAborted: {ActorPredecessor}},
	StateStarted:  {StateHandover: {ActorPredecessor}, StateAborted: {ActorPredecessor}},
	StateHandover: {StateDrained: {ActorPredecessor}, StateFinalising: {ActorSuccessor}, StateAborted: {ActorPredecessor}},
	StateDrained:  {StateFinalising: {ActorSuccessor}, StateAborted: {ActorPredecessor}},
	StateApplying: {
		StateApplying:   {ActorApplier},
		StateApplied:    {ActorApplier},
		StateRolledBack: {ActorApplier},
		StateFailed:     {ActorApplier},
	},
	StateApplied: {StateFinalising: {ActorSuccessor}},
}

// ContainerRef identifies a container involved in a handover.
type ContainerRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Number int    `json:"number,omitempty"`
}

// SourceInfo carries everything the successor needs to report the deploy that
// it did not itself run.
type SourceInfo struct {
	RepoName     string `json:"repo_name"`
	SourceURL    string `json:"source_url"`
	FullName     string `json:"full_name"`
	SourceType   string `json:"source_type"`
	Reference    string `json:"reference"`
	ConfigTarget string `json:"config_target"`
	CommitSHA    string `json:"commit_sha"`
	ProjectHash  string `json:"project_hash"`
	JobID        string `json:"job_id"`
	Trigger      string `json:"trigger"`
}

// DeployInfo carries the deploy parameters the applier must reproduce.
type DeployInfo struct {
	TimeoutSeconds int      `json:"timeout_seconds"`
	RecreateMode   string   `json:"recreate_mode"`
	Services       []string `json:"services,omitempty"`
}

// Transition is one entry of a record's audit trail.
type Transition struct {
	State State     `json:"state"`
	Actor Actor     `json:"actor"`
	At    time.Time `json:"at"`
}

// Record is the on-disk handover journal for one self-update.
type Record struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	State       State             `json:"state"`
	Strategy    Strategy          `json:"strategy"`
	Stack       string            `json:"stack"`
	Context     string            `json:"context"`
	Service     string            `json:"service"`
	Predecessor ContainerRef      `json:"predecessor"`
	Successor   ContainerRef      `json:"successor,omitzero"`
	Applier     ContainerRef      `json:"applier,omitzero"`
	Restored    ContainerRef      `json:"restored,omitzero"`
	Source      SourceInfo        `json:"source"`
	Deploy      DeployInfo        `json:"deploy"`
	Labels      map[string]string `json:"labels"`
	Error       string            `json:"error,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	History     []Transition      `json:"history"`
}

// Store persists handover records on the data volume.
type Store struct {
	mu   sync.Mutex
	root string
}

// NewStore returns a store rooted at <dataMountPath>/self-update.
func NewStore(dataMountPath string) *Store {
	return &Store{root: filepath.Join(dataMountPath, "self-update")}
}

// Root returns the directory the store writes to.
func (s *Store) Root() string {
	return s.root
}

func (s *Store) path(id string) string {
	return filepath.Join(s.root, id+".json")
}

// Create writes the initial record. The caller sets everything but the
// bookkeeping fields.
func (s *Store) Create(record *Record) error {
	if record.ID == "" {
		return errors.New("self-update record needs an ID")
	}

	now := time.Now().UTC()
	record.Version = RecordVersion
	record.CreatedAt = now
	record.UpdatedAt = now
	record.History = []Transition{{State: record.State, Actor: ActorPredecessor, At: now}}

	return s.write(*record)
}

// Update advances a record to the next state after checking that actor is
// allowed to make that change.
func (s *Store) Update(record Record, to State, actor Actor) (Record, error) {
	allowed, ok := transitions[record.State][to]
	if !ok {
		return record, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, record.State, to)
	}

	permitted := false

	for _, a := range allowed {
		if a == actor {
			permitted = true
			break
		}
	}

	if !permitted {
		return record, fmt.Errorf("%w: %s -> %s is not for actor %s", ErrInvalidTransition, record.State, to, actor)
	}

	now := time.Now().UTC()
	record.State = to
	record.UpdatedAt = now
	record.History = append(record.History, Transition{State: to, Actor: actor, At: now})

	return record, s.write(record)
}

// Save persists a record without a state change, for reference and error updates.
func (s *Store) Save(record Record) error {
	record.UpdatedAt = time.Now().UTC()

	return s.write(record)
}

// Load reads one record by ID.
func (s *Store) Load(id string) (Record, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, fmt.Errorf("%w: %s", ErrNoRecord, id)
		}

		return Record{}, fmt.Errorf("read self-update record %s: %w", id, err)
	}

	var record Record
	if err = json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("decode self-update record %s: %w", id, err)
	}

	return record, nil
}

// Active returns the single in-flight record, or nil when there is none.
// Malformed records are quarantined rather than failing the caller.
func (s *Store) Active() (*Record, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read self-update directory: %w", err)
	}

	var found []Record

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}

		if strings.HasSuffix(name, ".snapshot.json") || name == poisonFileName {
			continue
		}

		id := strings.TrimSuffix(name, ".json")

		record, loadErr := s.Load(id)
		if loadErr != nil || record.ID != id {
			if quarantineErr := s.Quarantine(id); quarantineErr != nil {
				return nil, quarantineErr
			}

			continue
		}

		found = append(found, record)
	}

	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &found[0], nil
	default:
		ids := make([]string, 0, len(found))
		for _, r := range found {
			ids = append(ids, r.ID)
		}

		return nil, fmt.Errorf("multiple active self-update records: %s", strings.Join(ids, ", "))
	}
}

// Remove deletes a record and its snapshot.
func (s *Store) Remove(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove self-update record %s: %w", id, err)
	}

	if err := os.Remove(s.snapshotPath(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove self-update snapshot %s: %w", id, err)
	}

	return nil
}

// Quarantine renames an unreadable record so it stops blocking startup.
func (s *Store) Quarantine(id string) error {
	if err := os.Rename(s.path(id), s.path(id)+".corrupt"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("quarantine self-update record %s: %w", id, err)
	}

	return nil
}

func (s *Store) snapshotPath(id string) string {
	return filepath.Join(s.root, id+".snapshot.json")
}

// WriteSnapshot stores the predecessor's inspect output for rollback.
func (s *Store) WriteSnapshot(id string, inspect container.InspectResponse) error {
	data, err := json.Marshal(inspect)
	if err != nil {
		return fmt.Errorf("encode self-update snapshot %s: %w", id, err)
	}

	return s.atomicWrite(s.snapshotPath(id), data)
}

// ReadSnapshot returns the stored predecessor snapshot, if one exists.
func (s *Store) ReadSnapshot(id string) (container.InspectResponse, bool, error) {
	data, err := os.ReadFile(s.snapshotPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return container.InspectResponse{}, false, nil
		}

		return container.InspectResponse{}, false, fmt.Errorf("read self-update snapshot %s: %w", id, err)
	}

	var inspect container.InspectResponse
	if err = json.Unmarshal(data, &inspect); err != nil {
		return container.InspectResponse{}, false, fmt.Errorf("decode self-update snapshot %s: %w", id, err)
	}

	return inspect, true, nil
}

func (s *Store) write(record Record) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode self-update record %s: %w", record.ID, err)
	}

	return s.atomicWrite(s.path(record.ID), data)
}

// atomicWrite writes through a temp file, syncs it, renames, then syncs the
// directory, so a crash mid-write cannot leave a half-written record.
func (s *Store) atomicWrite(path string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.root, filesystem.PermDir); err != nil {
		return fmt.Errorf("create self-update directory: %w", err)
	}

	file, err := os.CreateTemp(s.root, ".self-update-*.json")
	if err != nil {
		return fmt.Errorf("create temporary self-update record: %w", err)
	}

	tempPath := file.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write self-update record: %w", err)
	}

	if err = file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync self-update record: %w", err)
	}

	if err = file.Close(); err != nil {
		return fmt.Errorf("close self-update record: %w", err)
	}

	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish self-update record: %w", err)
	}

	dir, err := os.Open(s.root)
	if err != nil {
		return fmt.Errorf("open self-update directory: %w", err)
	}

	defer dir.Close() // nolint:errcheck

	if err = dir.Sync(); err != nil {
		return fmt.Errorf("sync self-update directory: %w", err)
	}

	return nil
}
