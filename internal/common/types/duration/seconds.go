package duration

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseSeconds parses numeric values as seconds and string values as either
// seconds or Go durations. Duration strings must resolve to whole seconds.
// Nil yields zero; already-typed durations are returned unchanged.
func ParseSeconds(field string, value any) (time.Duration, error) {
	if value == nil {
		return 0, nil
	}

	switch value := value.(type) {
	case time.Duration:
		return value, nil
	case string:
		return parseString(field, value)
	case int:
		return secondsToDuration(field, int64(value))
	case int64:
		return secondsToDuration(field, value)
	case uint:
		if uint64(value) > math.MaxInt64 {
			return 0, fmt.Errorf("invalid %s value %d: out of range", field, value)
		}

		return secondsToDuration(field, int64(value))
	case uint64:
		if value > math.MaxInt64 {
			return 0, fmt.Errorf("invalid %s value %d: out of range", field, value)
		}

		return secondsToDuration(field, int64(value))
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("invalid %s value %v: must be a whole number of seconds", field, value)
		}

		if value > float64(math.MaxInt64/int64(time.Second)) || value < float64(math.MinInt64/int64(time.Second)) {
			return 0, fmt.Errorf("invalid %s value %v: out of range", field, value)
		}

		return secondsToDuration(field, int64(value))
	case json.Number:
		seconds, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("invalid %s value %q: %w", field, value.String(), err)
		}

		return secondsToDuration(field, seconds)
	default:
		return 0, fmt.Errorf("invalid %s type %T: expected number or string", field, value)
	}
}

// parseString parses a string value as either a number of seconds or a Go duration.
func parseString(field, raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, fmt.Errorf("invalid %s value: must not be empty", field)
	}

	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return secondsToDuration(field, seconds)
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value %q: must be seconds or a Go duration", field, raw)
	}

	if _, err := WholeSeconds(field, duration); err != nil {
		return 0, err
	}

	return duration, nil
}

// secondsToDuration converts a number of seconds to a time.Duration,
// ensuring that the value is within the valid range for time.Duration.
func secondsToDuration(field string, seconds int64) (time.Duration, error) {
	maxSeconds := math.MaxInt64 / int64(time.Second)
	minSeconds := math.MinInt64 / int64(time.Second)

	if seconds > maxSeconds || seconds < minSeconds {
		return 0, fmt.Errorf("invalid %s value %d: out of range", field, seconds)
	}

	return time.Duration(seconds) * time.Second, nil
}

// WholeSeconds checks if the given duration resolves to whole seconds and returns that count.
func WholeSeconds(field string, value time.Duration) (int64, error) {
	if value%time.Second != 0 {
		return 0, fmt.Errorf("invalid %s duration %q: must resolve to full seconds", field, value)
	}

	return int64(value / time.Second), nil
}
