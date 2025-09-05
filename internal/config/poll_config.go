package config

import (
	"errors"
	"fmt"

	"github.com/creasty/defaults"
)

var (
	ErrInvalidPollConfig = errors.New("invalid poll configuration")
	ErrBothPollConfigSet = errors.New("both POLL_CONFIG and POLL_CONFIG_FILE are set, please use one or the other")
)

type PollConfig struct {
	CloneUrl     HttpUrl `yaml:"url" validate:"httpUrl"`              // CloneUrl is the URL to clone the Git repository that is used to poll for changes
	Reference    string  `yaml:"reference" default:"refs/heads/main"` // Reference is the Git reference to the deployment, e.g., refs/heads/main, main, refs/tags/v1.0.0 or v1.0.0
	Interval     int     `yaml:"interval" default:"180"`              // Interval is the interval in seconds to poll for changes
	CustomTarget string  `yaml:"target" default:""`                   // CustomTarget is the name of an optional custom deployment config file, e.g. ".doco-cd.custom-name.yaml"
}

type PollJob struct {
	Config  PollConfig // config is the PollConfig for this instance
	LastRun int64      // LastRun is the last time this instance ran
	NextRun int64      // NextRun is the next time this instance should run
}

// Validate checks if the PollConfig is valid.
func (c *PollConfig) Validate() error {
	if c.CloneUrl == "" {
		return fmt.Errorf("%w: url", ErrKeyNotFound)
	}

	if c.Reference == "" {
		return fmt.Errorf("%w: reference", ErrKeyNotFound)
	}

	if c.Interval < 10 && c.Interval != 0 {
		return fmt.Errorf("%w: interval must be at least 10 seconds", ErrInvalidPollConfig)
	}

	return nil
}

// String returns a string representation of the PollConfig.
func (c *PollConfig) String() string {
	return fmt.Sprintf("PollConfig{CloneUrl: %s, Reference: %s, Interval: %d}", c.CloneUrl, c.Reference, c.Interval)
}

func (c *PollConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	type Plain PollConfig

	if err := unmarshal((*Plain)(c)); err != nil {
		return err
	}

	return nil
}
