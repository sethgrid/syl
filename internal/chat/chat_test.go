package chat_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sethgrid/syl/internal/chat"
	"github.com/sethgrid/syl/internal/db"
)

func TestSQLiteStore(t *testing.T) {
	f, err := os.CreateTemp("", "syl-chat-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() {
		os.Remove(f.Name())
		os.Remove(f.Name() + "-wal")
		os.Remove(f.Name() + "-shm")
	})

	sqlDB, err := db.Open(f.Name())
	require.NoError(t, err)
	defer sqlDB.Close()

	// Insert a dummy agent row so FK constraint passes.
	_, err = sqlDB.Exec(`INSERT INTO agents (fingerprint) VALUES ('fp-test')`)
	require.NoError(t, err)

	store := chat.NewSQLiteStore(sqlDB)

	t.Run("add and retrieve messages", func(t *testing.T) {
		_, err := store.Add(1, "user", "hello")
		require.NoError(t, err)
		_, err = store.Add(1, "assistant", "world")
		require.NoError(t, err)

		msgs, err := store.Recent(1, 10)
		require.NoError(t, err)
		require.Len(t, msgs, 2)
		assert.Equal(t, "user", msgs[0].Role)
		assert.Equal(t, "hello", msgs[0].Content)
		assert.Equal(t, "assistant", msgs[1].Role)
	})

	t.Run("recent respects limit", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			_, _ = store.Add(1, "user", "msg")
		}
		msgs, err := store.Recent(1, 3)
		require.NoError(t, err)
		assert.Len(t, msgs, 3)
	})

	t.Run("LatestSummary returns nil when none exist", func(t *testing.T) {
		// Use a fresh agent.
		_, err := sqlDB.Exec(`INSERT INTO agents (fingerprint) VALUES ('fp-summary')`)
		require.NoError(t, err)
		var agentID int64
		require.NoError(t, sqlDB.QueryRow(`SELECT id FROM agents WHERE fingerprint='fp-summary'`).Scan(&agentID))

		s, err := store.LatestSummary(agentID)
		require.NoError(t, err)
		assert.Nil(t, s)
	})

	t.Run("LatestSummary returns most recent", func(t *testing.T) {
		_, err := sqlDB.Exec(`INSERT INTO agents (fingerprint) VALUES ('fp-sum2')`)
		require.NoError(t, err)
		var agentID int64
		require.NoError(t, sqlDB.QueryRow(`SELECT id FROM agents WHERE fingerprint='fp-sum2'`).Scan(&agentID))

		_, _ = store.Add(agentID, "user", "hi")
		_, _ = store.Add(agentID, "summary", "first summary")
		_, _ = store.Add(agentID, "user", "more")
		_, _ = store.Add(agentID, "summary", "second summary")

		s, err := store.LatestSummary(agentID)
		require.NoError(t, err)
		require.NotNil(t, s)
		assert.Equal(t, "second summary", s.Content)
	})

	t.Run("CountSince excludes summaries", func(t *testing.T) {
		_, err := sqlDB.Exec(`INSERT INTO agents (fingerprint) VALUES ('fp-count')`)
		require.NoError(t, err)
		var agentID int64
		require.NoError(t, sqlDB.QueryRow(`SELECT id FROM agents WHERE fingerprint='fp-count'`).Scan(&agentID))

		m1, _ := store.Add(agentID, "user", "a")
		_, _ = store.Add(agentID, "summary", "sum")
		_, _ = store.Add(agentID, "user", "b")
		_, _ = store.Add(agentID, "assistant", "c")

		count, err := store.CountSince(agentID, m1.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, count) // "b" and "c", not the summary
	})

	t.Run("Since returns non-summary messages after ID", func(t *testing.T) {
		_, err := sqlDB.Exec(`INSERT INTO agents (fingerprint) VALUES ('fp-since')`)
		require.NoError(t, err)
		var agentID int64
		require.NoError(t, sqlDB.QueryRow(`SELECT id FROM agents WHERE fingerprint='fp-since'`).Scan(&agentID))

		m1, _ := store.Add(agentID, "user", "before")
		_, _ = store.Add(agentID, "summary", "ignored summary")
		_, _ = store.Add(agentID, "user", "after1")
		_, _ = store.Add(agentID, "assistant", "after2")

		msgs, err := store.Since(agentID, m1.ID, 100)
		require.NoError(t, err)
		require.Len(t, msgs, 2)
		assert.Equal(t, "after1", msgs[0].Content)
		assert.Equal(t, "after2", msgs[1].Content)
	})
}
