package restore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		set  DatabaseSet
		want State
	}{
		{
			name: "idle",
			set:  DatabaseSet{Target: "core", TargetExists: true},
			want: StateIdle,
		},
		{
			// The target was never touched, so the staging database is simply garbage
			name: "crashed during prepare",
			set:  DatabaseSet{Target: "core", TargetExists: true, Staging: []string{"restored_a1b2c3d4"}},
			want: StateOrphanedStaging,
		},
		{
			name: "crashed during prepare with an older backup present",
			set: DatabaseSet{
				Target: "core", TargetExists: true,
				Backups: []string{"core_backup_20260804T120000Z"},
				Staging: []string{"restored_a1b2c3d4"},
			},
			want: StateOrphanedStaging,
		},
		{
			// The dangerous window: renamed away, never renamed back
			name: "crashed between the two renames",
			set: DatabaseSet{
				Target: "core", TargetExists: false,
				Backups: []string{"core_backup_20260804T120000Z"},
				Staging: []string{"restored_a1b2c3d4"},
			},
			want: StateCrashedMidSwap,
		},
		{
			name: "crashed after the staging database was renamed away",
			set: DatabaseSet{
				Target: "core", TargetExists: false,
				Backups: []string{"core_backup_20260804T120000Z"},
			},
			want: StateCrashedMidSwap,
		},
		{
			// Nothing this tool does can produce it, so it refuses rather than guessing
			name: "target gone with nothing to recover from",
			set:  DatabaseSet{Target: "core", TargetExists: false},
			want: StateMissing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Classify(tt.set))
		})
	}
}

func TestNewestBackup(t *testing.T) {
	// Recovery has nothing but the names to go on, so the timestamp format has to sort correctly
	set := DatabaseSet{Target: "core", Backups: []string{
		"core_backup_20260804T120000Z",
		"core_backup_20261115T093000Z",
		"core_backup_20260804T235959Z",
	}}
	assert.Equal(t, "core_backup_20261115T093000Z", set.NewestBackup())

	assert.Empty(t, DatabaseSet{Target: "core"}.NewestBackup())
}

func TestExpiredBackups(t *testing.T) {
	set := DatabaseSet{Target: "core", Backups: []string{
		"core_backup_20260803T120000Z",
		"core_backup_20260805T120000Z",
		"core_backup_20260804T120000Z",
	}}

	t.Run("keeps the newest", func(t *testing.T) {
		assert.Equal(t, []string{
			"core_backup_20260803T120000Z",
			"core_backup_20260804T120000Z",
		}, set.ExpiredBackups(1))
	})

	t.Run("keeps more when asked", func(t *testing.T) {
		assert.Equal(t, []string{"core_backup_20260803T120000Z"}, set.ExpiredBackups(2))
	})

	t.Run("keeps none", func(t *testing.T) {
		assert.Len(t, set.ExpiredBackups(0), 3)
	})

	t.Run("nothing to expire", func(t *testing.T) {
		assert.Nil(t, set.ExpiredBackups(5))
		assert.Nil(t, DatabaseSet{Target: "core"}.ExpiredBackups(1))
	})

	t.Run("negative retention is treated as zero", func(t *testing.T) {
		assert.Len(t, set.ExpiredBackups(-1), 3)
	})
}

func TestNames(t *testing.T) {
	t.Run("staging names are unique", func(t *testing.T) {
		a, err := StagingName()
		require.NoError(t, err)
		b, err := StagingName()
		require.NoError(t, err)

		assert.True(t, IsStaging(a))
		assert.NotEqual(t, a, b)
	})

	t.Run("backup names belong to their target", func(t *testing.T) {
		at := time.Date(2026, 8, 4, 15, 30, 0, 0, time.UTC)
		name := BackupName("core", at)

		assert.Equal(t, "core_backup_20260804T153000Z", name)
		assert.True(t, IsBackupOf("core", name))
		assert.False(t, IsBackupOf("other", name))
	})

	// A database named core_backup_* must not be mistaken for a staging database, and vice versa
	t.Run("staging and backup names do not overlap", func(t *testing.T) {
		backup := BackupName("core", time.Now())
		assert.False(t, IsStaging(backup))

		staging, err := StagingName()
		require.NoError(t, err)
		assert.False(t, IsBackupOf("core", staging))
	})

	// "restored" is a plausible database name for a human to pick; it must not look like ours
	t.Run("unrelated databases are ignored", func(t *testing.T) {
		assert.False(t, IsBackupOf("core", "core_snapshot_2026"))
		assert.False(t, IsBackupOf("core", "coreish_backup_20260804T153000Z"))
	})
}
