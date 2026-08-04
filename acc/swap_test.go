package acc

import (
	"fmt"
	"testing"
	"time"

	"github.com/nullstone-io/pg-snapshot/restore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The swap is the one part of a restore that can leave an environment broken, and its recovery
// reads state out of the catalog. Both halves are only really tested against real databases.
func TestSwapAndRecover(t *testing.T) {
	pool, ctx := Connect(t)
	admin := restore.Admin{DB: pool}

	target := fmt.Sprintf("pgsnap_acc_%d", time.Now().UnixNano()%1e6)

	cleanup := func() {
		set, err := admin.Inspect(ctx, target)
		if err != nil {
			return
		}
		for _, name := range append(append([]string{}, set.Backups...), set.Staging...) {
			admin.DropDatabase(ctx, name)
		}
		admin.DropDatabase(ctx, target)
	}
	cleanup()
	t.Cleanup(cleanup)

	require.NoError(t, admin.CreateDatabase(ctx, target, ""))

	t.Run("idle", func(t *testing.T) {
		set, err := admin.Inspect(ctx, target)
		require.NoError(t, err)
		assert.True(t, set.TargetExists)
		assert.Equal(t, restore.StateIdle, restore.Classify(set))
	})

	staging, err := restore.StagingName()
	require.NoError(t, err)
	require.NoError(t, admin.CreateDatabase(ctx, staging, ""))

	t.Run("orphaned staging is seen", func(t *testing.T) {
		set, err := admin.Inspect(ctx, target)
		require.NoError(t, err)
		assert.Equal(t, restore.StateOrphanedStaging, restore.Classify(set))
		assert.Contains(t, set.Staging, staging)
	})

	// Mark the staging database so the swap can be shown to have actually replaced the target
	stagingURL, err := withDatabase(URL(), staging)
	require.NoError(t, err)
	markDatabase(t, ctx, stagingURL, "from-staging")

	var backup string
	t.Run("swap", func(t *testing.T) {
		backup, err = admin.Swap(ctx, target, staging)
		require.NoError(t, err)
		assert.True(t, restore.IsBackupOf(target, backup))

		set, err := admin.Inspect(ctx, target)
		require.NoError(t, err)
		assert.True(t, set.TargetExists)
		assert.Contains(t, set.Backups, backup)
		assert.Empty(t, set.Staging)

		targetURL, err := withDatabase(URL(), target)
		require.NoError(t, err)
		assert.Equal(t, "from-staging", readMark(t, ctx, targetURL),
			"the target should now hold what the staging database held")
	})

	t.Run("connections are reopened after the swap", func(t *testing.T) {
		var allowed bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT datallowconn FROM pg_database WHERE datname = $1`, target).Scan(&allowed))
		assert.True(t, allowed, "leaving allow_connections false would break the environment")
	})

	// Simulate a process killed between the two renames: the target is gone under its own name.
	// The name is suffixed because the swap above already took this second's backup name -- the
	// same collision Swap disambiguates internally.
	t.Run("recovers from a crash mid-swap", func(t *testing.T) {
		crashed := restore.BackupName(target, time.Now()) + "_99"
		require.NoError(t, admin.RenameDatabase(ctx, target, crashed))

		set, err := admin.Inspect(ctx, target)
		require.NoError(t, err)
		require.Equal(t, restore.StateCrashedMidSwap, restore.Classify(set))

		require.NoError(t, admin.Recover(ctx, target))

		set, err = admin.Inspect(ctx, target)
		require.NoError(t, err)
		assert.True(t, set.TargetExists, "recovery should put the target back")
		assert.Equal(t, restore.StateIdle, restore.Classify(set))
	})

	t.Run("expired backups are dropped", func(t *testing.T) {
		set, err := admin.Inspect(ctx, target)
		require.NoError(t, err)

		for _, name := range set.ExpiredBackups(0) {
			require.NoError(t, admin.DropDatabase(ctx, name))
		}

		set, err = admin.Inspect(ctx, target)
		require.NoError(t, err)
		assert.Empty(t, set.Backups)
	})
}

// A target that does not exist and has no backup is not a state a restore can produce, so the
// tool must refuse rather than invent one.
func TestRecoverRefusesUnknownState(t *testing.T) {
	pool, ctx := Connect(t)
	admin := restore.Admin{DB: pool}

	err := admin.Recover(ctx, fmt.Sprintf("pgsnap_missing_%d", time.Now().UnixNano()%1e6))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing has been changed")
}
