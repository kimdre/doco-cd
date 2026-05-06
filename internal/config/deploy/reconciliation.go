package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

var supportedReconciliationEvents = map[string]struct{}{
	"die":       {},
	"destroy":   {},
	"update":    {},
	"stop":      {},
	"kill":      {},
	"oom":       {},
	"unhealthy": {},
}

// ReconciliationConfig holds settings for the reconciliation feature.
type ReconciliationConfig struct {
	Enabled        bool     `yaml:"enabled" json:"enabled" default:"true"`               // Enabled enables the reconciliation feature
	Events         []string `yaml:"events" json:"events" default:"[\"unhealthy\"]"`      // Events is the list of Docker container actions that trigger reconciliation
	RestartTimeout int      `yaml:"restart_timeout" json:"restart_timeout" default:"10"` // RestartTimeout is the timeout in seconds to wait before killing a container during a restart
	RestartSignal  string   `yaml:"restart_signal" json:"restart_signal" default:""`     // RestartSignal is the signal sent to stop containers during a restart. If not set, the default of the Docker daemon is used (SIGTERM).
	RestartLimit   int      `yaml:"restart_limit" json:"restart_limit" default:"5"`      // RestartLimit suppresses further unhealthy-triggered restarts after this many restarts in the configured window. Set to 0 to disable suppression.
	RestartWindow  int      `yaml:"restart_window" json:"restart_window" default:"300"`  // RestartWindow is the time window in seconds used with RestartLimit.
}

func (c *ReconciliationConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return errors.New("invalid reconciliation value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	case yaml.MappingNode:
		type plain ReconciliationConfig

		decoded := plain(*c)
		if err := node.Decode(&decoded); err != nil {
			return err
		}

		*c = ReconciliationConfig(decoded)

		return nil
	default:
		return errors.New("invalid reconciliation value: expected bool or object")
	}
}

func (c *ReconciliationConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("true")) || bytes.Equal(bytes.TrimSpace(data), []byte("false")) {
		var enabled bool
		if err := json.Unmarshal(data, &enabled); err != nil {
			return errors.New("invalid reconciliation value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	}

	type plain ReconciliationConfig

	decoded := plain(*c)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	*c = ReconciliationConfig(decoded)

	return nil
}

func (c *Config) normalizeReconciliationEvents() error {
	if len(c.Reconciliation.Events) == 0 {
		c.Reconciliation.Enabled = false
		return nil
	}

	normalized := make([]string, 0, len(c.Reconciliation.Events))
	seen := make(map[string]struct{}, len(c.Reconciliation.Events))

	for _, rawEvent := range c.Reconciliation.Events {
		event := strings.ToLower(strings.TrimSpace(rawEvent))

		switch event {
		case "remove", "delete":
			event = "destroy"
		}

		if event == "" {
			return fmt.Errorf("%w: reconciliation.events contains an empty event", ErrInvalidConfig)
		}

		if _, ok := supportedReconciliationEvents[event]; !ok {
			return fmt.Errorf("%w: unsupported reconciliation event %q", ErrInvalidConfig, rawEvent)
		}

		if _, exists := seen[event]; exists {
			continue
		}

		seen[event] = struct{}{}
		normalized = append(normalized, event)
	}

	c.Reconciliation.Events = normalized

	return nil
}
