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
		{name: "zero retries", config: RetryConfig{MaxRetries: new(0)}},
		{name: "finite retries", config: RetryConfig{MaxRetries: new(2)}},
		{name: "cooldown only", config: RetryConfig{Cooldown: duration.Duration(time.Minute)}},
		{name: "negative retries", config: RetryConfig{MaxRetries: new(-1)}, field: "MaxRetries"},
		{name: "negative cooldown", config: RetryConfig{Cooldown: duration.Duration(-time.Minute)}, field: "Cooldown"},
		{name: "negative limits", config: RetryConfig{MaxRetries: new(-1), Cooldown: duration.Duration(-time.Minute)}, field: "MaxRetries"},
		{name: "subsecond cooldown", config: RetryConfig{Cooldown: duration.Duration(500 * time.Millisecond)}, field: "Cooldown"},
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
		{name: "no retries", config: RetryConfig{MaxRetries: new(0)}, want: true},
		{name: "finite retries", config: RetryConfig{MaxRetries: new(2)}, want: true},
		{name: "cooldown only", config: RetryConfig{Cooldown: duration.Duration(time.Minute)}, want: true},
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
		name       string
		yaml       string
		json       string
		maxRetries *int
		cooldown   time.Duration
	}{
		{name: "omitted"},
		{name: "empty object", yaml: "retry: {}", json: `"retry":{}`},
		{name: "null object", yaml: "retry: null", json: `"retry":null`},
		{name: "explicit zero", yaml: "retry: {max_retries: 0, cooldown: 0}", json: `"retry":{"max_retries":0,"cooldown":0}`, maxRetries: new(0)},
		{name: "finite retries", yaml: "retry: {max_retries: 2}", json: `"retry":{"max_retries":2}`, maxRetries: new(2)},
		{name: "cooldown only", yaml: "retry: {cooldown: 5m}", json: `"retry":{"cooldown":"5m"}`, cooldown: 5 * time.Minute},
		{name: "numeric seconds", yaml: "retry: {max_retries: 2, cooldown: 90}", json: `"retry":{"max_retries":2,"cooldown":90}`, maxRetries: new(2), cooldown: 90 * time.Second},
		{name: "duration string", yaml: "retry: {max_retries: 2, cooldown: 1m30s}", json: `"retry":{"max_retries":2,"cooldown":"1m30s"}`, maxRetries: new(2), cooldown: 90 * time.Second},
		{name: "null fields", yaml: "retry: {max_retries: null, cooldown: null}", json: `"retry":{"max_retries":null,"cooldown":null}`},
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

					if !reflect.DeepEqual(cfg.Retry.MaxRetries, test.maxRetries) || cfg.Retry.Cooldown.Duration() != test.cooldown {
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

	if cfg.Retry.MaxRetries != nil || cfg.Retry.Cooldown != 0 || cfg.Retry.Configured() {
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
		{name: "negative retries", yaml: "max_retries: -1", json: `"max_retries":-1`, field: "Retry.MaxRetries"},
		{name: "negative numeric cooldown", yaml: "cooldown: -1", json: `"cooldown":-1`, field: "Retry.Cooldown"},
		{name: "negative duration cooldown", yaml: "cooldown: -5m", json: `"cooldown":"-5m"`, field: "Retry.Cooldown"},
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

	t.Run("programmatic subsecond cooldown", func(t *testing.T) {
		t.Parallel()

		var cfg Config
		if err := yaml.Unmarshal([]byte("name: app"), &cfg); err != nil {
			t.Fatal(err)
		}

		cfg.Retry.Cooldown = duration.Duration(500 * time.Millisecond)
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "Retry.Cooldown") {
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
		{name: "fractional retries", yaml: "retry: {max_retries: 1.5}", json: `"retry":{"max_retries":1.5}`},
		{name: "invalid duration", yaml: "retry: {cooldown: soon}", json: `"retry":{"cooldown":"soon"}`},
		{name: "fractional seconds", yaml: "retry: {cooldown: 1.5}", json: `"retry":{"cooldown":1.5}`},
		{name: "subsecond duration", yaml: "retry: {cooldown: 500ms}", json: `"retry":{"cooldown":"500ms"}`},
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

	for _, input := range []string{"name: app", "name: app\nretry: {}", "name: app\nretry: {cooldown: 0}"} {
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

	cfg.Retry.MaxRetries = new(0)

	got, err := cfg.Hash()
	if err != nil {
		t.Fatal(err)
	}

	if got == want {
		t.Fatal("explicit max_retries: 0 must change the hash")
	}
}

func TestConfigRetryCanonicalDuration(t *testing.T) {
	t.Parallel()

	var hashes []string

	for _, value := range []string{"300", "5m"} {
		var cfg Config
		if err := yaml.Unmarshal([]byte("name: app\nretry: {max_retries: 2, cooldown: "+value+"}"), &cfg); err != nil {
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
			{encode: yaml.Marshal, want: "cooldown: 300"},
			{encode: json.Marshal, want: `"cooldown":300`},
		} {
			encoded, err := codec.encode(cfg)
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(string(encoded), codec.want) {
				t.Fatalf("retry cooldown not encoded as seconds: %s", encoded)
			}
		}
	}

	if hashes[0] != hashes[1] {
		t.Fatal("equivalent cooldown forms must have the same config hash")
	}
}

func TestConfigRetryNestedOverrides(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		override   string
		maxRetries int
		cooldown   time.Duration
	}{
		{name: "omitted", override: "environment: {LOG_LEVEL: debug}", maxRetries: 2, cooldown: 5 * time.Minute},
		{name: "empty", override: "retry: {}", maxRetries: 2, cooldown: 5 * time.Minute},
		{name: "zero retries", override: "retry: {max_retries: 0}", maxRetries: 0, cooldown: 5 * time.Minute},
		{name: "new budget", override: "retry: {max_retries: 3}", maxRetries: 3, cooldown: 5 * time.Minute},
		{name: "cooldown only", override: "retry: {cooldown: 1m30s}", maxRetries: 2, cooldown: 90 * time.Second},
		{name: "zero cooldown", override: "retry: {cooldown: 0}", maxRetries: 2, cooldown: 0},
		{name: "null fields inherit", override: "retry: {max_retries: null, cooldown: null}", maxRetries: 2, cooldown: 5 * time.Minute},
		{name: "YAML merge zero", override: "base: &base {max_retries: 0}\nretry: {<<: *base, cooldown: 0}", maxRetries: 0, cooldown: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var base Config
			if err := yaml.Unmarshal([]byte("name: app\nauto_discovery: true\nretry: {max_retries: 2, cooldown: 5m}"), &base); err != nil {
				t.Fatal(err)
			}

			overrides, err := getConfigFromYAMLBytes([]byte(test.override), ".doco-cd.yml", false)
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(test.override, "max_retries:") && overrides[0].Retry.MaxRetries != nil {
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
			if retry.MaxRetries == nil || *retry.MaxRetries != test.maxRetries || retry.Cooldown.Duration() != test.cooldown {
				t.Fatalf("unexpected inherited retry policy: %+v", retry)
			}

			if *base.Retry.MaxRetries != 2 || base.Retry.Cooldown.Duration() != 5*time.Minute {
				t.Fatal("nested override changed base policy")
			}
		})
	}
}

func TestConfigRetryMergeExplicitZero(t *testing.T) {
	t.Parallel()

	base := &Config{Retry: RetryConfig{MaxRetries: new(2), Cooldown: duration.Duration(5 * time.Minute)}}
	override := &Config{Retry: RetryConfig{MaxRetries: new(0)}}

	mergeConfig(base, override)

	if base.Retry.MaxRetries == nil || *base.Retry.MaxRetries != 0 || base.Retry.Cooldown.Duration() != 5*time.Minute {
		t.Fatalf("expected explicit zero with inherited cooldown: %+v", base.Retry)
	}
}

func TestResolveConfigsInlineRetryPolicy(t *testing.T) {
	t.Parallel()

	var inline []*Config
	if err := json.Unmarshal([]byte(`[{"name":"app","retry":{"max_retries":0,"cooldown":"5m"}}]`), &inline); err != nil {
		t.Fatal(err)
	}

	configs, err := ResolveConfigs(context.Background(), inline, "", "main", ".", ".", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 || configs[0].Retry.MaxRetries == nil || *configs[0].Retry.MaxRetries != 0 || configs[0].Retry.Cooldown.Duration() != 5*time.Minute {
		t.Fatalf("inline retry policy changed during resolution: %+v", configs)
	}

	*configs[0].Retry.MaxRetries = 3
	if *inline[0].Retry.MaxRetries != 0 {
		t.Fatal("resolved inline config shares retry budget pointer with input")
	}
}
