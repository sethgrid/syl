package agent

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Agent struct {
	ID          int64
	Name        sql.NullString
	Fingerprint string
	Soul        string
	CreatedAt   time.Time
}

// Store is the interface for agent persistence.
type Store interface {
	Resolve(name, fingerprint string) (*Agent, error)
	Get(agentID int64) (*Agent, error)
	UpdateSoul(agentID int64, soul string) error
}

// SQLiteStore implements Store against SQLite.
type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

// Resolve returns agent by name first, then fingerprint, creating if not found.
func (s *SQLiteStore) Resolve(name, fingerprint string) (*Agent, error) {
	if name != "" {
		a, err := s.getByName(name)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get by name: %w", err)
		}
	}
	a, err := s.getByFingerprint(fingerprint)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get by fingerprint: %w", err)
	}
	return s.create(name, fingerprint)
}

func (s *SQLiteStore) getByName(name string) (*Agent, error) {
	row := s.db.QueryRow(
		`SELECT id, name, fingerprint, soul, created_at FROM agents WHERE name = ?`, name)
	return scanAgent(row)
}

func (s *SQLiteStore) getByFingerprint(fp string) (*Agent, error) {
	row := s.db.QueryRow(
		`SELECT id, name, fingerprint, soul, created_at FROM agents WHERE fingerprint = ?`, fp)
	return scanAgent(row)
}

func (s *SQLiteStore) create(name, fingerprint string) (*Agent, error) {
	var nameVal sql.NullString
	if name != "" {
		nameVal = sql.NullString{String: name, Valid: true}
	}
	res, err := s.db.Exec(
		`INSERT INTO agents (name, fingerprint) VALUES (?, ?)`, nameVal, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("insert agent: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.Get(id)
}

func (s *SQLiteStore) Get(agentID int64) (*Agent, error) {
	row := s.db.QueryRow(
		`SELECT id, name, fingerprint, soul, created_at FROM agents WHERE id = ?`, agentID)
	return scanAgent(row)
}

func (s *SQLiteStore) UpdateSoul(agentID int64, soul string) error {
	_, err := s.db.Exec(`UPDATE agents SET soul = ? WHERE id = ?`, soul, agentID)
	return err
}

func scanAgent(row *sql.Row) (*Agent, error) {
	var a Agent
	if err := row.Scan(&a.ID, &a.Name, &a.Fingerprint, &a.Soul, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}
