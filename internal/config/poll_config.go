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
	Url      HttpUrl `yaml:"url" validate:"httpUrl"` // Url is the URL to the Git repository to poll for changes
	Branch   string  `yaml:"branch" default:"main"`  // Branch is the branch to poll for changes
	Interval int     `yaml:"interval" default:"180"` // IntervalSeconds is the interval in seconds to poll for changes
}

type PollInstance struct {
	Config   *PollConfig  // config is the PollConfig for this instance
	NextRun  int64        // NextRun is the next time this instance should run
	LastRun  int64        // LastRun is the last time this instance ran
	PollFunc func() error // PollFunc is the function to call to poll the repository for changes
}

// Validate checks if the PollConfig is valid
func (c *PollConfig) Validate() error {
	if c.Url == "" {
		return fmt.Errorf("%w: url", ErrKeyNotFound)
	}

	if c.Branch == "" {
		return fmt.Errorf("%w: branch", ErrKeyNotFound)
	}

	if c.Interval == 0 {
		return fmt.Errorf("%w: interval", ErrKeyNotFound)
	}

	return nil
}

// String returns a string representation of the PollConfig
func (c *PollConfig) String() string {
	return fmt.Sprintf("PollConfig{Url: %s, Branch: %s, Interval: %d}", c.Url, c.Branch, c.Interval)
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
