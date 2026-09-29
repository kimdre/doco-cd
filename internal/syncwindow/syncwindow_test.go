package syncwindow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()

	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q) failed: %v", name, err)
	}

	return location
}

func mustPolicy(t *testing.T, windows ...Window) *Policy {
	t.Helper()

	policy, err := New(windows)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	return policy
}

var anyTarget = Target{Repository: "github.com/acme/app", Deployment: "app", Context: ""}

func TestParse(t *testing.T) {
	t.Parallel()

	policy, err := Parse(`
- name: business-hours
  kind: allow
  schedule: "0 8 * * 1-5"
  duration: 15h
  timezone: Europe/Berlin
  repositories: ["github.com/acme/*"]
  deployments: ["prod-*"]
  contexts: ["default"]
  manual_sync: true
- kind: DENY
  schedule: "@daily"
  duration: 30m
`)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	windows := policy.Windows()
	if len(windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(windows))
	}

	first := windows[0]
	if first.Name != "business-hours" || first.Kind != KindAllow || first.Duration != 15*time.Hour ||
		first.Location().String() != "Europe/Berlin" || !first.ManualSync {
		t.Fatalf("unexpected first window: %+v", first)
	}

	second := windows[1]
	if second.Name != "deny-2" || second.Kind != KindDeny || second.Location() != time.Local {
		t.Fatalf("unexpected second window: name=%q kind=%q location=%s", second.Name, second.Kind, second.Location())
	}
}

func TestParse_Empty(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "  \n", "[]", "# no windows\n"} {
		policy, err := Parse(input)
		if err != nil {
			t.Fatalf("Parse(%q) failed: %v", input, err)
		}

		if !policy.Empty() {
			t.Fatalf("Parse(%q) returned a non-empty policy", input)
		}
	}
}

func TestParse_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "not a list", input: "kind: allow", wantErr: "yaml:"},
		{name: "unknown field", input: `[{kind: allow, schedule: "@daily", duration: 1h, deployment: [app]}]`, wantErr: "deployment"},
		{name: "unknown kind", input: `[{kind: maybe, schedule: "@daily", duration: 1h}]`, wantErr: "kind must be"},
		{name: "missing kind", input: `[{schedule: "@daily", duration: 1h}]`, wantErr: "kind must be"},
		{name: "missing schedule", input: `[{kind: allow, duration: 1h}]`, wantErr: "schedule is required"},
		{name: "invalid schedule", input: `[{kind: allow, schedule: "whenever", duration: 1h}]`, wantErr: "invalid schedule"},
		{name: "seconds field", input: `[{kind: allow, schedule: "0 0 8 * * *", duration: 1h}]`, wantErr: "invalid schedule"},
		{name: "every descriptor", input: `[{kind: allow, schedule: "@every 1h", duration: 1h}]`, wantErr: "@every"},
		{name: "timezone prefix", input: `[{kind: allow, schedule: "CRON_TZ=UTC 0 8 * * *", duration: 1h}]`, wantErr: "use timezone instead"},
		{name: "missing duration", input: `[{kind: allow, schedule: "@daily"}]`, wantErr: "duration is required"},
		{name: "invalid duration", input: `[{kind: allow, schedule: "@daily", duration: forever}]`, wantErr: "invalid duration"},
		{name: "short duration", input: `[{kind: allow, schedule: "@daily", duration: 30s}]`, wantErr: "at least 1m0s"},
		{name: "invalid timezone", input: `[{kind: allow, schedule: "@daily", duration: 1h, timezone: Mars/Olympus}]`, wantErr: "invalid timezone"},
		{name: "empty pattern", input: `[{kind: allow, schedule: "@daily", duration: 1h, deployments: [""]}]`, wantErr: "empty patterns"},
		{
			name:    "duplicate names",
			input:   `[{name: a, kind: allow, schedule: "@daily", duration: 1h}, {name: A, kind: deny, schedule: "@daily", duration: 1h}]`,
			wantErr: "duplicate name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.input)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Parse() err=%v, want ErrInvalidConfig", err)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse() err=%q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestWindowActive(t *testing.T) {
	t.Parallel()

	policy := mustPolicy(t, Window{Kind: KindAllow, Schedule: "0 8 * * *", Duration: 2 * time.Hour, Timezone: "UTC"})
	window := policy.windows[0]

	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{name: "before start", at: time.Date(2026, 1, 5, 7, 59, 59, 0, time.UTC), want: false},
		{name: "at start", at: time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC), want: true},
		{name: "inside", at: time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC), want: true},
		{name: "just before end", at: time.Date(2026, 1, 5, 9, 59, 59, 0, time.UTC), want: true},
		{name: "at end", at: time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC), want: false},
		{name: "after end", at: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC), want: false},
	}

	for _, tt := range tests {
		if got := window.Active(tt.at); got != tt.want {
			t.Errorf("%s: Active(%s) = %v, want %v", tt.name, tt.at, got, tt.want)
		}
	}
}

