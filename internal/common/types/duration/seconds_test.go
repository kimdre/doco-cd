package duration

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestParseSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   any
		want    time.Duration
		wantErr bool
	}{
		{name: "integer seconds", value: 300, want: 300 * time.Second},
		{name: "whole float seconds", value: float64(90), want: 90 * time.Second},
		{name: "negative seconds", value: int64(-10), want: -10 * time.Second},
		{name: "unsigned seconds", value: uint64(10), want: 10 * time.Second},
		{name: "numeric string seconds", value: "300", want: 300 * time.Second},
		{name: "duration string", value: "5m", want: 5 * time.Minute},
		{name: "trimmed duration string", value: " 5m ", want: 5 * time.Minute},
		{name: "composite duration string", value: "1m30s", want: 90 * time.Second},
		{name: "fractional unit with whole seconds", value: "0.5m", want: 30 * time.Second},
		{name: "milliseconds with whole seconds", value: "1000ms", want: time.Second},
		{name: "json number", value: json.Number("60"), want: 60 * time.Second},
		{name: "zero", value: "0s"},
		{name: "fractional numeric seconds", value: float64(1.5), wantErr: true},
		{name: "fractional duration", value: "500ms", wantErr: true},
		{name: "duration value", value: 500 * time.Millisecond, want: 500 * time.Millisecond},
		{name: "invalid duration", value: "abc", wantErr: true},
		{name: "empty duration", value: " ", wantErr: true},
		{name: "invalid type", value: true, wantErr: true},
		{name: "seconds out of duration range", value: int64(math.MaxInt64), wantErr: true},
		{name: "unsigned seconds out of range", value: uint64(math.MaxUint64), wantErr: true},
		{name: "maximum float seconds", value: float64(math.MaxInt64 / int64(time.Second)), want: time.Duration(math.MaxInt64/int64(time.Second)) * time.Second},
		{name: "minimum float seconds", value: float64(math.MinInt64 / int64(time.Second)), want: time.Duration(math.MinInt64/int64(time.Second)) * time.Second},
		{name: "float seconds overflow", value: float64(math.MaxInt64/int64(time.Second) + 1), wantErr: true},
		{name: "float seconds underflow", value: float64(math.MinInt64/int64(time.Second) - 1), wantErr: true},
		{name: "float int64 boundary", value: float64(math.MaxInt64), wantErr: true},
		{name: "not a number", value: math.NaN(), wantErr: true},
		{name: "positive infinity", value: math.Inf(1), wantErr: true},
		{name: "negative infinity", value: math.Inf(-1), wantErr: true},
		{name: "duration out of range", value: "999999999999999999999h", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSeconds("interval", test.value)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != test.want {
				t.Fatalf("ParseSeconds() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestWholeSeconds(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		value   time.Duration
		want    int64
		wantErr bool
	}{
		{name: "zero"},
		{name: "positive", value: 3 * time.Minute, want: 180},
		{name: "negative", value: -time.Second, want: -1},
		{name: "subsecond", value: 500 * time.Millisecond, wantErr: true},
		{name: "negative subsecond", value: -500 * time.Millisecond, wantErr: true},
		{name: "maximum duration", value: time.Duration(math.MaxInt64), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := WholeSeconds("timeout", test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("WholeSeconds() error = %v, wantErr = %v", err, test.wantErr)
			}

			if got != test.want {
				t.Fatalf("WholeSeconds() = %d, want %d", got, test.want)
			}
		})
	}
}
