package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/common/types/duration"
)

func TestRetryConfigValidate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		config RetryConfig
		field  string
	}{
		{name: "omitted"},
		{name: "zero retries", config: RetryConfig{Limit: new(0)}},
		{name: "finite retries", config: RetryConfig{Limit: new(2)}},
		{name: "delay only", config: RetryConfig{Delay: duration.Duration(time.Minute)}},
		{name: "negative retries", config: RetryConfig{Limit: new(-1)}, field: "Limit"},
		{name: "negative delay", config: RetryConfig{Delay: duration.Duration(-time.Minute)}, field: "Delay"},
		{name: "negative limits", config: RetryConfig{Limit: new(-1), Delay: duration.Duration(-time.Minute)}, field: "Limit"},
		{name: "subsecond delay", config: RetryConfig{Delay: duration.Duration(500 * time.Millisecond)}, field: "Delay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.config.Validate()
			if test.field == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}

				return
			}

			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("expected %s validation error, got %v", test.field, err)
			}
		})
	}
}

func TestRetryConfigConfigured(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		config RetryConfig
		want   bool
	}{
		{name: "omitted", config: RetryConfig{}, want: false},
		{name: "no retries", config: RetryConfig{Limit: new(0)}, want: true},
		{name: "finite retries", config: RetryConfig{Limit: new(2)}, want: true},
		{name: "delay only", config: RetryConfig{Delay: duration.Duration(time.Minute)}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := test.config.Configured(); got != test.want {
				t.Errorf("Configured() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestConfigRetryDecoding(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		yaml  string
		json  string
		limit *int
		delay time.Duration
	}{
		{name: "omitted"},
		{name: "empty object", yaml: "retry: {}", json: `"retry":{}`},
		{name: "null object", yaml: "retry: null", json: `"retry":null`},
		{name: "explicit zero", yaml: "retry: {limit: 0, delay: 0}", json: `"retry":{"limit":0,"delay":0}`, limit: new(0)},
		{name: "finite retries", yaml: "retry: {limit: 2}", json: `"retry":{"limit":2}`, limit: new(2)},
		{name: "delay only", yaml: "retry: {delay: 5m}", json: `"retry":{"delay":"5m"}`, delay: 5 * time.Minute},
		{name: "numeric seconds", yaml: "retry: {limit: 2, delay: 90}", json: `"retry":{"limit":2,"delay":90}`, limit: new(2), delay: 90 * time.Second},
		{name: "duration string", yaml: "retry: {limit: 2, delay: 1m30s}", json: `"retry":{"limit":2,"delay":"1m30s"}`, limit: new(2), delay: 90 * time.Second},
		{name: "null fields", yaml: "retry: {limit: null, delay: null}", json: `"retry":{"limit":null,"delay":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			for _, codec := range []struct {
				name   string
				input  string
				decode func([]byte, any) error
				encode func(any) ([]byte, error)
			}{
				{name: "YAML", input: "name: app\n" + test.yaml, decode: yaml.Unmarshal, encode: yaml.Marshal},
				{name: "JSON", input: `{"name":"app"` + retryJSONField(test.json) + `}`, decode: json.Unmarshal, encode: json.Marshal},
			} {
				t.Run(codec.name, func(t *testing.T) {
					t.Parallel()

					var cfg Config
					if err := codec.decode([]byte(codec.input), &cfg); err != nil {
						t.Fatal(err)
					}

					if !reflect.DeepEqual(cfg.Retry.Limit, test.limit) || cfg.Retry.Delay.Duration() != test.delay {
						t.Fatalf("unexpected retry config: %+v", cfg.Retry)
					}

					if err := cfg.Validate(); err != nil {
						t.Fatal(err)
					}

					encoded, err := codec.encode(cfg)
					if err != nil {
						t.Fatal(err)
					}

					var roundTrip Config
					if err := codec.decode(encoded, &roundTrip); err != nil {
						t.Fatal(err)
					}

					if !reflect.DeepEqual(roundTrip.Retry, cfg.Retry) {
						t.Fatalf("retry changed on round trip: %+v != %+v", roundTrip.Retry, cfg.Retry)
					}
				})
			}
		})
	}
}

func retryJSONField(field string) string {
	if field == "" {
		return ""
	}

	return "," + field
}

func TestConfigRetryDefaults(t *testing.T) {
	t.Parallel()

	cfg := New("app", "main")
	if err := defaults.Set(cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.Retry.Limit != nil || cfg.Retry.Delay != 0 || cfg.Retry.Configured() {
		t.Fatalf("default retry must be unlimited without delay: %+v", cfg.Retry)
	}
}

func TestConfigRetryValidation(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		yaml  string
		json  string
		field string
	}{
		{name: "negative retries", yaml: "limit: -1", json: `"limit":-1`, field: "Retry.Limit"},
		{name: "negative numeric delay", yaml: "delay: -1", json: `"delay":-1`, field: "Retry.Delay"},
		{name: "negative duration delay", yaml: "delay: -5m", json: `"delay":"-5m"`, field: "Retry.Delay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			for _, codec := range []struct {
				name   string
				input  string
				decode func([]byte, any) error
			}{
				{name: "YAML", input: "name: app\nretry: {" + test.yaml + "}", decode: yaml.Unmarshal},
				{name: "JSON", input: `{"name":"app","retry":{` + test.json + `}}`, decode: json.Unmarshal},
			} {
				t.Run(codec.name, func(t *testing.T) {
					t.Parallel()

					var cfg Config
					if err := codec.decode([]byte(codec.input), &cfg); err != nil {
						t.Fatal(err)
					}

					err := cfg.Validate()
					if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), test.field) {
						t.Fatalf("expected %s validation error, got %v", test.field, err)
					}
				})
			}
		})
	}

	t.Run("programmatic subsecond delay", func(t *testing.T) {
		t.Parallel()

		var cfg Config
		if err := yaml.Unmarshal([]byte("name: app"), &cfg); err != nil {
			t.Fatal(err)
		}

		cfg.Retry.Delay = duration.Duration(500 * time.Millisecond)
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "Retry.Delay") {
			t.Fatalf("expected duration validation error, got %v", err)
		}
	})
}

func TestConfigRetryInvalidInput(t *testing.T) {
	t.Parallel()

	for _, input := range []struct {
		name string
		yaml string
		json string
	}{
		{name: "boolean shorthand", yaml: "retry: false", json: `"retry":false`},
		{name: "sequence", yaml: "retry: []", json: `"retry":[]`},
		{name: "fractional retries", yaml: "retry: {limit: 1.5}", json: `"retry":{"limit":1.5}`},
		{name: "invalid duration", yaml: "retry: {delay: soon}", json: `"retry":{"delay":"soon"}`},
		{name: "fractional seconds", yaml: "retry: {delay: 1.5}", json: `"retry":{"delay":1.5}`},
		{name: "subsecond duration", yaml: "retry: {delay: 500ms}", json: `"retry":{"delay":"500ms"}`},
	} {
		t.Run(input.name, func(t *testing.T) {
			t.Parallel()

			var cfg Config
			if err := yaml.Unmarshal([]byte(input.yaml), &cfg); err == nil {
				t.Fatal("expected invalid YAML retry config to fail")
			}

			if err := json.Unmarshal([]byte("{"+input.json+"}"), &cfg); err == nil {
				t.Fatal("expected invalid JSON retry config to fail")
			}
		})
	}
}

func TestConfigRetryHashCompatibility(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := yaml.Unmarshal([]byte("name: app"), &cfg); err != nil {
		t.Fatal(err)
	}

	// Reconstruct the pre-retry YAML representation rather than a hash that
	// would need to change whenever unrelated deployment defaults change.
	typ := reflect.TypeFor[Config]()

	fields := make([]reflect.StructField, 0, typ.NumField()-1)
	for field := range typ.Fields() {
		if field.Name != "Retry" {
			fields = append(fields, field)
		}
	}

	legacy := reflect.New(reflect.StructOf(fields)).Elem()
	for i := range legacy.NumField() {
		legacy.Field(i).Set(reflect.ValueOf(cfg).FieldByName(fields[i].Name))
	}

	encoded, err := yaml.Marshal(legacy.Interface())
	if err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf("%x", sha256.Sum256(encoded))

	for _, input := range []string{"name: app", "name: app\nretry: {}", "name: app\nretry: {delay: 0}"} {
		var decoded Config
		if err := yaml.Unmarshal([]byte(input), &decoded); err != nil {
			t.Fatal(err)
		}

		got, err := decoded.Hash()
		if err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Fatalf("zero retry changed legacy config hash: got %s, want %s", got, want)
		}
	}

	cfg.Retry.Limit = new(0)

	got, err := cfg.Hash()
	if err != nil {
		t.Fatal(err)
	}

	if got == want {
		t.Fatal("explicit retry limit 0 must change the hash")
	}
}

func TestConfigRetryCanonicalDuration(t *testing.T) {
	t.Parallel()

	var hashes []string

	for _, value := range []string{"300", "5m"} {
		var cfg Config
		if err := yaml.Unmarshal([]byte("name: app\nretry: {limit: 2, delay: "+value+"}"), &cfg); err != nil {
			t.Fatal(err)
		}

		hash, err := cfg.Hash()
		if err != nil {
			t.Fatal(err)
		}

		hashes = append(hashes, hash)

		for _, codec := range []struct {
			encode func(any) ([]byte, error)
			want   string
		}{
			{encode: yaml.Marshal, want: "delay: 300"},
			{encode: json.Marshal, want: `"delay":300`},
		} {
			encoded, err := codec.encode(cfg)
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(string(encoded), codec.want) {
				t.Fatalf("retry delay not encoded as seconds: %s", encoded)
			}
		}
	}

	if hashes[0] != hashes[1] {
		t.Fatal("equivalent delay forms must have the same config hash")
	}
}

func TestConfigRetryNestedOverrides(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		override string
		limit    int
		delay    time.Duration
	}{
		{name: "omitted", override: "environment: {LOG_LEVEL: debug}", limit: 2, delay: 5 * time.Minute},
		{name: "empty", override: "retry: {}", limit: 2, delay: 5 * time.Minute},
		{name: "zero retries", override: "retry: {limit: 0}", limit: 0, delay: 5 * time.Minute},
		{name: "new budget", override: "retry: {limit: 3}", limit: 3, delay: 5 * time.Minute},
		{name: "delay only", override: "retry: {delay: 1m30s}", limit: 2, delay: 90 * time.Second},
		{name: "zero delay", override: "retry: {delay: 0}", limit: 2, delay: 0},
		{name: "null fields inherit", override: "retry: {limit: null, delay: null}", limit: 2, delay: 5 * time.Minute},
		{name: "YAML merge zero", override: "base: &base {limit: 0}\nretry: {<<: *base, delay: 0}", limit: 0, delay: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var base Config
			if err := yaml.Unmarshal([]byte("name: app\nauto_discovery: true\nretry: {limit: 2, delay: 5m}"), &base); err != nil {
				t.Fatal(err)
			}

			overrides, err := getConfigFromYAMLBytes([]byte(test.override), ".doco-cd.yml", false)
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(test.override, "limit:") && overrides[0].Retry.Limit != nil {
				t.Fatal("omitted retry budget must remain unset without defaults")
			}

			configs, err := autoDiscoverDeployments(fstest.MapFS{
				"service/compose.yaml": {Data: []byte("services:\n  app:\n    image: nginx")},
				"service/.doco-cd.yml": {Data: []byte(test.override)},
			}, "retry-test", "", &base)
			if err != nil {
				t.Fatal(err)
			}

			if len(configs) != 1 {
				t.Fatalf("expected one discovered deployment, got %d", len(configs))
			}

			retry := configs[0].Retry
			if retry.Limit == nil || *retry.Limit != test.limit || retry.Delay.Duration() != test.delay {
				t.Fatalf("unexpected inherited retry policy: %+v", retry)
			}

			if *base.Retry.Limit != 2 || base.Retry.Delay.Duration() != 5*time.Minute {
				t.Fatal("nested override changed base policy")
			}
		})
	}
}

func TestConfigRetryMergeExplicitZero(t *testing.T) {
	t.Parallel()

	base := &Config{Retry: RetryConfig{Limit: new(2), Delay: duration.Duration(5 * time.Minute)}}
	override := &Config{Retry: RetryConfig{Limit: new(0)}}

	mergeConfig(base, override)

	if base.Retry.Limit == nil || *base.Retry.Limit != 0 || base.Retry.Delay.Duration() != 5*time.Minute {
		t.Fatalf("expected explicit zero with inherited delay: %+v", base.Retry)
	}
}

func TestResolveConfigsInlineRetryPolicy(t *testing.T) {
	t.Parallel()

	var inline []*Config
	if err := json.Unmarshal([]byte(`[{"name":"app","retry":{"limit":0,"delay":"5m"}}]`), &inline); err != nil {
		t.Fatal(err)
	}

	configs, err := ResolveConfigs(context.Background(), inline, "", "main", ".", ".", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 || configs[0].Retry.Limit == nil || *configs[0].Retry.Limit != 0 || configs[0].Retry.Delay.Duration() != 5*time.Minute {
		t.Fatalf("inline retry policy changed during resolution: %+v", configs)
	}

	*configs[0].Retry.Limit = 3
	if *inline[0].Retry.Limit != 0 {
		t.Fatal("resolved inline config shares retry budget pointer with input")
	}
}
