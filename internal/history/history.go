package history

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// Submission is one saved shell command plus the slurm jobs it produced.
// (No memory field: the cluster sizes memory from the CPU count.)
type Submission struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Command   string    `json:"command"`
	JobName   string    `json:"job_name"`
	Partition string    `json:"partition,omitempty"`
	CPUs      int       `json:"cpus,omitempty"`
	Time      string    `json:"time,omitempty"`
	WorkDir   string    `json:"workdir,omitempty"`
	// Kind is "script" for directly-submitted sbatch files, "" for wrapped
	// shell commands. CPUs/Time are unset for script submissions (the
	// script's own directives apply).
	Kind   string   `json:"kind,omitempty"`
	JobIDs []string `json:"job_ids"`
}

// Store persists submissions to a JSON file.
type Store struct {
	path string
	mu   sync.Mutex
}

// New opens (or creates) the store at path.
func New(path string) *Store { return &Store{path: path} }

// Path returns the backing file.
func (s *Store) Path() string { return s.path }

// List returns submissions newest-first. Missing file => empty list.
func (s *Store) List() ([]Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Store) listLocked() ([]Submission, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var subs []Submission
	if err := json.Unmarshal(data, &subs); err != nil {
		return nil, err
	}
	sort.Slice(subs, func(i, j int) bool {
		return subs[i].Timestamp.After(subs[j].Timestamp)
	})
	return subs, nil
}

// Append adds a submission and persists. Newest-first on disk too.
func (s *Store) Append(sub Submission) ([]Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	if sub.ID == "" {
		sub.ID = time.Now().Format("20060102-150405.000")
	}
	if sub.Timestamp.IsZero() {
		sub.Timestamp = time.Now()
	}
	subs = append([]Submission{sub}, subs...)
	if err := s.saveLocked(subs); err != nil {
		return nil, err
	}
	return subs, nil
}

// AddJobID records a new job id on an existing submission.
func (s *Store) AddJobID(submissionID, jobID string) ([]Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	for i := range subs {
		if subs[i].ID == submissionID {
			for _, j := range subs[i].JobIDs {
				if j == jobID {
					return subs, nil
				}
			}
			subs[i].JobIDs = append(subs[i].JobIDs, jobID)
			break
		}
	}
	return subs, s.saveLocked(subs)
}

// AttachJobToLatestWithCommand links jobID to the newest submission whose
// Command matches, or to the newest submission if none matches.
func (s *Store) AttachJobToLatestWithCommand(cmd, jobID string) ([]Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	idx := -1
	for i := range subs {
		if subs[i].Command == cmd {
			idx = i
			break
		}
	}
	if idx == -1 && len(subs) > 0 {
		idx = 0
	}
	if idx == -1 {
		return subs, nil
	}
	dup := false
	for _, j := range subs[idx].JobIDs {
		if j == jobID {
			dup = true
		}
	}
	if !dup {
		subs[idx].JobIDs = append(subs[idx].JobIDs, jobID)
	}
	return subs, s.saveLocked(subs)
}

func (s *Store) saveLocked(subs []Submission) error {
	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(s.path, data, 0o644)
}
