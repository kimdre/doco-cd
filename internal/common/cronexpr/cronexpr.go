// Package cronexpr parses the cron expressions shared by job scheduling,
// poll schedules and sync windows, so all of them accept the same syntax.
package cronexpr

import (
	"errors"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"
)

// ErrEmptyExpression is returned when an expression is empty or only whitespace.
var ErrEmptyExpression = errors.New("cron expression must not be empty")

// Parse validates spec as a 5-field cron expression (descriptors such as
// "@daily" and "@every 5m" are supported, seconds are not) evaluated in loc.
// A nil loc means time.Local. A "TZ=" or "CRON_TZ=" prefix in spec takes
// precedence over loc.
//
// Errors are returned without the expression so callers can add their own
// context.
//
// Every call returns a new schedule: gocron keeps the parsed state inside the
// instance that validated it, so instances must not be shared between
// expressions.
func Parse(spec string, loc *time.Location) (gocron.Cron, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, ErrEmptyExpression
	}

	if loc == nil {
		loc = time.Local
	}

	schedule := gocron.NewDefaultCron(false)
	if err := schedule.IsValid(spec, loc, time.Now()); err != nil {
		return nil, err
	}

	return schedule, nil
}

// HasTimezonePrefix reports whether spec carries its own "TZ=" or "CRON_TZ=" prefix.
func HasTimezonePrefix(spec string) bool {
	spec = strings.TrimSpace(spec)

	return strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=")
}

// MinGap returns the smallest gap between the next samples occurrences of
// schedule after from. It lets callers enforce a minimum interval on
// descriptors such as "@every 5s". It returns 0 if schedule has fewer than two
// upcoming occurrences.
func MinGap(schedule gocron.Cron, from time.Time, samples int) time.Duration {
	samples = max(samples, 2)

	prev := schedule.Next(from)
	if prev.IsZero() {
		return 0
	}

	var minGap time.Duration

	for range samples - 1 {
		next := schedule.Next(prev)
		if next.IsZero() {
			break
		}

		if gap := next.Sub(prev); minGap == 0 || gap < minGap {
			minGap = gap
		}

		prev = next
	}

	return minGap
}
