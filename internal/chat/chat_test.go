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
			store.Add(1, "user", "msg")
		}
		msgs, err := store.Recent(1, 3)
		require.NoError(t, err)
		assert.Len(t, msgs, 3)
	})
}
