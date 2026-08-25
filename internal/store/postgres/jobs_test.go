package postgres

import (
	"errors"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestValidateExecutionFeatures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		backend  string
		features domain.ExecutionFeatures
		wantErr  bool
	}{
		{
			name:     "subprocess direct native slice",
			backend:  "subprocess",
			features: domain.ExecutionFeatures{DirectCommand: true, RetryMaxRuns: 1},
		},
		{
			name:    "Slurm resources",
			backend: "slurm",
			features: domain.ExecutionFeatures{
				DirectCommand: true, Resources: true, RetryMaxRuns: 1,
			},
		},
		{
			name:    "subprocess resources",
			backend: "subprocess",
			features: domain.ExecutionFeatures{
				DirectCommand: true, Resources: true, RetryMaxRuns: 1,
			},
			wantErr: true,
		},
		{
			name:    "Slurm temporary storage",
			backend: "slurm",
			features: domain.ExecutionFeatures{
				DirectCommand: true, Resources: true, TemporaryStorage: true, RetryMaxRuns: 1,
			},
			wantErr: true,
		},
		{
			name:    "Slurm scheduler environment override",
			backend: "slurm",
			features: domain.ExecutionFeatures{
				DirectCommand: true, RetryMaxRuns: 1, SchedulerEnvironmentOverride: true,
			},
			wantErr: true,
		},
		{
			name:    "unsupported shared feature",
			backend: "slurm",
			features: domain.ExecutionFeatures{
				DirectCommand: true, RetryMaxRuns: 1, Extensions: true,
			},
			wantErr: true,
		},
		{
			name:     "unknown backend",
			backend:  "future",
			features: domain.ExecutionFeatures{DirectCommand: true, RetryMaxRuns: 1},
			wantErr:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateExecutionFeatures(test.backend, test.features)
			if test.wantErr && !errors.Is(err, domain.ErrInvalidPlacement) {
				t.Fatalf("validateExecutionFeatures() error = %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateExecutionFeatures() error = %v", err)
			}
		})
	}
}
