package pg

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhasesNumberAndPair(t *testing.T) {
	var buf bytes.Buffer
	phases := &Phases{Log: slog.New(slog.NewTextHandler(&buf, nil)), Total: 2}
	ctx := context.Background()

	require.NoError(t, phases.Run(ctx, "load data", func() error { return nil }, "tables", 3))
	require.NoError(t, phases.Run(ctx, "swap", func() error { return nil }))

	lines := nonEmptyLines(buf.String())
	require.Len(t, lines, 4, "every phase logs exactly one start and one end")

	assert.Contains(t, lines[0], `msg="phase started" phase="1 of 2" name="load data" tables=3`)
	assert.Contains(t, lines[1], `msg="phase complete" phase="1 of 2" name="load data"`)
	assert.Contains(t, lines[1], "elapsed=")
	assert.Contains(t, lines[2], `phase="2 of 2" name=swap`)
}

// Without a total there is no denominator to report, and inventing one would be a guess.
func TestPhasesWithoutTotal(t *testing.T) {
	var buf bytes.Buffer
	phases := &Phases{Log: slog.New(slog.NewTextHandler(&buf, nil))}

	require.NoError(t, phases.Run(context.Background(), "swap", func() error { return nil }))
	assert.Contains(t, buf.String(), `phase=1 name=swap`)
	assert.NotContains(t, buf.String(), " of ")
}

// A caller whose arithmetic is wrong gets the honest count rather than "3 of 2".
func TestPhasesOvershootingTheTotal(t *testing.T) {
	var buf bytes.Buffer
	phases := &Phases{Log: slog.New(slog.NewTextHandler(&buf, nil)), Total: 2}
	ctx := context.Background()

	for _, name := range []string{"one", "two", "three"} {
		require.NoError(t, phases.Run(ctx, name, func() error { return nil }))
	}

	assert.Contains(t, buf.String(), `phase="2 of 2" name=two`)
	assert.Contains(t, buf.String(), `phase=3 name=three`)
	assert.NotContains(t, buf.String(), `"3 of 2"`)
}

// A failed phase still closes, so a stall is distinguishable from a failure: silence after a start
// means stuck, "phase failed" means finished badly.
func TestPhasesReportFailure(t *testing.T) {
	var buf bytes.Buffer
	phases := &Phases{Log: slog.New(slog.NewTextHandler(&buf, nil))}

	err := phases.Run(context.Background(), "migrate", func() error {
		return errors.New("migration 309 failed")
	})

	require.Error(t, err)
	assert.EqualError(t, err, "migration 309 failed", "the phase must not wrap the caller's error")

	lines := nonEmptyLines(buf.String())
	require.Len(t, lines, 2)
	assert.Contains(t, lines[1], `level=ERROR msg="phase failed" phase=1 name=migrate`)
	assert.Contains(t, lines[1], "migration 309 failed")
}

func nonEmptyLines(s string) []string {
	out := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