func TestWindowActive_OverlappingOccurrences(t *testing.T) {
	t.Parallel()

	// Every hour for 90 minutes: occurrences overlap, so the window never
	// closes between 00:00 and the end of the last occurrence.
	policy := mustPolicy(t, Window{Kind: KindDeny, Schedule: "0 * * * *", Duration: 90 * time.Minute, Timezone: "UTC"})
	window := policy.windows[0]

	at := time.Date(2026, 1, 5, 10, 45, 0, 0, time.UTC)

	start, active := window.activeSince(at)
	if !active {
		t.Fatal("expected overlapping window to be active")
	}

	if want := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("activeSince() start = %s, want latest occurrence %s", start, want)
	}
}

func TestWindowActive_DaylightSavingTime(t *testing.T) {
	t.Parallel()

	berlin := mustLocation(t, "Europe/Berlin")
	policy := mustPolicy(t, Window{Kind: KindAllow, Schedule: "0 8 * * 1-5", Duration: 15 * time.Hour, Timezone: "Europe/Berlin"})
	window := policy.windows[0]

	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		// Friday 2026-03-27 (CET, UTC+1) and Monday 2026-03-30 (CEST, UTC+2).
		{name: "friday 08:00 CET", at: time.Date(2026, 3, 27, 8, 0, 0, 0, berlin), want: true},
		{name: "friday 22:59 CET", at: time.Date(2026, 3, 27, 22, 59, 0, 0, berlin), want: true},
		{name: "friday 23:00 CET", at: time.Date(2026, 3, 27, 23, 0, 0, 0, berlin), want: false},
		{name: "sunday at DST switch", at: time.Date(2026, 3, 29, 12, 0, 0, 0, berlin), want: false},
		{name: "monday 07:59 CEST", at: time.Date(2026, 3, 30, 7, 59, 0, 0, berlin), want: false},
		{name: "monday 08:00 CEST", at: time.Date(2026, 3, 30, 8, 0, 0, 0, berlin), want: true},
		{name: "monday 08:00 CEST in UTC", at: time.Date(2026, 3, 30, 6, 0, 0, 0, time.UTC), want: true},
		{name: "monday 07:00 UTC is 09:00 CEST", at: time.Date(2026, 3, 30, 7, 0, 0, 0, time.UTC), want: true},
		// Monday 2026-10-26 is after the switch back to CET.
		{name: "monday 08:00 CET after October switch", at: time.Date(2026, 10, 26, 7, 0, 0, 0, time.UTC), want: true},
		{name: "monday 07:59 CET after October switch", at: time.Date(2026, 10, 26, 6, 59, 0, 0, time.UTC), want: false},
	}

	for _, tt := range tests {
		if got := window.Active(tt.at); got != tt.want {
			t.Errorf("%s: Active(%s) = %v, want %v", tt.name, tt.at, got, tt.want)
		}
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	businessHours := Window{Name: "business-hours", Kind: KindAllow, Schedule: "0 8 * * 1-5", Duration: 10 * time.Hour, Timezone: "UTC"}
	lunchFreeze := Window{Name: "lunch-freeze", Kind: KindDeny, Schedule: "0 12 * * *", Duration: time.Hour, Timezone: "UTC"}

	monday0900 := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	monday1230 := time.Date(2026, 1, 5, 12, 30, 0, 0, time.UTC)
	monday2000 := time.Date(2026, 1, 5, 20, 0, 0, 0, time.UTC)
	saturday1000 := time.Date(2026, 1, 10, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		windows      []Window
		at           time.Time
		origin       Origin
		wantAllowed  bool
		wantOverride bool
		wantWindows  []string
		wantNextOpen time.Time
	}{
		{name: "no windows", at: monday2000, origin: OriginAutomatic, wantAllowed: true},
		{name: "inside allow window", windows: []Window{businessHours}, at: monday0900, origin: OriginAutomatic, wantAllowed: true},
		{
			name: "outside allow window", windows: []Window{businessHours}, at: monday2000, origin: OriginAutomatic,
			wantWindows: []string{"business-hours"}, wantNextOpen: time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC),
		},
		{
			name: "weekend opens monday", windows: []Window{businessHours}, at: saturday1000, origin: OriginAutomatic,
			wantWindows: []string{"business-hours"}, wantNextOpen: time.Date(2026, 1, 12, 8, 0, 0, 0, time.UTC),
		},
		{
			name: "deny wins over active allow", windows: []Window{businessHours, lunchFreeze}, at: monday1230, origin: OriginAutomatic,
			wantWindows: []string{"lunch-freeze"}, wantNextOpen: time.Date(2026, 1, 5, 13, 0, 0, 0, time.UTC),
		},
		{
			name: "deny only", windows: []Window{lunchFreeze}, at: monday1230, origin: OriginAutomatic,
			wantWindows: []string{"lunch-freeze"}, wantNextOpen: time.Date(2026, 1, 5, 13, 0, 0, 0, time.UTC),
		},
		{name: "inactive deny only", windows: []Window{lunchFreeze}, at: monday0900, origin: OriginAutomatic, wantAllowed: true},
		{name: "reconciliation bypasses", windows: []Window{businessHours, lunchFreeze}, at: monday1230, origin: OriginReconciliation, wantAllowed: true},
		{
			name: "manual without manual_sync", windows: []Window{businessHours}, at: monday2000, origin: OriginManual,
			wantWindows: []string{"business-hours"}, wantNextOpen: time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC),
		},
		{
			name: "manual with manual_sync", windows: []Window{withManualSync(businessHours)}, at: monday2000, origin: OriginManual,
			wantAllowed: true, wantOverride: true, wantWindows: []string{"business-hours"},
		},
		{
			name: "automatic ignores manual_sync", windows: []Window{withManualSync(businessHours)}, at: monday2000, origin: OriginAutomatic,
			wantWindows: []string{"business-hours"}, wantNextOpen: time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC),
		},
		{
			name:    "manual needs manual_sync on every blocking window",
			windows: []Window{withManualSync(lunchFreeze), {Name: "noon-deny", Kind: KindDeny, Schedule: "30 12 * * *", Duration: time.Hour, Timezone: "UTC"}},
			at:      monday1230, origin: OriginManual,
			wantWindows: []string{"lunch-freeze", "noon-deny"}, wantNextOpen: time.Date(2026, 1, 5, 13, 30, 0, 0, time.UTC),
		},
		{
			name:    "manual needs manual_sync on inactive allow windows during an active deny",
			windows: []Window{businessHours, withManualSync(Window{Name: "night-freeze", Kind: KindDeny, Schedule: "0 19 * * *", Duration: 2 * time.Hour, Timezone: "UTC"})},
			at:      monday2000, origin: OriginManual,
			wantWindows: []string{"night-freeze", "business-hours"}, wantNextOpen: time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC),
		},
		{
			name:    "manual bypasses active deny and inactive allow with manual_sync",
			windows: []Window{withManualSync(businessHours), withManualSync(Window{Name: "night-freeze", Kind: KindDeny, Schedule: "0 19 * * *", Duration: 2 * time.Hour, Timezone: "UTC"})},
			at:      monday2000, origin: OriginManual, wantAllowed: true, wantOverride: true,
			wantWindows: []string{"night-freeze", "business-hours"},
		},
		{
			name: "deny inside allow waits for both", windows: []Window{businessHours, {Name: "late", Kind: KindDeny, Schedule: "0 17 * * *", Duration: 2 * time.Hour, Timezone: "UTC"}},
			at: time.Date(2026, 1, 5, 17, 30, 0, 0, time.UTC), origin: OriginAutomatic,
			// The deny ends at 19:00, after the allow window closed at 18:00.
			wantWindows: []string{"late"}, wantNextOpen: time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			decision := mustPolicy(t, tt.windows...).Evaluate(tt.at, anyTarget, tt.origin)

			if decision.Allowed != tt.wantAllowed || decision.ManualOverride != tt.wantOverride {
				t.Fatalf("Evaluate() allowed=%v override=%v, want allowed=%v override=%v",
					decision.Allowed, decision.ManualOverride, tt.wantAllowed, tt.wantOverride)
			}

			if strings.Join(decision.Windows, ",") != strings.Join(tt.wantWindows, ",") {
				t.Fatalf("Evaluate() windows=%v, want %v", decision.Windows, tt.wantWindows)
			}

			if !decision.NextOpen.Equal(tt.wantNextOpen) {
				t.Fatalf("Evaluate() next open=%s, want %s", decision.NextOpen, tt.wantNextOpen)
			}
		})
	}
}

