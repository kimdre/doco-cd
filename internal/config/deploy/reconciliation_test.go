package deploy

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestConfig_ReconciliationEvents_Default(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

	err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
`)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigFromYAML(filePath, true)
	if err != nil {
		t.Fatal(err)
	}

	if err = configs[0].Validate(); err != nil {
		t.Fatal(err)
	}

	want := append([]string(nil), configs[0].Reconciliation.Events...)
	if !reflect.DeepEqual(want, configs[0].Reconciliation.Events) {
		t.Fatalf("expected reconciliation events %v, got %v", want, configs[0].Reconciliation.Events)
	}

	if configs[0].Reconciliation.RestartTimeout != 10 {
		t.Fatalf("expected default reconciliation restart_timeout 10, got %d", configs[0].Reconciliation.RestartTimeout)
	}

	if configs[0].Reconciliation.RestartSignal != "" {
		t.Fatalf("expected default restart_signal empty string, got %q", configs[0].Reconciliation.RestartSignal)
	}

	if configs[0].Reconciliation.RestartLimit != 5 {
		t.Fatalf("expected default restart_limit 5, got %d", configs[0].Reconciliation.RestartLimit)
	}

	if configs[0].Reconciliation.RestartWindow != 300 {
		t.Fatalf("expected default restart_window 300, got %d", configs[0].Reconciliation.RestartWindow)
	}
}

func TestConfig_BoolOrObjectRejectsInvalidScalarTypes(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := json.Unmarshal([]byte(`{"name":"test","compose_files":["compose.yaml"],"auto_discovery":1}`), &cfg)
	if err == nil {
		t.Fatal("expected error for numeric auto_discovery")
	}

	if !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("expected unmarshal error, got %v", err)
	}

	var raw struct {
		AutoDiscovery AutoDiscoveryConfig `yaml:"auto_discovery"`
	}

	err = yaml.Unmarshal([]byte("auto_discovery: 1\n"), &raw)
	if err == nil {
		t.Fatal("expected error for numeric auto_discovery yaml value")
	}
}

func TestConfig_ReconciliationEvents_Normalize(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

	err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
reconciliation:
  events:
    - " DIE "
    - destroy
    - " UNHEALTHY "
    - " unhealthy "
    - update
    - remove
    - delete
`)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigFromYAML(filePath, true)
	if err != nil {
		t.Fatal(err)
	}

	if err = configs[0].Validate(); err != nil {
		t.Fatal(err)
	}

	want := []string{"die", "destroy", "unhealthy", "update"}
	if !reflect.DeepEqual(want, configs[0].Reconciliation.Events) {
		t.Fatalf("expected normalized reconciliation events %v, got %v", want, configs[0].Reconciliation.Events)
	}
}

func TestConfig_ReconciliationRestartSignal_Normalize(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

	err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
reconciliation:
  restart_signal: "  sigquit  "
`)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigFromYAML(filePath, true)
	if err != nil {
		t.Fatal(err)
	}

	if err = configs[0].Validate(); err != nil {
		t.Fatal(err)
	}

	if configs[0].Reconciliation.RestartSignal != "SIGQUIT" {
		t.Fatalf("expected normalized restart_signal SIGQUIT, got %q", configs[0].Reconciliation.RestartSignal)
	}
}

func TestConfig_ReconciliationEvents_Invalid(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

	err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
reconciliation:
  events: ["created"]
`)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigFromYAML(filePath, true)
	if err != nil {
		t.Fatal(err)
	}

	err = configs[0].Validate()
	if err == nil {
		t.Fatal("expected invalid reconciliation event error")
	}

	if !strings.Contains(err.Error(), "unsupported reconciliation event") {
		t.Fatalf("expected unsupported reconciliation event error, got %v", err)
	}
}

func TestConfig_ReconciliationRestartSuppression_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		yaml  string
		match string
	}{
		{
			name: "negative limit",
			yaml: `name: test
compose_files: ["compose.yaml"]
reconciliation:
  restart_limit: -1
`,
			match: "reconciliation.restart_limit",
		},
		{
			name: "negative window",
			yaml: `name: test
compose_files: ["compose.yaml"]
reconciliation:
  restart_window: -10
`,
			match: "reconciliation.restart_window",
		},
		{
			name: "limit requires positive window",
			yaml: `name: test
compose_files: ["compose.yaml"]
reconciliation:
  restart_limit: 3
  restart_window: 0
`,
			match: "reconciliation.restart_window",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")
			if err := createTestFile(t, filePath, tc.yaml); err != nil {
				t.Fatal(err)
			}

			configs, err := GetConfigFromYAML(filePath, true)
			if err != nil {
				t.Fatal(err)
			}

			err = configs[0].Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q", tc.match)
			}

			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("expected error to contain %q, got %v", tc.match, err)
			}
		})
	}
}

