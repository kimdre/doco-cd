package deploy

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/common/validation"
)

// RetryConfig controls automatic retries of a failed deployment's desired state.
type RetryConfig struct {
	Limit *int              `yaml:"limit,omitempty" json:"limit,omitempty" validate:"omitempty,min=0"` // Limit sets the maximum number of automatic retries after the original attempt. Omitted means unlimited; zero disables automatic retries. Negative values are invalid.
	Delay duration.Duration `yaml:"delay,omitempty" json:"delay,omitempty" validate:"min=0"`           // Delay is the fixed minimum wait after an actual failure before another automatic retry; numeric values are seconds and strings accept Go duration syntax. Omitted or zero means no delay. Negative values are invalid.
}

// Validate checks retry limits and delay values.
func (c RetryConfig) Validate() error {
	if err := validation.Validate(c); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	return nil
}

// Configured reports whether the policy limits retries or adds a delay.
func (c RetryConfig) Configured() bool {
	return c.Limit != nil || c.Delay > 0
}
