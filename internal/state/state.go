package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const Version = 1

type Store struct {
	path string
	mu   sync.Mutex
	data Data
}

type Data struct {
	Version           int                     `json:"version"`
	Status            Status                  `json:"status"`
	Targets           map[string]TargetRecord `json:"targets"`
	Counters          Counters                `json:"counters"`
	AdminPasswordHash string                  `json:"admin_password_hash,omitempty"`
	SessionSigningKey string                  `json:"session_signing_key,omitempty"`
	RecentEvents      []Event                 `json:"recent_events,omitempty"`
}

type Status struct {
	Stage              string    `json:"stage"`
	Progress           int       `json:"progress"`
	CurrentTarget      string    `json:"current_target,omitempty"`
	Ready              bool      `json:"ready"`
	LastError          string    `json:"last_error,omitempty"`
	LastInitialSyncAt  time.Time `json:"last_initial_sync_at,omitempty"`
	LastScheduledAt    time.Time `json:"last_scheduled_at,omitempty"`
	NextScheduledAt    time.Time `json:"next_scheduled_at,omitempty"`
	LastManualSyncAt   time.Time `json:"last_manual_sync_at,omitempty"`
	LastSuccessfulSync time.Time `json:"last_successful_sync_at,omitempty"`
}

func (s Status) MarshalJSON() ([]byte, error) {
	type statusJSON struct {
		Stage              string     `json:"stage"`
		Progress           int        `json:"progress"`
		CurrentTarget      string     `json:"current_target,omitempty"`
		Ready              bool       `json:"ready"`
		LastError          string     `json:"last_error,omitempty"`
		LastInitialSyncAt  *time.Time `json:"last_initial_sync_at,omitempty"`
		LastScheduledAt    *time.Time `json:"last_scheduled_at,omitempty"`
		NextScheduledAt    *time.Time `json:"next_scheduled_at,omitempty"`
		LastManualSyncAt   *time.Time `json:"last_manual_sync_at,omitempty"`
		LastSuccessfulSync *time.Time `json:"last_successful_sync_at,omitempty"`
	}
	out := statusJSON{
		Stage:         s.Stage,
		Progress:      s.Progress,
		CurrentTarget: s.CurrentTarget,
		Ready:         s.Ready,
		LastError:     s.LastError,
	}
	if !s.LastInitialSyncAt.IsZero() {
		t := s.LastInitialSyncAt
		out.LastInitialSyncAt = &t
	}
	if !s.LastScheduledAt.IsZero() {
		t := s.LastScheduledAt
		out.LastScheduledAt = &t
	}
	if !s.NextScheduledAt.IsZero() {
		t := s.NextScheduledAt
		out.NextScheduledAt = &t
	}
	if !s.LastManualSyncAt.IsZero() {
		t := s.LastManualSyncAt
		out.LastManualSyncAt = &t
	}
	if !s.LastSuccessfulSync.IsZero() {
		t := s.LastSuccessfulSync
		out.LastSuccessfulSync = &t
	}
	return json.Marshal(out)
}

type TargetRecord struct {
	Target     string         `json:"target"`
	AbsPath    string         `json:"abs_path"`
	ObjectKey  string         `json:"object_key"`
	Local      LocalMetadata  `json:"local"`
	Remote     RemoteMetadata `json:"remote"`
	LastAction string         `json:"last_action,omitempty"`
	LastError  string         `json:"last_error,omitempty"`
	UpdatedAt  time.Time      `json:"updated_at,omitempty"`
}

type LocalMetadata struct {
	Exists  bool      `json:"exists"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
}

type RemoteMetadata struct {
	Exists       bool      `json:"exists"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified time.Time `json:"last_modified,omitempty"`
}

type Counters struct {
	Month           string `json:"month"`
	ClassA          int64  `json:"class_a"`
	ClassB          int64  `json:"class_b"`
	FreeOps         int64  `json:"free_ops"`
	UploadedBytes   int64  `json:"uploaded_bytes"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
}

type Event struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Target  string    `json:"target,omitempty"`
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Snapshot() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneData(s.data)
}

func (s *Store) Update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.data); err != nil {
		return err
	}
	ensureDefaults(&s.data)
	return s.saveLocked()
}

func (s *Store) SetStatus(status Status) error {
	return s.Update(func(d *Data) error {
		d.Status = status
		return nil
	})
}

func (s *Store) AddEvent(level, msg, target string) error {
	return s.Update(func(d *Data) error {
		d.RecentEvents = append(d.RecentEvents, Event{
			Time:    time.Now().UTC(),
			Level:   level,
			Message: msg,
			Target:  target,
		})
		if len(d.RecentEvents) > 100 {
			d.RecentEvents = d.RecentEvents[len(d.RecentEvents)-100:]
		}
		return nil
	})
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.data = Data{}
			ensureDefaults(&s.data)
			return nil
		}
		return fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return fmt.Errorf("decode state file: %w", err)
	}
	ensureDefaults(&s.data)
	return nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state temp file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func ensureDefaults(d *Data) {
	if d.Version == 0 {
		d.Version = Version
	}
	if d.Targets == nil {
		d.Targets = map[string]TargetRecord{}
	}
	nowMonth := time.Now().UTC().Format("2006-01")
	if d.Counters.Month == "" {
		d.Counters.Month = nowMonth
	}
	if d.Counters.Month != nowMonth {
		d.Counters = Counters{Month: nowMonth}
	}
}

func cloneData(in Data) Data {
	out := in
	out.Targets = make(map[string]TargetRecord, len(in.Targets))
	for key, value := range in.Targets {
		out.Targets[key] = value
	}
	out.RecentEvents = append([]Event(nil), in.RecentEvents...)
	return out
}
