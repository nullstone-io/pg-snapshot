package pg

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Phases narrates a run as a numbered sequence.
//
// A restore is a long chain of steps against a database nobody can watch directly, and "it is
// still going" is a question the logs have to answer on their own. Each phase logs when it starts
// and when it ends, with how long it took, so a stall is visible as a phase that began and never
// finished rather than as silence.
type Phases struct {
	Log *slog.Logger

	// Total is how many phases this run will execute, which the caller works out from its own
	// options -- a restore with no migrate command runs one fewer than one with.
	//
	// Zero means unknown, and the phase is then reported without a denominator rather than against
	// a total that would be a guess.
	Total int

	n int
}

func (p *Phases) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// Run executes one phase, logging its start and end.
//
// The error is returned untouched: a phase is a narration wrapper, not an error handler, and
// wrapping here would bury the message the caller wrote for the operator.
func (p *Phases) Run(ctx context.Context, name string, fn func() error, attrs ...any) error {
	done := p.Start(name, attrs...)
	err := fn()
	done(err)
	return err
}

// Start opens a phase and returns the function that closes it. Use it directly when the phase's
// body cannot be expressed as a single closure; prefer Run otherwise.
func (p *Phases) Start(name string, attrs ...any) func(error) {
	p.n++
	position := p.position(p.n)
	began := time.Now()

	p.log().Info("phase started", append([]any{"phase", position, "name", name}, attrs...)...)

	return func(err error) {
		fields := []any{"phase", position, "name", name,
			"elapsed", time.Since(began).Round(time.Millisecond).String()}
		if err != nil {
			p.log().Error("phase failed", append(fields, "error", err)...)
			return
		}
		p.log().Info("phase complete", fields...)
	}
}

// position renders "4 of 11", or just "4" when the total is unknown.
//
// One field rather than two so it reads as a sentence in a text log and stays a single value in a
// structured one. A run that overshoots its declared total reports the honest count rather than a
// nonsense "12 of 11": the total was wrong, and hiding that helps nobody.
func (p *Phases) position(n int) string {
	if p.Total < 1 || n > p.Total {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%d of %d", n, p.Total)
}
