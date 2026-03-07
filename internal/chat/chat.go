package chat

import (
	"database/sql"
	"fmt"
	"time"
)

type Message struct {
	ID        int64
	AgentID   int64
	Role      string // "user", "assistant", "summary"
	Content   string
	CreatedAt time.Time
}

// Store is the interface for message persistence.
type Store interface {
	Add(agentID int64, role, content string) (*Message, error)
	Recent(agentID int64, limit int) ([]Message, error)
	History(agentID int64, tokenBudget, maxMsgs int) ([]Message, error)
}

// SQLiteStore implements Store against SQLite.
type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

func (s *SQLiteStore) Add(agentID int64, role, content string) (*Message, error) {
	res, err := s.db.Exec(
		`INSERT INTO messages (agent_id, role, content) VALUES (?, ?, ?)`,
		agentID, role, content)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	id, _ := res.LastInsertId()
	return &Message{ID: id, AgentID: agentID, Role: role, Content: content, CreatedAt: time.Now()}, nil
}

func (s *SQLiteStore) Recent(agentID int64, limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, agent_id, role, content, created_at FROM messages
		 WHERE agent_id = ? ORDER BY id DESC LIMIT ?`,
		agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	// reverse to chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// History returns messages for agentID, newest-first, stopping once the
// estimated token count exceeds tokenBudget or maxMsgs is reached.
// Result is returned in chronological order.
func (s *SQLiteStore) History(agentID int64, tokenBudget, maxMsgs int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, agent_id, role, content, created_at FROM messages
		 WHERE agent_id = ? ORDER BY id DESC LIMIT ?`,
		agentID, maxMsgs)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	tokensUsed := 0
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		tokensUsed += len(m.Content) / 4
		if tokensUsed > tokenBudget {
			break
		}
		msgs = append(msgs, m)
	}
	// reverse to chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}