func TestReconciliationConfig_UnmarshalYAML_BooleanTrue(t *testing.T) {
	t.Parallel()

	yamlStr := `
name: test-deploy
reconciliation: true
`

	var cfg Config

	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal yaml: %v", err)
	}

	if !cfg.Reconciliation.Enabled {
		t.Errorf("expected reconciliation.enabled to be true, got false")
	}

	// Should have default events
	if len(cfg.Reconciliation.Events) != 1 || cfg.Reconciliation.Events[0] != "unhealthy" {
		t.Errorf("expected default event [unhealthy], got %v", cfg.Reconciliation.Events)
	}
}

func TestReconciliationConfig_UnmarshalYAML_BooleanFalse(t *testing.T) {
	t.Parallel()

	yamlStr := `
name: test-deploy
reconciliation: false
`

	var cfg Config

	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal yaml: %v", err)
	}

	if cfg.Reconciliation.Enabled {
		t.Errorf("expected reconciliation.enabled to be false, got true")
	}
}

func TestReconciliationConfig_UnmarshalYAML_Object(t *testing.T) {
	t.Parallel()

	yamlStr := `
name: test-deploy
reconciliation:
  enabled: true
  restart_timeout: 30
  restart_signal: SIGQUIT
  restart_limit: 5
  restart_window: 300
  events:
    - destroy
    - unhealthy
`

	var cfg Config

	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal yaml: %v", err)
	}

	if !cfg.Reconciliation.Enabled {
		t.Errorf("expected reconciliation.enabled to be true, got false")
	}

	if cfg.Reconciliation.RestartTimeout != 30 {
		t.Errorf("expected restart_timeout 30, got %d", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartSignal != "SIGQUIT" {
		t.Errorf("expected restart_signal SIGQUIT, got %s", cfg.Reconciliation.RestartSignal)
	}

	if len(cfg.Reconciliation.Events) != 2 {
		t.Errorf("expected 2 events, got %d", len(cfg.Reconciliation.Events))
	}
}

func TestReconciliationConfig_UnmarshalJSON_BooleanTrue(t *testing.T) {
	t.Parallel()

	jsonStr := `{"name":"test-deploy","reconciliation":true}`

	var cfg Config

	err := json.Unmarshal([]byte(jsonStr), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal json: %v", err)
	}

	if !cfg.Reconciliation.Enabled {
		t.Errorf("expected reconciliation.enabled to be true, got false")
	}

	// Should have default events
	if len(cfg.Reconciliation.Events) != 1 || cfg.Reconciliation.Events[0] != "unhealthy" {
		t.Errorf("expected default event [unhealthy], got %v", cfg.Reconciliation.Events)
	}
}

func TestReconciliationConfig_UnmarshalJSON_BooleanFalse(t *testing.T) {
	t.Parallel()

	jsonStr := `{"name":"test-deploy","reconciliation":false}`

	var cfg Config

	err := json.Unmarshal([]byte(jsonStr), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal json: %v", err)
	}

	if cfg.Reconciliation.Enabled {
		t.Errorf("expected reconciliation.enabled to be false, got true")
	}
}

func TestReconciliationConfig_UnmarshalJSON_Object(t *testing.T) {
	t.Parallel()

	jsonStr := `{
		"name":"test-deploy",
		"reconciliation":{
			"enabled":true,
			"restart_timeout":30,
			"restart_signal":"SIGQUIT",
			"restart_limit":5,
			"restart_window":300,
			"events":["destroy","unhealthy"]
		}
	}`

	var cfg Config

	err := json.Unmarshal([]byte(jsonStr), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal json: %v", err)
	}

	if !cfg.Reconciliation.Enabled {
		t.Errorf("expected reconciliation.enabled to be true, got false")
	}

	if cfg.Reconciliation.RestartTimeout != 30 {
		t.Errorf("expected restart_timeout 30, got %d", cfg.Reconciliation.RestartTimeout)
	}

	if cfg.Reconciliation.RestartSignal != "SIGQUIT" {
		t.Errorf("expected restart_signal SIGQUIT, got %s", cfg.Reconciliation.RestartSignal)
	}

	if len(cfg.Reconciliation.Events) != 2 {
		t.Errorf("expected 2 events, got %d", len(cfg.Reconciliation.Events))
	}
}
