// Package restore loads a snapshot into a target environment and swaps it into place.
package restore

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	stagingPrefix = "restored_"
	backupInfix   = "_backup_"

	// BackupTimestampFormat sorts lexicographically, so the newest backup is the last one in a
	// sorted list. Recovery depends on picking the newest correctly with nothing but the name.
	BackupTimestampFormat = "20060102T150405Z"
)

// StagingName generates the database a restore loads into.
//
// Random rather than sequential so that two restores racing on the same instance cannot pick the
// same name; the advisory lock is what actually prevents the race, this is defence in depth.
func StagingName() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("error generating staging database name: %w", err)
	}
	return stagingPrefix + hex.EncodeToString(b), nil
}

func BackupName(target string, at time.Time) string {
	return target + backupInfix + at.UTC().Format(BackupTimestampFormat)
}

func IsStaging(name string) bool {
	return strings.HasPrefix(name, stagingPrefix)
}

func IsBackupOf(target, name string) bool {
	return strings.HasPrefix(name, target+backupInfix)
}

// DatabaseSet is what the catalog says about a target database and its siblings.
//
// This is the entire persisted state of a restore. There is no journal and no state table: a
// journal can disagree with reality after a crash, and the thing it would be describing -- which
// databases exist -- is already recorded by postgres itself.
type DatabaseSet struct {
	Target       string
	TargetExists bool

	// Backups are previous versions of Target, newest last
	Backups []string

	// Staging are in-flight restore databases
	Staging []string
}

type State string

const (
	// StateIdle is a normal target with nothing in flight
	StateIdle State = "idle"

	// StateOrphanedStaging means a previous run died before swapping. The target was never
	// touched, so the staging database is simply garbage.
	StateOrphanedStaging State = "orphaned_staging"

	// StateCrashedMidSwap means a run died between the two renames, so the target does not
	// exist under its own name. Recovering is renaming the newest backup back.
	StateCrashedMidSwap State = "crashed_mid_swap"

	// StateMissing means the target is gone with no backup to restore it from. Nothing this
	// tool did can produce it, so it refuses to act rather than guessing.
	StateMissing State = "missing"
)

// Classify reduces the catalog to the one fact a restore needs before it starts.
func Classify(s DatabaseSet) State {
	switch {
	case s.TargetExists && len(s.Staging) == 0:
		return StateIdle
	case s.TargetExists:
		return StateOrphanedStaging
	case len(s.Backups) > 0:
		return StateCrashedMidSwap
	default:
		return StateMissing
	}
}

// NewestBackup is the database a crashed swap is recovered from.
func (s DatabaseSet) NewestBackup() string {
	if len(s.Backups) < 1 {
		return ""
	}
	sorted := append([]string(nil), s.Backups...)
	sort.Strings(sorted)
	return sorted[len(sorted)-1]
}

// ExpiredBackups returns the backups to drop so that `keep` of them remain, oldest first.
//
// Called at the *start* of a restore rather than the end of the previous one: a backup is only
// worth dropping once there is a newer database to fall back to, and steady state is then a
// predictable multiple of the database size rather than a slow climb.
func (s DatabaseSet) ExpiredBackups(keep int) []string {
	if keep < 0 {
		keep = 0
	}
	sorted := append([]string(nil), s.Backups...)
	sort.Strings(sorted)
	if len(sorted) <= keep {
		return nil
	}
	return sorted[:len(sorted)-keep]
}

// Describe renders the set for a log line, so a recovery decision is auditable after the fact.
func (s DatabaseSet) Describe() string {
	return fmt.Sprintf("target=%s exists=%t backups=%v staging=%v",
		s.Target, s.TargetExists, s.Backups, s.Staging)
}