func withManualSync(w Window) Window {
	w.ManualSync = true

	return w
}

func TestEvaluate_Selectors(t *testing.T) {
	t.Parallel()

	policy := mustPolicy(t, Window{
		Kind:         KindDeny,
		Schedule:     "@daily",
		Duration:     24 * time.Hour,
		Timezone:     "UTC",
		Repositories: []string{"github.com/acme/*"},
		Deployments:  []string{"prod-*", "db"},
		Contexts:     []string{"default", "edge-?"},
	})

	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		target      Target
		wantBlocked bool
	}{
		{name: "all match with default context", target: Target{Repository: "github.com/acme/app", Deployment: "prod-web"}, wantBlocked: true},
		{name: "glob crosses slashes", target: Target{Repository: "github.com/acme/group/app", Deployment: "db"}, wantBlocked: true},
		{name: "case insensitive", target: Target{Repository: "GitHub.com/ACME/app", Deployment: "PROD-web", Context: "Default"}, wantBlocked: true},
		{name: "question mark context", target: Target{Repository: "github.com/acme/app", Deployment: "db", Context: "edge-1"}, wantBlocked: true},
		{name: "config repository matches", target: Target{Repository: "github.com/other/app", ConfigRepository: "github.com/acme/infra", Deployment: "db"}, wantBlocked: true},
		{name: "neither repository matches", target: Target{Repository: "github.com/other/app", ConfigRepository: "github.com/other/infra", Deployment: "db"}},
		{name: "other repository", target: Target{Repository: "github.com/other/app", Deployment: "prod-web"}},
		{name: "other deployment", target: Target{Repository: "github.com/acme/app", Deployment: "staging-web"}},
		{name: "other context", target: Target{Repository: "github.com/acme/app", Deployment: "db", Context: "edge-10"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			decision := policy.Evaluate(at, tt.target, OriginAutomatic)
			if decision.Allowed == tt.wantBlocked {
				t.Fatalf("Evaluate(%+v) allowed=%v, want blocked=%v", tt.target, decision.Allowed, tt.wantBlocked)
			}
		})
	}
}

