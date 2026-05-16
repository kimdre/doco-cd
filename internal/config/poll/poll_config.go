package poll

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/creasty/defaults"
	"gopkg.in/validator.v2"

	"github.com/kimdre/doco-cd/internal/config"
	gitInternal "github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/logger"
)

type Config struct {
	Source       config.SourceType `yaml:"source" json:"source" default:"git"`          // Source selects the poll source backend (git or oci)
	SourceUrl    string            `yaml:"url" json:"url"`                              // SourceUrl is the repository/artifact URL; validated as GitUrl or OciUrl depending on Source
	Reference    string            `yaml:"reference" json:"reference"`                  // Reference is the Git reference to the deployment, e.g., refs/heads/main, main, refs/tags/v1.0.0 or v1.0.0
	Interval     time.Duration     `yaml:"interval" default:"180s"`                     // Interval is the interval at which to poll for changes
	CustomTarget string            `yaml:"target" json:"target" default:""`             // CustomTarget is the name of an optional custom deployment config file, e.g. ".doco-cd.custom-name.yaml"
	RunOnce      bool              `yaml:"run_once" default:"false"`                    // RunOnce when true, performs a single run and exits
	Deployments  []*deploy.Config  `yaml:"deployments" json:"deployments" default:"[]"` // Deployments allows defining deployment configs inline in the poll configuration
}

type rawConfig struct {
	Source       config.SourceType `yaml:"source" json:"source" default:"git"`
	SourceUrl    string            `yaml:"url" json:"url"`
	Reference    string            `yaml:"reference" json:"reference"`
	Interval     any               `yaml:"interval" json:"interval" default:"180s"`
	CustomTarget string            `yaml:"target" json:"target" default:""`
	RunOnce      bool              `yaml:"run_once" json:"run_once" default:"false"`
	Deployments  []*deploy.Config  `yaml:"deployments" json:"deployments" default:"[]"`
}

type Job struct {
	Config  Config // config is the Config for this instance
	LastRun int64  // LastRun is the last time this instance ran
	NextRun int64  // NextRun is the next time this instance should run
}

const MinPollInterval = 10 * time.Second // Minimum allowed poll interval

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

	if c.Reference == "" && c.Source != config.SourceTypeOCI {
		c.Reference = gitInternal.MainBranch
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
		return fmt.Errorf("%w: must be at least %s", ErrIntervalTooLow, MinPollInterval)
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
	return fmt.Sprintf("Config{Source: %s, SourceUrl: %s, Reference: %s, Interval: %s}", c.Source, c.SourceUrl, c.Reference, c.Interval)
}

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	raw := rawConfig{
		Source:       c.Source,
		SourceUrl:    c.SourceUrl,
		Reference:    c.Reference,
		Interval:     c.Interval,
		CustomTarget: c.CustomTarget,
		RunOnce:      c.RunOnce,
		Deployments:  c.Deployments,
	}

	if err := unmarshal(&raw); err != nil {
		return err
	}

	parsedInterval, err := parsePollInterval(raw.Interval)
	if err != nil {
		return err
	}

	c.Source = raw.Source
	c.SourceUrl = raw.SourceUrl
	c.Reference = raw.Reference
	c.Interval = parsedInterval
	c.CustomTarget = raw.CustomTarget
	c.RunOnce = raw.RunOnce
	c.Deployments = raw.Deployments

	return nil
}

func (c *Config) UnmarshalJSON(data []byte) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	raw := rawConfig{
		Source:       c.Source,
		SourceUrl:    c.SourceUrl,
		Reference:    c.Reference,
		Interval:     c.Interval,
		CustomTarget: c.CustomTarget,
		RunOnce:      c.RunOnce,
		Deployments:  c.Deployments,
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	parsedInterval, err := parsePollInterval(raw.Interval)
	if err != nil {
		return err
	}

	c.Source = raw.Source
	c.SourceUrl = raw.SourceUrl
	c.Reference = raw.Reference
	c.Interval = parsedInterval
	c.CustomTarget = raw.CustomTarget
	c.RunOnce = raw.RunOnce
	c.Deployments = raw.Deployments

	return nil
}

func parsePollInterval(v any) (time.Duration, error) {
	if v == nil {
		return 0, nil
	}

	switch value := v.(type) {
	case string:
		return parsePollIntervalString(value)
	case int:
		return secondsToDuration(int64(value))
	case int64:
		return secondsToDuration(value)
	case uint:
		if uint64(value) > math.MaxInt64 {
			return 0, fmt.Errorf("invalid interval value %d: out of range", value)
		}

		return secondsToDuration(int64(value))
	case uint64:
		if value > math.MaxInt64 {
			return 0, fmt.Errorf("invalid interval value %d: out of range", value)
		}

		return secondsToDuration(int64(value))
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("invalid interval value %v: must be a whole number of seconds", value)
		}

		if value > math.MaxInt64 || value < math.MinInt64 {
			return 0, fmt.Errorf("invalid interval value %v: out of range", value)
		}

		return secondsToDuration(int64(value))
	case json.Number:
		seconds, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("invalid interval value %q: %w", value.String(), err)
		}

		return secondsToDuration(seconds)
	default:
		return 0, fmt.Errorf("invalid interval type %T: expected number or string", v)
	}
}

func secondsToDuration(seconds int64) (time.Duration, error) {
	maxSeconds := math.MaxInt64 / int64(time.Second)
	minSeconds := math.MinInt64 / int64(time.Second)

	if seconds > maxSeconds || seconds < minSeconds {
		return 0, fmt.Errorf("invalid interval value %d: out of range", seconds)
	}

	return time.Duration(seconds) * time.Second, nil
}

func parsePollIntervalString(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, errors.New("invalid interval value: must not be empty")
	}

	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return secondsToDuration(seconds)
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid interval value %q: must be seconds or a Go duration", raw)
	}

	if duration%time.Second != 0 {
		return 0, fmt.Errorf("invalid interval duration %q: must resolve to full seconds", raw)
	}

	return duration, nil
}
