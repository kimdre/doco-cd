package poll

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/kimdre/doco-cd/internal/common/types/duration"

	"github.com/kimdre/doco-cd/internal/common/cronexpr"
	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	gitInternal "github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/logger"
)

type Config struct {
	Source       config.SourceType `yaml:"source" json:"source" default:"git"`          // Source selects the poll source backend (git or oci)
	SourceUrl    string            `yaml:"url" json:"url"`                              // SourceUrl is the repository/artifact URL; validated as GitUrl or OciUrl depending on Source
	Reference    string            `yaml:"reference" json:"reference"`                  // Reference is the Git reference to the deployment, e.g., refs/heads/main, main, refs/tags/v1.0.0 or v1.0.0
	Interval     time.Duration     `yaml:"interval" json:"interval" default:"180s"`     // Interval is the interval at which to poll for changes
	Schedule     string            `yaml:"schedule" json:"schedule" default:""`         // Schedule is an optional cron expression (5-field or descriptor) evaluated in the local timezone (TZ) that replaces Interval
	CustomTarget string            `yaml:"target" json:"target" default:""`             // CustomTarget is the name of an optional custom deployment config file, e.g. ".doco-cd.custom-name.yaml"
	RunOnce      bool              `yaml:"run_once" json:"run_once" default:"false"`    // RunOnce when true, performs a single run and exits
	Watch        bool              `yaml:"watch" json:"watch" default:"true"`           // Watch enables a filesystem watcher for local git repositories that triggers a poll immediately on new commits; ignored for non-local git and OCI sources
	Deployments  []*deploy.Config  `yaml:"deployments" json:"deployments" default:"[]"` // Deployments allows defining deployment configs inline in the poll configuration
}

type rawConfig struct {
	Source       config.SourceType `yaml:"source" json:"source" default:"git"`
	SourceUrl    string            `yaml:"url" json:"url"`
	Reference    string            `yaml:"reference" json:"reference"`
	Interval     any               `yaml:"interval" json:"interval" default:"180s"`
	Schedule     string            `yaml:"schedule" json:"schedule" default:""`
	CustomTarget string            `yaml:"target" json:"target" default:""`
	RunOnce      bool              `yaml:"run_once" json:"run_once" default:"false"`
	Watch        bool              `yaml:"watch" json:"watch" default:"true"`
	Deployments  []*deploy.Config  `yaml:"deployments" json:"deployments" default:"[]"`
}

type Job struct {
	Config  Config // config is the Config for this instance
	LastRun int64  // LastRun is the last time this instance ran
	NextRun int64  // NextRun is the next time this instance should run
}

const MinPollInterval = 10 * time.Second // Minimum allowed poll interval

var (
	ErrInvalidConfig        = errors.New("invalid poll configuration")
	ErrBothConfigSet        = errors.New("both POLL_CONFIG and POLL_CONFIG_FILE are set, please use one or the other")
	ErrIntervalTooLow       = errors.New("poll interval too low")
	ErrScheduleWithInterval = errors.New("poll schedule and interval are mutually exclusive, please set only one of them")
	ErrInvalidSchedule      = errors.New("invalid poll schedule")
)

// intervalNotSet marks an interval that was omitted from the decoded
// document, so it can be told apart from an explicit "interval: 0" or null.
type intervalNotSet struct{}

// minScheduleGapSamples is how many upcoming occurrences are compared when
// checking a schedule against MinPollInterval.
const minScheduleGapSamples = 5

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
		c.SourceUrl = config.NormalizeGitURL(c.SourceUrl)

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

	if err := c.validateSchedule(); err != nil {
		return err
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

	err := validation.Validate(c)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	return nil
}

