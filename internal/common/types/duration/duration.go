package duration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"go.yaml.in/yaml/v4"
)

// Duration is a configuration duration stored as nanoseconds, like time.Duration.
// YAML and JSON accept integer seconds or Go duration strings and emit integer seconds.
type Duration time.Duration

// Duration returns the standard-library value for timers and runtime APIs.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// String returns the standard Go duration representation.
func (d Duration) String() string {
	return d.Duration().String()
}

// Seconds returns the integer seconds expected by Docker APIs.
// Configuration validation ensures the value has whole-second precision and fits an int.
func (d Duration) Seconds() int {
	return int(d.Duration() / time.Second)
}

// ValidateValue rejects fractional seconds and values that exceed native integer seconds.
func (d Duration) ValidateValue() error {
	seconds, err := WholeSeconds("configuration", d.Duration())
	if err != nil {
		return err
	}

	if int64(int(seconds)) != seconds {
		return fmt.Errorf("configuration duration %q is out of range", d)
	}

	return nil
}

// UnmarshalYAML decodes seconds or a duration string without changing the source node.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var value any
	if err := node.Decode(&value); err != nil {
		return err
	}

	return d.set(value)
}

// UnmarshalJSON keeps numeric seconds exact and preserves the current value for null.
func (d *Duration) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}

		return fmt.Errorf("unexpected JSON value after duration: %s", trailing)
	}

	return d.set(value)
}

// MarshalYAML emits canonical seconds without changing the stored duration.
func (d Duration) MarshalYAML() (any, error) {
	if err := d.ValidateValue(); err != nil {
		return nil, err
	}

	return d.Seconds(), nil
}

// MarshalJSON emits canonical seconds and rejects invalid programmatic values.
func (d Duration) MarshalJSON() ([]byte, error) {
	if err := d.ValidateValue(); err != nil {
		return nil, err
	}

	return strconv.AppendInt(nil, int64(d.Seconds()), 10), nil
}

// set assigns a parsed value only after validation; null leaves defaults unchanged.
func (d *Duration) set(value any) error {
	if value == nil {
		return nil
	}

	parsed, err := ParseSeconds("configuration", value)
	if err != nil {
		return err
	}

	next := Duration(parsed)
	if err := next.ValidateValue(); err != nil {
		return err
	}

	*d = next

	return nil
}
