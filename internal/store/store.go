package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type CheckHistory struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"` // "scheduled" | "manual"
	StartedAt time.Time `json:"started_at"`
	DurationMs int64    `json:"duration_ms"`
	Total     int       `json:"total"`
	Updated   int       `json:"updated"`
	Failed    int       `json:"failed"`
	UpToDate  int       `json:"up_to_date"`
	Details   string    `json:"details"`
}

type ContainerState struct {
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	LocalHash  string    `json:"local_hash"`
	RemoteHash string    `json:"remote_hash"`
	Status     string    `json:"status"` // "up_to_date" | "update_available" | "failed" | "unknown"
	LastCheck  time.Time `json:"last_check"`
	Error      string    `json:"error,omitempty"`
}

type Store struct {
	db *sql.DB
	mu sync.RWMutex

	containerStates map[string]*ContainerState
	lastCheckResult *CheckHistory
}

func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	s := &Store{
		db:              db,
		containerStates: make(map[string]*ContainerState),
	}

	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS check_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			type TEXT NOT NULL DEFAULT 'scheduled',
			started_at DATETIME NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			total INTEGER NOT NULL DEFAULT 0,
			updated INTEGER NOT NULL DEFAULT 0,
			failed INTEGER NOT NULL DEFAULT 0,
			up_to_date INTEGER NOT NULL DEFAULT 0,
			details TEXT NOT NULL DEFAULT '{}'
		);
		CREATE INDEX IF NOT EXISTS idx_check_history_started_at ON check_history(started_at DESC);
	`)
	return err
}

func (s *Store) SaveCheckHistory(h *CheckHistory) error {
	result, err := s.db.Exec(
		`INSERT INTO check_history (type, started_at, duration_ms, total, updated, failed, up_to_date, details)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		h.Type, h.StartedAt, h.DurationMs, h.Total, h.Updated, h.Failed, h.UpToDate, h.Details,
	)
	if err != nil {
		return err
	}
	h.ID, _ = result.LastInsertId()

	s.mu.Lock()
	s.lastCheckResult = h
	s.mu.Unlock()

	return nil
}

func (s *Store) GetCheckHistory(limit, offset int) ([]CheckHistory, error) {
	rows, err := s.db.Query(
		`SELECT id, type, started_at, duration_ms, total, updated, failed, up_to_date, details
		 FROM check_history ORDER BY started_at DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []CheckHistory
	for rows.Next() {
		var h CheckHistory
		if err := rows.Scan(&h.ID, &h.Type, &h.StartedAt, &h.DurationMs, &h.Total, &h.Updated, &h.Failed, &h.UpToDate, &h.Details); err != nil {
			return nil, err
		}
		results = append(results, h)
	}
	return results, nil
}

func (s *Store) GetCheckHistoryByID(id int64) (*CheckHistory, error) {
	var h CheckHistory
	err := s.db.QueryRow(
		`SELECT id, type, started_at, duration_ms, total, updated, failed, up_to_date, details
		 FROM check_history WHERE id = ?`, id,
	).Scan(&h.ID, &h.Type, &h.StartedAt, &h.DurationMs, &h.Total, &h.Updated, &h.Failed, &h.UpToDate, &h.Details)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *Store) UpdateContainerStates(states []*ContainerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range states {
		s.containerStates[state.Name] = state
	}
}

func (s *Store) GetContainerStates() []*ContainerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ContainerState, 0, len(s.containerStates))
	for _, state := range s.containerStates {
		result = append(result, state)
	}
	return result
}

func (s *Store) GetLastCheckResult() *CheckHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastCheckResult
}

func (s *Store) GetHistoryCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM check_history`).Scan(&count)
	return count, err
}

func MarshalDetails(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func (s *Store) Close() error {
	return s.db.Close()
}