func (c *Config) validateSchedule() error {
	c.Schedule = strings.TrimSpace(c.Schedule)
	if c.Schedule == "" {
		return nil
	}

	if c.Interval != 0 {
		return ErrScheduleWithInterval
	}

	schedule, err := c.ParseSchedule()
	if err != nil {
		return err
	}

	if gap := cronexpr.MinGap(schedule, time.Now(), minScheduleGapSamples); gap > 0 && gap < MinPollInterval {
		return fmt.Errorf("%w: schedule %q runs every %s, must be at least %s", ErrIntervalTooLow, c.Schedule, gap, MinPollInterval)
	}

	return nil
}

// ParseSchedule parses Schedule in the local timezone (TZ). It returns nil
// without an error when no schedule is configured.
func (c *Config) ParseSchedule() (gocron.Cron, error) {
	spec := strings.TrimSpace(c.Schedule)
	if spec == "" {
		return nil, nil
	}

	schedule, err := cronexpr.Parse(spec, time.Local)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %v", ErrInvalidSchedule, spec, err)
	}

	return schedule, nil
}

// NextRun returns when a job for this config is expected to run next after
// now, based on Schedule or Interval. It returns the zero time when the config
// runs once or has neither a schedule nor an interval.
func (c *Config) NextRun(now time.Time) time.Time {
	if c.RunOnce {
		return time.Time{}
	}

	if c.Schedule != "" {
		schedule, err := c.ParseSchedule()
		if err != nil || schedule == nil {
			return time.Time{}
		}

		return schedule.Next(now)
	}

	if c.Interval > 0 {
		return now.Add(c.Interval)
	}

	return time.Time{}
}

// String returns a string representation of the Config.
func (c *Config) String() string {
	if c.Schedule != "" {
		return fmt.Sprintf("Config{Source: %s, SourceUrl: %s, Reference: %s, Schedule: %s}", c.Source, c.SourceUrl, c.Reference, c.Schedule)
	}

	return fmt.Sprintf("Config{Source: %s, SourceUrl: %s, Reference: %s, Interval: %s}", c.Source, c.SourceUrl, c.Reference, c.Interval)
}

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	raw := c.newRawConfig()

	if err := unmarshal(&raw); err != nil {
		return err
	}

	return c.applyRawConfig(raw)
}

func (c *Config) UnmarshalJSON(data []byte) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	raw := c.newRawConfig()

	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	return c.applyRawConfig(raw)
}

// newRawConfig seeds a rawConfig with the current (default) values. The
// interval starts as intervalNotSet so applyRawConfig can tell an omitted
// interval apart from an explicit one.
func (c *Config) newRawConfig() rawConfig {
	return rawConfig{
		Source:       c.Source,
		SourceUrl:    c.SourceUrl,
		Reference:    c.Reference,
		Interval:     intervalNotSet{},
		Schedule:     c.Schedule,
		CustomTarget: c.CustomTarget,
		RunOnce:      c.RunOnce,
		Watch:        c.Watch,
		Deployments:  c.Deployments,
	}
}

// applyRawConfig copies a decoded rawConfig into c. An omitted interval keeps
// the default unless a schedule is set, which replaces the interval.
func (c *Config) applyRawConfig(raw rawConfig) error {
	schedule := strings.TrimSpace(raw.Schedule)

	interval := c.Interval
	if _, omitted := raw.Interval.(intervalNotSet); omitted {
		if schedule != "" {
			interval = 0
		}
	} else {
		parsedInterval, err := parsePollInterval(raw.Interval)
		if err != nil {
			return err
		}

		if schedule != "" && parsedInterval != 0 {
			return ErrScheduleWithInterval
		}

		interval = parsedInterval
	}

	c.Source = raw.Source
	c.SourceUrl = raw.SourceUrl
	c.Reference = raw.Reference
	c.Interval = interval
	c.Schedule = schedule
	c.CustomTarget = raw.CustomTarget
	c.RunOnce = raw.RunOnce
	c.Watch = raw.Watch
	c.Deployments = raw.Deployments

	return nil
}

func parsePollInterval(v any) (time.Duration, error) {
	return duration.ParseSeconds("interval", v)
}