func TestEvaluate_NilPolicy(t *testing.T) {
	t.Parallel()

	var policy *Policy
	if !policy.Evaluate(time.Now(), anyTarget, OriginAutomatic).Allowed {
		t.Fatal("nil policy must allow deployments")
	}

	if got := policy.Statuses(time.Now()); len(got) != 0 {
		t.Fatalf("nil policy statuses = %v, want none", got)
	}
}

func TestDecisionMessage(t *testing.T) {
	t.Parallel()

	decision := Decision{Windows: []string{"a", "b"}, NextOpen: time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)}
	if got, want := decision.Message(), "deferred by sync window a, b, next opening at 2026-01-06T08:00:00Z"; got != want {
		t.Fatalf("Message() = %q, want %q", got, want)
	}

	decision.NextOpen = time.Time{}
	if got, want := decision.Message(), "deferred by sync window a, b"; got != want {
		t.Fatalf("Message() = %q, want %q", got, want)
	}
}

func TestStatuses(t *testing.T) {
	t.Parallel()

	policy := mustPolicy(t,
		Window{Name: "business-hours", Kind: KindAllow, Schedule: "0 8 * * *", Duration: 10 * time.Hour, Timezone: "UTC"},
		Window{Name: "night", Kind: KindDeny, Schedule: "0 22 * * *", Duration: 8 * time.Hour, Timezone: "UTC"},
	)

	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	statuses := policy.Statuses(at)

	if len(statuses) != 2 {
		t.Fatalf("got %d statuses, want 2", len(statuses))
	}

	allow := statuses[0]
	if !allow.Active || allow.ActiveSince == nil || allow.ActiveUntil == nil ||
		!allow.ActiveSince.Equal(time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)) ||
		!allow.ActiveUntil.Equal(time.Date(2026, 1, 5, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected allow status: %+v", allow)
	}

	deny := statuses[1]
	if deny.Active || deny.ActiveSince != nil || deny.NextStart == nil ||
		!deny.NextStart.Equal(time.Date(2026, 1, 5, 22, 0, 0, 0, time.UTC)) || deny.Timezone != "UTC" || deny.Duration != "8h0m0s" {
		t.Fatalf("unexpected deny status: %+v", deny)
	}
}

