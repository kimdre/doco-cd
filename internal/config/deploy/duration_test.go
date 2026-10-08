package deploy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
)

func TestConfig_UnmarshalYAMLDurationValues(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := yaml.Unmarshal([]byte(`name: app
timeout: 3m
reconciliation:
  restart_timeout: 15s
  restart_window: 1m30s
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 3*time.Minute {
		t.Errorf("timeout = %s, want 3m", cfg.Timeout)
	}

	if cfg.Reconciliation.RestartTimeout.Duration() != 15*time.Second {
		t.Errorf("restart_timeout = %s, want 15s", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartWindow.Duration() != 90*time.Second {
		t.Errorf("restart_window = %s, want 90s", cfg.Reconciliation.RestartWindow)
	}
}

func TestConfig_UnmarshalYAMLDurationFromMerge(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := yaml.Unmarshal([]byte(`base: &base
  timeout: 3m
  reconciliation: {restart_timeout: 15s, restart_window: 5m}
<<: *base
name: app
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 3*time.Minute {
		t.Errorf("timeout = %s, want 3m", cfg.Timeout)
	}

	if cfg.Reconciliation.RestartTimeout.Duration() != 15*time.Second {
		t.Errorf("restart_timeout = %s, want 15s", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartWindow.Duration() != 5*time.Minute {
		t.Errorf("restart_window = %s, want 5m", cfg.Reconciliation.RestartWindow)
	}
}

func TestConfig_UnmarshalJSONDurationValues(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := json.Unmarshal([]byte(`{"name":"app","timeout":"3m","reconciliation":{"restart_timeout":"15s","restart_window":"1m30s"}}`), &cfg)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 3*time.Minute {
		t.Errorf("timeout = %s, want 3m", cfg.Timeout)
	}

	if cfg.Reconciliation.RestartTimeout.Duration() != 15*time.Second {
		t.Errorf("restart_timeout = %s, want 15s", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartWindow.Duration() != 90*time.Second {
		t.Errorf("restart_window = %s, want 90s", cfg.Reconciliation.RestartWindow)
	}
}

func TestConfig_UnmarshalNumericJSONDurationValuesAsSeconds(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := json.Unmarshal([]byte(`{"name":"app","timeout":180,"reconciliation":{"restart_timeout":10,"restart_window":300}}`), &cfg)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 3*time.Minute {
		t.Errorf("timeout = %s, want 3m", cfg.Timeout)
	}

	if cfg.Reconciliation.RestartTimeout.Duration() != 10*time.Second {
		t.Errorf("restart_timeout = %s, want 10s", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartWindow.Duration() != 5*time.Minute {
		t.Errorf("restart_window = %s, want 5m", cfg.Reconciliation.RestartWindow)
	}
}

func TestConfig_UnmarshalNumericDurationValuesAsSeconds(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := yaml.Unmarshal([]byte(`name: app
timeout: 180
reconciliation: {restart_timeout: 10, restart_window: 300}
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 3*time.Minute {
		t.Errorf("timeout = %s, want 3m", cfg.Timeout)
	}

	if cfg.Reconciliation.RestartTimeout.Duration() != 10*time.Second {
		t.Errorf("restart_timeout = %s, want 10s", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartWindow.Duration() != 5*time.Minute {
		t.Errorf("restart_window = %s, want 5m", cfg.Reconciliation.RestartWindow)
	}
}

func TestConfig_DurationDefaults(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := yaml.Unmarshal([]byte("name: app"), &cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 3*time.Minute {
		t.Errorf("timeout default = %s, want 3m", cfg.Timeout)
	}

	if cfg.Reconciliation.RestartTimeout.Duration() != 10*time.Second {
		t.Errorf("restart_timeout default = %s, want 10s", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartWindow.Duration() != 5*time.Minute {
		t.Errorf("restart_window default = %s, want 5m", cfg.Reconciliation.RestartWindow)
	}
}

func TestConfig_UnmarshalRejectsSubsecondDurations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
	}{
		{name: "timeout", yaml: "timeout: 500ms"},
		{name: "restart timeout", yaml: "reconciliation: {restart_timeout: 500ms}"},
		{name: "restart window", yaml: "reconciliation: {restart_window: 500ms}"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var cfg Config
			if err := yaml.Unmarshal([]byte("name: app\n"+test.yaml), &cfg); err == nil {
				t.Fatal("expected subsecond duration to be rejected")
			}
		})
	}
}

func TestConfig_HashCanonicalizesDurationsAsSeconds(t *testing.T) {
	t.Parallel()

	var numeric Config
	if err := yaml.Unmarshal([]byte(`name: app
timeout: 180
reconciliation: {restart_timeout: 10, restart_window: 300}
`), &numeric); err != nil {
		t.Fatal(err)
	}

	var durationConfig Config
	if err := yaml.Unmarshal([]byte(`name: app
timeout: 3m
reconciliation: {restart_timeout: 10s, restart_window: 5m}
`), &durationConfig); err != nil {
		t.Fatal(err)
	}

	numericHash, err := numeric.Hash()
	if err != nil {
		t.Fatal(err)
	}

	durationHash, err := durationConfig.Hash()
	if err != nil {
		t.Fatal(err)
	}

	if numericHash != durationHash {
		t.Fatalf("equivalent duration forms have different hashes: %s != %s", numericHash, durationHash)
	}

	encoded, err := yaml.Marshal(&durationConfig)
	if err != nil {
		t.Fatal(err)
	}

	for _, expected := range []string{"timeout: 180", "restart_timeout: 10", "restart_window: 300"} {
		if !strings.Contains(string(encoded), expected) {
			t.Errorf("YAML encoding missing canonical seconds %q:\n%s", expected, encoded)
		}
	}
}

func TestConfig_MarshalJSONUsesSeconds(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Timeout: duration.Duration(3 * time.Minute),
		Reconciliation: ReconciliationConfig{
			RestartTimeout: duration.Duration(10 * time.Second),
			RestartWindow:  duration.Duration(5 * time.Minute),
		},
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Timeout        int64 `json:"timeout"`
		Reconciliation struct {
			RestartTimeout int64 `json:"restart_timeout"`
			RestartWindow  int64 `json:"restart_window"`
		} `json:"reconciliation"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}

	if got.Timeout != 180 {
		t.Errorf("timeout JSON value = %d, want 180 seconds", got.Timeout)
	}

	if got.Reconciliation.RestartTimeout != 10 {
		t.Errorf("restart_timeout JSON value = %d, want 10 seconds", got.Reconciliation.RestartTimeout)
	}

	if got.Reconciliation.RestartWindow != 300 {
		t.Errorf("restart_window JSON value = %d, want 300 seconds", got.Reconciliation.RestartWindow)
	}
}

func TestConfig_DurationsWithoutDefaults(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"180", "3m"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			configs, err := getConfigFromYAMLBytes([]byte("timeout: "+value), "override.yml", false)
			if err != nil {
				t.Fatal(err)
			}

			if got := configs[0].Timeout.Duration(); got != 3*time.Minute {
				t.Fatalf("timeout = %s, want 3m", got)
			}

			if configs[0].Reconciliation.RestartTimeout != 0 || configs[0].Reconciliation.RestartWindow != 0 {
				t.Fatal("omitted reconciliation durations must remain unset")
			}
		})
	}

	t.Run("subsecond override", func(t *testing.T) {
		t.Parallel()

		if _, err := getConfigFromYAMLBytes([]byte("timeout: 500ms"), "override.yml", false); err == nil {
			t.Fatal("expected subsecond duration to be rejected")
		}
	})
}

func TestConfig_DurationMergePreservesScalarText(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := yaml.Unmarshal([]byte(`base: &base {timeout: 180}
<<: *base
name: 00123
environment: {CODE: 00123}
`), &cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.Name != "00123" || cfg.Environment["CODE"] != "00123" {
		t.Fatalf("merge changed scalar text: name = %q, CODE = %q", cfg.Name, cfg.Environment["CODE"])
	}
}

func TestConfig_RejectsDuplicateYAMLDurationKeys(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"timeout: 180\ntimeout: 3m",
		"reconciliation: {restart_timeout: 10, restart_timeout: 10s}",
		"reconciliation: {restart_window: 300, restart_window: 5m}",
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			var cfg Config
			if err := yaml.Unmarshal([]byte(input), &cfg); err == nil {
				t.Fatal("expected duplicate duration key to be rejected")
			}
		})
	}
}

func TestConfig_DuplicateJSONObjectsPreserveDurations(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := json.Unmarshal([]byte(`{
		"reconciliation": {"restart_timeout": "20s"},
		"reconciliation": {"restart_window": "45s"}
	}`), &cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.Reconciliation.RestartTimeout.Duration() != 20*time.Second || cfg.Reconciliation.RestartWindow.Duration() != 45*time.Second {
		t.Fatalf("repeated JSON objects lost durations: %+v", cfg.Reconciliation)
	}
}

func TestConfig_JSONDurationKeysFollowInputOrder(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := json.Unmarshal([]byte(`{
		"timeout": 180, "TIMEOUT": "90s", "timeout": null,
		"reconciliation": {
			"restart_timeout": 10, "RESTART_TIMEOUT": "20s",
			"restart_window": 300, "RESTART_WINDOW": "45s"
		}
	}`), &cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 90*time.Second || cfg.Reconciliation.RestartTimeout.Duration() != 20*time.Second || cfg.Reconciliation.RestartWindow.Duration() != 45*time.Second {
		t.Fatalf("duration keys did not follow JSON input order: timeout = %s, reconciliation = %+v", cfg.Timeout, cfg.Reconciliation)
	}
}

func TestConfig_YAMLDurationMergesAndAliases(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := yaml.Unmarshal([]byte(`first: &first {timeout: 180}
second: &second {timeout: 60}
restart: &restart {restart_timeout: 10, restart_window: 300}
window: &window 45s
<<: [*first, *second]
reconciliation:
  <<: *restart
  restart_window: *window
`), &cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.Timeout.Duration() != 3*time.Minute || cfg.Reconciliation.RestartTimeout.Duration() != 10*time.Second || cfg.Reconciliation.RestartWindow.Duration() != 45*time.Second {
		t.Fatalf("unexpected merged durations: timeout = %s, reconciliation = %+v", cfg.Timeout, cfg.Reconciliation)
	}
}

func TestConfig_NullDurationsPreserveDefaults(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		decode func(*Config) error
	}{
		{name: "YAML", decode: func(cfg *Config) error {
			return yaml.Unmarshal([]byte("timeout: null\nreconciliation: {restart_timeout: null, restart_window: null}"), cfg)
		}},
		{name: "JSON", decode: func(cfg *Config) error {
			return json.Unmarshal([]byte(`{"timeout":null,"reconciliation":{"restart_timeout":null,"restart_window":null}}`), cfg)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var cfg Config
			if err := test.decode(&cfg); err != nil {
				t.Fatal(err)
			}

			if cfg.Timeout.Duration() != 3*time.Minute || cfg.Reconciliation.RestartTimeout.Duration() != 10*time.Second || cfg.Reconciliation.RestartWindow.Duration() != 5*time.Minute {
				t.Fatalf("null changed defaults: timeout = %s, reconciliation = %+v", cfg.Timeout, cfg.Reconciliation)
			}
		})
	}
}

func TestConfig_ProgrammaticDurationsRejectSubseconds(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		field string
		set   func(*Config)
	}{
		{field: "Timeout", set: func(cfg *Config) { cfg.Timeout = duration.Duration(500 * time.Millisecond) }},
		{field: "Reconciliation.RestartTimeout", set: func(cfg *Config) { cfg.Reconciliation.RestartTimeout = duration.Duration(500 * time.Millisecond) }},
		{field: "Reconciliation.RestartWindow", set: func(cfg *Config) { cfg.Reconciliation.RestartWindow = duration.Duration(500 * time.Millisecond) }},
	} {
		t.Run(test.field, func(t *testing.T) {
			t.Parallel()

			var cfg Config
			if err := yaml.Unmarshal([]byte("name: app"), &cfg); err != nil {
				t.Fatal(err)
			}

			test.set(&cfg)

			if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("expected field-specific invalid config error, got %v", err)
			}

			if _, err := yaml.Marshal(cfg); err == nil || !strings.Contains(err.Error(), "full seconds") {
				t.Fatalf("expected whole-second YAML encoding error, got %v", err)
			}

			if _, err := json.Marshal(cfg); err == nil || !strings.Contains(err.Error(), "full seconds") {
				t.Fatalf("expected whole-second JSON encoding error, got %v", err)
			}
		})
	}
}

func TestConfig_DurationValidationOrder(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := yaml.Unmarshal([]byte("name: app"), &cfg); err != nil {
		t.Fatal(err)
	}

	fields := []struct {
		name  string
		value *duration.Duration
	}{
		{name: "Timeout", value: &cfg.Timeout},
		{name: "Reconciliation.RestartTimeout", value: &cfg.Reconciliation.RestartTimeout},
		{name: "Reconciliation.RestartWindow", value: &cfg.Reconciliation.RestartWindow},
	}
	for _, field := range fields {
		*field.value = duration.Duration(500 * time.Millisecond)
	}

	for _, field := range fields {
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), field.name+":") {
			t.Fatalf("expected %s validation error, got %v", field.name, err)
		}

		*field.value = duration.Duration(time.Second)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected whole-second durations to be valid: %v", err)
	}
}

func TestConfig_DurationAliasDoesNotChangeOtherFields(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"timeout: &shared 180\nname: *shared",
		"shared: &shared 180\nname: *shared\ntimeout: *shared",
	} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
			t.Fatal(err)
		}

		if cfg.Name != "180" || cfg.Timeout.Duration() != 3*time.Minute {
			t.Fatalf("duration decoding changed shared alias: name = %q, timeout = %s", cfg.Name, cfg.Timeout)
		}
	}
}

func TestConfig_DurationRoundTrip(t *testing.T) {
	t.Parallel()

	const input = `name: app
timeout: 90s
reconciliation: {restart_timeout: 20s, restart_window: 45s}
`

	var cfg Config
	if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatal(err)
	}

	for _, codec := range []struct {
		name   string
		encode func(any) ([]byte, error)
		decode func([]byte, any) error
	}{
		{name: "YAML", encode: yaml.Marshal, decode: yaml.Unmarshal},
		{name: "JSON", encode: json.Marshal, decode: json.Unmarshal},
	} {
		t.Run(codec.name, func(t *testing.T) {
			t.Parallel()

			data, err := codec.encode(cfg)
			if err != nil {
				t.Fatal(err)
			}

			var roundTrip Config
			if err := codec.decode(data, &roundTrip); err != nil {
				t.Fatal(err)
			}

			if roundTrip.Timeout != cfg.Timeout || roundTrip.Reconciliation.RestartTimeout != cfg.Reconciliation.RestartTimeout || roundTrip.Reconciliation.RestartWindow != cfg.Reconciliation.RestartWindow {
				t.Fatalf("round trip changed durations: %+v", roundTrip)
			}
		})
	}
}
