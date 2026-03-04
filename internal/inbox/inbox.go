package inbox

import (
	"database/sql"
	"fmt"
	"time"
)

type Item struct {
	ID        int64
	Question  string
	Answer    sql.NullString
	Status    string // "open", "answered"
	CreatedAt time.Time
}

// Store is the interface for inbox persistence. Inbox is global (not per-agent).
type Store interface {
	Add(question string) (*Item, error)
	List() ([]Item, error)
	ListOpen() ([]Item, error)
	Answer(id int64, answer string) error
}

// SQLiteStore implements Store against SQLite.
type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

func (s *SQLiteStore) Add(question string) (*Item, error) {
	res, err := s.db.Exec(`INSERT INTO inbox_items (question) VALUES (?)`, question)
	if err != nil {
		return nil, fmt.Errorf("insert inbox item: %w", err)
	}
	id, _ := res.LastInsertId()
	return &Item{ID: id, Question: question, Status: "open", CreatedAt: time.Now()}, nil
}

func (s *SQLiteStore) List() ([]Item, error) {
	return s.query(`SELECT id, question, answer, status, created_at FROM inbox_items ORDER BY id DESC`)
}

func (s *SQLiteStore) ListOpen() ([]Item, error) {
	return s.query(`SELECT id, question, answer, status, created_at FROM inbox_items WHERE status = 'open' ORDER BY id DESC`)
}

func (s *SQLiteStore) query(q string, args ...any) ([]Item, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query inbox: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Question, &it.Answer, &it.Status, &it.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

func (s *SQLiteStore) Answer(id int64, answer string) error {
	_, err := s.db.Exec(
		`UPDATE inbox_items SET answer = ?, status = 'answered' WHERE id = ?`, answer, id)
	return err
}
