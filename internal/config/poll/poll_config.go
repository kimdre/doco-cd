package poll

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/creasty/defaults"
	"gopkg.in/validator.v2"

	"github.com/kimdre/doco-cd/internal/config"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/logger"
)

type Config struct {
	Source       config.SourceType `yaml:"source" json:"source" default:"git"`                   // Source selects the poll source backend (git or oci)
	SourceUrl    string            `yaml:"url" json:"url"`                                       // SourceUrl is the repository/artifact URL; validated as GitUrl or OciUrl depending on Source
	Reference    string            `yaml:"reference" json:"reference" default:"refs/heads/main"` // Reference is the Git reference to the deployment, e.g., refs/heads/main, main, refs/tags/v1.0.0 or v1.0.0
	Interval     int               `yaml:"interval" default:"180"`                               // Interval is the interval in seconds to poll for changes
	CustomTarget string            `yaml:"target" json:"target" default:""`                      // CustomTarget is the name of an optional custom deployment config file, e.g. ".doco-cd.custom-name.yaml"
	RunOnce      bool              `yaml:"run_once" default:"false"`                             // RunOnce when true, performs a single run and exits
	Deployments  []*deploy.Config  `yaml:"deployments" json:"deployments" default:"[]"`          // Deployments allows defining deployment configs inline in the poll configuration
}

type Job struct {
	Config  Config // config is the Config for this instance
	LastRun int64  // LastRun is the last time this instance ran
	NextRun int64  // NextRun is the next time this instance should run
}

const MinPollInterval = 10 // Minimum allowed poll interval in seconds

var (
	ErrInvalidConfig  = errors.New("invalid poll configuration")
	ErrBothConfigSet  = errors.New("both POLL_CONFIG and POLL_CONFIG_FILE are set, please use one or the other")
	ErrIntervalTooLow = errors.New("poll interval too low")
)

// LogValue implements the slog.LogValuer interface for Config.
func (c *Config) LogValue() slog.Value {
	return logger.BuildLogValue(c, "Deployments.Internal")
}

// Validate checks if the Config is valid.
func (c *Config) Validate() error {
	c.Source = config.NormalizeSourceType(c.Source)

	if err := config.ValidateSourceType(c.Source); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	switch c.Source {
	case config.SourceTypeGit:
		if c.SourceUrl == "" {
			return fmt.Errorf("%w: url", deploy.ErrKeyNotFound)
		}

		if c.Reference == "" {
			return fmt.Errorf("%w: reference", deploy.ErrKeyNotFound)
		}

		if err := config.GitUrl(c.SourceUrl).Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}

	case config.SourceTypeOCI:
		if c.SourceUrl == "" {
			return fmt.Errorf("%w: url", deploy.ErrKeyNotFound)
		}

		ociUrl := config.OciUrl(c.SourceUrl)
		if err := ociUrl.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}

		// Derive reference from the artifact tag so users don't need to specify it separately.
		if ref := ociUrl.Tag(); ref != "" {
			c.Reference = ref
		}
	}

	if c.Interval < MinPollInterval && c.Interval != 0 {
		return fmt.Errorf("%w: must be at least %d seconds", ErrIntervalTooLow, MinPollInterval)
	}

	// If inline deployments are defined, validate them
	if len(c.Deployments) > 0 {
		for _, d := range c.Deployments {
			if err := defaults.Set(d); err != nil {
				return err
			}

			if d.Reference == "" {
				d.Reference = c.Reference
			}

			if err := d.Validate(); err != nil {
				return fmt.Errorf("%w: %v", deploy.ErrInvalidConfig, err)
			}
		}

		if err := deploy.ValidateUniqueProjectNames(c.Deployments); err != nil {
			return err
		}
	}

	err := validator.Validate(c)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	return nil
}

// String returns a string representation of the Config.
func (c *Config) String() string {
	return fmt.Sprintf("Config{Source: %s, SourceUrl: %s, Reference: %s, Interval: %d}", c.Source, c.SourceUrl, c.Reference, c.Interval)
}

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	type Plain Config

	if err := unmarshal((*Plain)(c)); err != nil {
		return err
	}

	return nil
}

func (c *Config) UnmarshalJSON(data []byte) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	type Plain Config

	if err := json.Unmarshal(data, (*Plain)(c)); err != nil {
		return err
	}

	return nil
}
