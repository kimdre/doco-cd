package duration

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/common/validation"
)

func TestDurationDecode(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		input string
		want  time.Duration
	}{
		{input: "180", want: 3 * time.Minute},
		{input: `"3m"`, want: 3 * time.Minute},
		{input: `"180"`, want: 3 * time.Minute},
		{input: `"1.5m"`, want: 90 * time.Second},
		{input: "0", want: 0},
		{input: "-10", want: -10 * time.Second},
		{input: "null", want: time.Minute},
	} {
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()

			for _, decode := range []func([]byte, any) error{json.Unmarshal, yaml.Unmarshal} {
				value := Duration(time.Minute)
				if err := decode([]byte(test.input), &value); err != nil {
					t.Fatal(err)
				}

				if value.Duration() != test.want {
					t.Fatalf("decoded duration = %s, want %s", value, test.want)
				}
			}
		})
	}
}

func TestDurationRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	for _, input := range []string{`"500ms"`, `"invalid"`, `""`, "0.5", "true", "[]", "{}", "9223372036854775807"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			for _, decode := range []func([]byte, any) error{json.Unmarshal, yaml.Unmarshal} {
				value := Duration(time.Minute)
				if err := decode([]byte(input), &value); err == nil {
					t.Fatal("expected invalid duration to be rejected")
				}

				if value.Duration() != time.Minute {
					t.Fatalf("failed decoding changed duration to %s", value)
				}
			}
		})
	}

	for _, input := range []string{"60.0", "6e1", "60 true"} {
		var value Duration
		if err := value.UnmarshalJSON([]byte(input)); err == nil {
			t.Fatalf("expected invalid JSON integer duration %q to be rejected", input)
		}
	}
}

func TestDurationSerialization(t *testing.T) {
	t.Parallel()

	value := Duration(90 * time.Second)

	jsonData, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	yamlData, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	if string(jsonData) != "90" || string(yamlData) != "90\n" {
		t.Fatalf("expected canonical seconds, got JSON %q and YAML %q", jsonData, yamlData)
	}

	if value.Duration() != 90*time.Second || value.Seconds() != 90 || value.String() != "1m30s" {
		t.Fatalf("serialization changed duration or runtime conversions: %s", value)
	}
}

func TestDurationRejectsInvalidProgrammaticValues(t *testing.T) {
	t.Parallel()

	values := []Duration{Duration(500 * time.Millisecond)}
	if strconv.IntSize == 32 {
		values = append(values, Duration(3_000_000_000*time.Second))
	}

	for _, value := range values {
		if err := validation.Validate(value); err == nil {
			t.Fatalf("expected %s to fail scalar validation", value)
		}

		if _, err := json.Marshal(value); err == nil {
			t.Fatalf("expected %s to fail JSON serialization", value)
		}

		if _, err := yaml.Marshal(value); err == nil {
			t.Fatalf("expected %s to fail YAML serialization", value)
		}
	}
}

func TestDurationNewFieldsNeedNoRegistration(t *testing.T) {
	t.Parallel()

	type child struct {
		Delay Duration `yaml:"delay" json:"delay" default:"45s"`
	}

	type config struct {
		Extra    Duration            `yaml:"extra" json:"extra" default:"2m"`
		Child    *child              `yaml:"child" json:"child"`
		List     []Duration          `yaml:"list" json:"list"`
		Map      map[string]Duration `yaml:"map" json:"map"`
		Optional *Duration           `yaml:"optional,omitempty" json:"optional,omitempty"`
	}

	for _, decode := range []func([]byte, any) error{json.Unmarshal, yaml.Unmarshal} {
		cfg := config{Child: &child{}}
		if err := defaults.Set(&cfg); err != nil {
			t.Fatal(err)
		}

		if cfg.Extra.Duration() != 2*time.Minute || cfg.Child.Delay.Duration() != 45*time.Second {
			t.Fatalf("custom duration defaults were not applied: %+v", cfg)
		}

		if err := decode([]byte(`{"extra":"90s","child":{"delay":30},"list":["10s"],"map":{"value":"20s"}}`), &cfg); err != nil {
			t.Fatal(err)
		}

		if err := validation.Validate(cfg); err != nil {
			t.Fatal(err)
		}

		jsonData, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}

		if string(jsonData) != `{"extra":90,"child":{"delay":30},"list":[10],"map":{"value":20}}` {
			t.Fatalf("new fields did not serialize as seconds: %s", jsonData)
		}

		yamlData, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}

		var roundTrip config
		if err := yaml.Unmarshal(yamlData, &roundTrip); err != nil {
			t.Fatal(err)
		}

		if roundTrip.Extra != cfg.Extra || roundTrip.Child.Delay != cfg.Child.Delay || roundTrip.List[0] != cfg.List[0] || roundTrip.Map["value"] != cfg.Map["value"] {
			t.Fatalf("YAML round trip changed duration values:\n%s", yamlData)
		}

		cfg.Child.Delay = Duration(500 * time.Millisecond)
		if err := validation.Validate(cfg); err == nil || !strings.Contains(err.Error(), "Child.Delay") {
			t.Fatalf("expected automatic nested validation with a field path, got %v", err)
		}
	}
}