func TestReport(t *testing.T) {
	t.Parallel()

	policy := mustPolicy(t,
		Window{Name: "freeze", Kind: KindDeny, Schedule: "0 9 * * *", Duration: 2 * time.Hour, Timezone: "UTC", ManualSync: true},
	)
	at := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)

	report := policy.Report(at, nil, OriginAutomatic)
	if !report.Time.Equal(at) || len(report.Windows) != 1 || report.Decision != nil {
		t.Fatalf("Report() without target = %+v", report)
	}

	report = policy.Report(at, &anyTarget, OriginAutomatic)
	if report.Decision == nil || report.Decision.Allowed ||
		!report.Decision.NextOpen.Equal(time.Date(2026, 1, 5, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("automatic decision = %+v", report.Decision)
	}

	report = policy.Report(at, &anyTarget, OriginManual)
	if report.Decision == nil || !report.Decision.Allowed || !report.Decision.ManualOverride {
		t.Fatalf("manual decision = %+v", report.Decision)
	}

	// An allowed decision serializes an empty window list and no next_open.
	report = policy.Report(at.Add(2*time.Hour), &anyTarget, OriginAutomatic)

	data, err := json.Marshal(report.Decision)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	if want := `{"allowed":true,"manual_override":false,"windows":[]}`; string(data) != want {
		t.Fatalf("decision JSON = %s, want %s", data, want)
	}

	var nilPolicy *Policy
	if report := nilPolicy.Report(at, &anyTarget, OriginAutomatic); len(report.Windows) != 0 || !report.Decision.Allowed {
		t.Fatalf("nil policy report = %+v", report)
	}
}
