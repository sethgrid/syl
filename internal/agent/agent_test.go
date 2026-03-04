package agent_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sethgrid/syl/internal/agent"
	"github.com/sethgrid/syl/internal/db"
)

func TestSQLiteStore(t *testing.T) {
	f, err := os.CreateTemp("", "syl-agent-*.db")
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

	store := agent.NewSQLiteStore(sqlDB)

	t.Run("creates agent on first fingerprint", func(t *testing.T) {
		a, err := store.Resolve("", "fp-abc")
		require.NoError(t, err)
		assert.NotZero(t, a.ID)
		assert.Equal(t, "fp-abc", a.Fingerprint)
		assert.False(t, a.Name.Valid)
	})

	t.Run("returns same agent for same fingerprint", func(t *testing.T) {
		a1, _ := store.Resolve("", "fp-xyz")
		a2, _ := store.Resolve("", "fp-xyz")
		assert.Equal(t, a1.ID, a2.ID)
	})

	t.Run("resolves by name first", func(t *testing.T) {
		_, err := store.Resolve("syl", "fp-named")
		require.NoError(t, err)
		// different fingerprint, same name → same agent
		got, err := store.Resolve("syl", "fp-other")
		require.NoError(t, err)
		assert.Equal(t, "syl", got.Name.String)
	})

	t.Run("UpdateSoul persists", func(t *testing.T) {
		a, _ := store.Resolve("", "fp-soul")
		err := store.UpdateSoul(a.ID, "curious and warm")
		require.NoError(t, err)
		got, err := store.Get(a.ID)
		require.NoError(t, err)
		assert.Equal(t, "curious and warm", got.Soul)
	})
}
