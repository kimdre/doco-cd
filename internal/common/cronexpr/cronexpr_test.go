package cronexpr

import (
	"errors"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    string
		wantErr bool
	}{
		{name: "5-field", spec: "*/5 * * * *"},
		{name: "descriptor", spec: "@daily"},
		{name: "every", spec: "@every 1h30m"},
		{name: "surrounding whitespace", spec: "  0 8 * * 1-5  "},
		{name: "timezone prefix", spec: "CRON_TZ=Europe/Berlin 0 8 * * *"},
		{name: "seconds field", spec: "*/5 * * * * *", wantErr: true},
		{name: "garbage", spec: "every minute", wantErr: true},
		{name: "empty", spec: "  ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.spec, time.UTC)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) err=%v wantErr=%v", tt.spec, err, tt.wantErr)
			}
		})
	}
}

func TestParse_EmptyExpression(t *testing.T) {
	t.Parallel()

	if _, err := Parse("", nil); !errors.Is(err, ErrEmptyExpression) {
		t.Fatalf("Parse(\"\") err=%v, want ErrEmptyExpression", err)
	}
}

func TestParse_UsesLocation(t *testing.T) {
	t.Parallel()

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation() failed: %v", err)
	}

	schedule, err := Parse("0 8 * * *", berlin)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	// 2026-03-29 is the DST switch in Europe/Berlin: 08:00 local is 06:00 UTC
	// afterwards and 07:00 UTC the day before.
	before := schedule.Next(time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 3, 28, 7, 0, 0, 0, time.UTC); !before.Equal(want) {
		t.Fatalf("Next() before DST = %s, want %s", before.UTC(), want)
	}

	after := schedule.Next(time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 3, 29, 6, 0, 0, 0, time.UTC); !after.Equal(want) {
		t.Fatalf("Next() after DST = %s, want %s", after.UTC(), want)
	}
}

func TestParse_ReturnsIndependentSchedules(t *testing.T) {
	t.Parallel()

	hourly, err := Parse("0 * * * *", time.UTC)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if _, err = Parse("30 2 * * *", time.UTC); err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	from := time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)
	if got, want := hourly.Next(from), time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next() = %s, want %s", got, want)
	}
}

func TestHasTimezonePrefix(t *testing.T) {
	t.Parallel()

	for spec, want := range map[string]bool{
		"TZ=UTC 0 * * * *":           true,
		" CRON_TZ=UTC 0 * * * *":     true,
		"0 * * * *":                  false,
		"@every 5m":                  false,
		"0 * * * * TZ=Europe/Berlin": false,
	} {
		if got := HasTimezonePrefix(spec); got != want {
			t.Errorf("HasTimezonePrefix(%q) = %v, want %v", spec, got, want)
		}
	}
}

func TestMinGap(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		spec string
		want time.Duration
	}{
		{spec: "@every 5s", want: 5 * time.Second},
		{spec: "*/5 * * * *", want: 5 * time.Minute},
		{spec: "0,1 * * * *", want: time.Minute},
		{spec: "@hourly", want: time.Hour},
	}

	for _, tt := range tests {
		schedule, err := Parse(tt.spec, time.UTC)
		if err != nil {
			t.Fatalf("Parse(%q) failed: %v", tt.spec, err)
		}

		if got := MinGap(schedule, from, 5); got != tt.want {
			t.Errorf("MinGap(%q) = %s, want %s", tt.spec, got, tt.want)
		}
	}
}
