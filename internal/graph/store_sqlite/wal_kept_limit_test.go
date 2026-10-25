package store_sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Use the real read-mark refusal and a small WAL; reaching the cleanup path
// must not depend on the production 512MiB file-size heuristic.
func keptWALAfterExternalTruncate(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	previous := walShrinkInPlaceBytes
	walShrinkInPlaceBytes = 1
	t.Cleanup(func() { walShrinkInPlaceBytes = previous })
	s, checkpoint := finalBackfillFixture(t)
	reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Rollback() })
	var payload string
	require.NoError(t, reader.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
	_, err = checkpointWALOnceOn(t.Context(), checkpoint, "PASSIVE")
	require.NoError(t, err)
	s.writeMu.Lock()
	_, err = s.resetWALForPressure(t.Context())
	s.writeMu.Unlock()
	require.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
	require.NotNil(t, s.walLimitKept.Load())
	require.False(t, s.walShrinkPending.Load())
	require.EqualValues(t, -1, activeWriterJournalLimit(t, s))
	require.NoError(t, reader.Rollback())
	// An independent SQLite connection empties the file without the marker
	// write our in-place reset normally uses to signal its pending shrink.
	_, err = checkpointWALOnceOn(t.Context(), checkpoint, "TRUNCATE")
	require.NoError(t, err)
	info, err := os.Stat(s.dbPath + "-wal")
	require.NoError(t, err)
	require.Zero(t, info.Size())
	require.True(t, s.walShrinkNeeded(), "an emptied kept WAL still owes limit restoration")
	return s, checkpoint
}

func TestWALKeptLimitRestoredAfterExternalTruncate(t *testing.T) {
	s, checkpoint := keptWALAfterExternalTruncate(t)
	_, _, err := s.shrinkWAL(t.Context(), checkpoint)
	require.NoError(t, err)
	require.EqualValues(t, walShrinkKeepBytes, activeWriterJournalLimit(t, s))
	require.Nil(t, s.walLimitKept.Load())
	require.False(t, s.walShrinkPending.Load())
	require.False(t, s.walShrinkNeeded())
}

func TestWALKeptLimitRestorationFailureRetainsCleanup(t *testing.T) {
	s, checkpoint := keptWALAfterExternalTruncate(t)
	kept := s.walLimitKept.Load()
	// A closed pinned connection fails through database/sql without changing
	// its underlying pooled connection's pager limit. Cleanup must be retryable.
	closed, err := s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	func() {
		s.writeMu.Lock()
		previous := s.bulkConn
		s.bulkConn = closed
		s.writeMu.Unlock()
		defer func() {
			s.writeMu.Lock()
			s.bulkConn = previous
			s.writeMu.Unlock()
		}()
		_, _, err = s.shrinkWAL(t.Context(), checkpoint)
	}()
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.Same(t, kept, s.walLimitKept.Load(), "the failed setting lost its incarnation receipt")
	require.True(t, s.walShrinkPending.Load())
	require.True(t, s.walShrinkNeeded())
	require.EqualValues(t, -1, activeWriterJournalLimit(t, s))
	_, _, err = s.shrinkWAL(t.Context(), checkpoint)
	require.NoError(t, err)
	require.EqualValues(t, walShrinkKeepBytes, activeWriterJournalLimit(t, s))
	require.Nil(t, s.walLimitKept.Load())
	require.False(t, s.walShrinkPending.Load())
	require.False(t, s.walShrinkNeeded())
}

func TestWALKeptLogUnreadableFileIsNotEmpty(t *testing.T) {
	root := t.TempDir()
	s := &Store{storeCore: &storeCore{dbPath: filepath.Join(root, "store.sqlite")}}
	kept := &walReclaimFrontier{salt: [8]byte{1}}
	walPath := s.dbPath + "-wal"
	// A real symlink-loop error also works when tests run as root (chmod
	// cannot reliably produce EACCES there). Windows may lack symlink rights.
	t.Run("unexpected_stat_error", func(t *testing.T) {
		if err := os.Symlink(filepath.Base(walPath), walPath); err != nil {
			t.Skipf("symlink loop unavailable: %v", err)
		}
		defer func() { require.NoError(t, os.Remove(walPath)) }()
		_, err := os.Stat(walPath)
		require.Error(t, err)
		if os.IsNotExist(err) {
			t.Skip("platform classifies the loop as absence; no unexpected-stat fixture")
		}
		require.False(t, s.walKeptLogRestarted(kept), "a stat error was mistaken for an emptied WAL")
	})
	require.True(t, s.walKeptLogRestarted(kept), "an actually absent WAL has no incarnation to protect")
	require.NoError(t, os.WriteFile(walPath, []byte("unreadable header"), 0o600))
	require.False(t, s.walKeptLogRestarted(kept), "a non-empty, unreadable frontier is not proof of restart")
	require.NoError(t, os.Truncate(walPath, 0))
	require.True(t, s.walKeptLogRestarted(kept), "an actually empty WAL permits restoration")
}
