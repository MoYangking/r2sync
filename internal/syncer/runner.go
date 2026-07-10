package syncer

import (
	"context"

	"r2sync/internal/config"
)

// Runner is one selectable sync implementation (R2 object storage, GitHub
// repository, ...). All implementations share the process-wide sync gate and
// the same state.Store conventions, so the UI, the /api/ready gate and the
// scheduler treat them uniformly.
type Runner interface {
	Sync(ctx context.Context, mode Mode) (Result, error)
	Verify(ctx context.Context) (Result, error)
	DeleteRemote(ctx context.Context, target, confirm string) error
	// Check validates connectivity/credentials without transferring data.
	Check(ctx context.Context) error
}

// RunnerFactory builds the Runner for the given configuration, validating
// method-specific credentials in the process.
type RunnerFactory func(ctx context.Context, cfg config.Config) (Runner, error)
