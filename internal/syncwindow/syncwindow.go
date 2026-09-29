// Package syncwindow implements sync windows: operator-defined,
// timezone-aware allow and deny windows that decide whether a deployment
// may change a stack at a given time.
//
// A window is active from each occurrence of its cron schedule for its
// duration. Only windows whose selectors match a deployment target are
// considered:
//
//   - an active matching deny window blocks the deployment,
//   - matching allow windows that are all inactive block the deployment,
//   - otherwise the deployment is allowed.
//
// Deny windows win over allow windows. Manual (API/MCP) deployments bypass a
// block only if every blocking window has manual_sync enabled, and
// reconciliation of an already deployed revision is never blocked.
package syncwindow

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/cronexpr"
)

// Kind is the type of window.
type Kind string

const (
	// KindAllow permits deployments while active and blocks them otherwise.
	KindAllow Kind = "allow"
	// KindDeny blocks deployments while active.
	KindDeny Kind = "deny"
)

// Origin identifies what started a deployment, which decides how windows apply to it.
type Origin string

const (
	// OriginAutomatic covers webhooks, polls and the local repository watcher.
	OriginAutomatic Origin = "automatic"
	// OriginManual covers deployments requested through the REST API or MCP.
	OriginManual Origin = "manual"
	// OriginReconciliation covers restoring the already deployed revision
	// after drift, for example when a container died. It is never blocked;
	// a reconciliation that would deploy another revision must be evaluated
	// as OriginAutomatic instead.
	OriginReconciliation Origin = "reconciliation"
)

// MinDuration is the shortest allowed window duration.
const MinDuration = time.Minute

const (
	// maxOccurrenceSteps bounds the walk over overlapping occurrences when
	// finding the latest start of an active window.
	maxOccurrenceSteps = 10000
	// maxNextOpenSteps and nextOpenHorizon bound the search for the next time
	// a blocked target may deploy again.
	maxNextOpenSteps = 512
	nextOpenHorizon  = 366 * 24 * time.Hour
)

var (
	// ErrInvalidConfig is returned for any invalid window definition.
	ErrInvalidConfig = errors.New("invalid sync window configuration")
	// ErrBothConfigSet is returned when both SYNC_WINDOWS and SYNC_WINDOWS_FILE are set.
	ErrBothConfigSet = errors.New("both SYNC_WINDOWS and SYNC_WINDOWS_FILE are set, please use one or the other")
)

// Window is a single sync window definition.
type Window struct {
	Name         string        // Name identifies the window in logs, metrics and the API. Defaults to "<kind>-<index>".
	Kind         Kind          // Kind is either allow or deny.
	Schedule     string        // Schedule is the 5-field cron expression or descriptor at which the window opens.
	Duration     time.Duration // Duration is how long the window stays active after each occurrence.
	Timezone     string        // Timezone is the IANA timezone the schedule is evaluated in. Empty means the doco-cd timezone (TZ).
	Repositories []string      // Repositories are glob patterns matched against the repository/artifact name. Empty matches all.
	Deployments  []string      // Deployments are glob patterns matched against the deployment (stack/project) name. Empty matches all.
	Contexts     []string      // Contexts are glob patterns matched against the Docker context name ("default" for the default context). Empty matches all.
	ManualSync   bool          // ManualSync lets manual (API/MCP) deployments bypass this window.

	schedule     gocron.Cron
	location     *time.Location
	repositories []*regexp.Regexp
	deployments  []*regexp.Regexp
	contexts     []*regexp.Regexp
}

// rawWindow is the YAML/JSON representation of a Window.
type rawWindow struct {
	Name         string   `yaml:"name" json:"name"`
	Kind         string   `yaml:"kind" json:"kind"`
	Schedule     string   `yaml:"schedule" json:"schedule"`
	Duration     string   `yaml:"duration" json:"duration"`
	Timezone     string   `yaml:"timezone" json:"timezone"`
	Repositories []string `yaml:"repositories" json:"repositories"`
	Deployments  []string `yaml:"deployments" json:"deployments"`
	Contexts     []string `yaml:"contexts" json:"contexts"`
	ManualSync   bool     `yaml:"manual_sync" json:"manual_sync"`
}

// Target identifies the stack a deployment would change.
type Target struct {
	Repository string // Repository is the repository/artifact name the stack is deployed from, e.g. "github.com/acme/app".
	// ConfigRepository is the repository containing the deploy config, if it
	// differs from Repository (deploy configs with repository_url). Repository
	// selectors match if either name matches.
	ConfigRepository string
	Deployment       string // Deployment is the deployment (stack/project) name.
	Context          string // Context is the Docker context name. Empty means "default".
}

// Decision is the result of evaluating a target against the policy.
type Decision struct {
	Allowed bool `json:"allowed"`
	// ManualOverride is true when a manual deployment is allowed only
	// because every blocking window has manual_sync enabled.
	ManualOverride bool `json:"manual_override"`
	// Windows lists the names of the windows that block (or, with
	// ManualOverride, would block) the deployment.
	Windows []string `json:"windows"`
	// NextOpen is the earliest time the target may deploy again. It is zero
	// when unknown or when the deployment is allowed.
	NextOpen time.Time `json:"next_open,omitzero"`
}

// Message returns a short human-readable description of a blocking decision.
func (d Decision) Message() string {
	msg := "deferred by sync window " + strings.Join(d.Windows, ", ")
	if !d.NextOpen.IsZero() {
		msg += ", next opening at " + d.NextOpen.Format(time.RFC3339)
	}

	return msg
}

// Policy is a validated, immutable set of windows. A nil Policy allows everything.
type Policy struct {
	windows []*Window
}

// Parse parses and validates a YAML (or JSON) list of windows. Empty input
// returns an empty policy.
func Parse(data string) (*Policy, error) {
	if strings.TrimSpace(data) == "" {
		return &Policy{}, nil
	}

	// Unknown keys are rejected, because a typo in a selector (e.g.
	// "deployment" instead of "deployments") would silently widen a window
	// to all deployments.
	decoder := yaml.NewDecoder(strings.NewReader(data))
	decoder.KnownFields(true)

	var raws []rawWindow
	if err := decoder.Decode(&raws); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	windows := make([]Window, 0, len(raws))

	for i, raw := range raws {
		window, err := raw.toWindow()
		if err != nil {
			return nil, fmt.Errorf("%w: window %d: %v", ErrInvalidConfig, i+1, err)
		}

		windows = append(windows, window)
	}

	return New(windows)
}

// New validates windows and returns a policy for them.
func New(windows []Window) (*Policy, error) {
	policy := &Policy{windows: make([]*Window, 0, len(windows))}
	names := make(map[string]struct{}, len(windows))

	for i := range windows {
		window := windows[i]

		if err := window.compile(i); err != nil {
			return nil, fmt.Errorf("%w: window %d: %v", ErrInvalidConfig, i+1, err)
		}

		key := strings.ToLower(window.Name)
		if _, exists := names[key]; exists {
			return nil, fmt.Errorf("%w: window %d: duplicate name %q", ErrInvalidConfig, i+1, window.Name)
		}

		names[key] = struct{}{}

		policy.windows = append(policy.windows, &window)
	}

	return policy, nil
}

func (r rawWindow) toWindow() (Window, error) {
	window := Window{
		Name:         r.Name,
		Kind:         Kind(r.Kind),
		Schedule:     r.Schedule,
		Timezone:     r.Timezone,
		Repositories: r.Repositories,
		Deployments:  r.Deployments,
		Contexts:     r.Contexts,
		ManualSync:   r.ManualSync,
	}

	duration := strings.TrimSpace(r.Duration)
	if duration == "" {
		return window, errors.New("duration is required")
	}

	parsed, err := time.ParseDuration(duration)
	if err != nil {
		return window, fmt.Errorf("invalid duration %q: %v", r.Duration, err)
	}

	window.Duration = parsed

	return window, nil
}

func (w *Window) compile(index int) error {
	w.Kind = Kind(strings.ToLower(strings.TrimSpace(string(w.Kind))))
	if w.Kind != KindAllow && w.Kind != KindDeny {
		return fmt.Errorf("kind must be %q or %q, got %q", KindAllow, KindDeny, w.Kind)
	}

	w.Name = strings.TrimSpace(w.Name)
	if w.Name == "" {
		w.Name = fmt.Sprintf("%s-%d", w.Kind, index+1)
	}

	w.Schedule = strings.TrimSpace(w.Schedule)
	if w.Schedule == "" {
		return errors.New("schedule is required")
	}

	if cronexpr.HasTimezonePrefix(w.Schedule) {
		return errors.New("schedule must not contain a TZ= or CRON_TZ= prefix, use timezone instead")
	}

	// @every schedules are relative to the time they are evaluated at, so they
	// don't describe fixed opening times.
	if strings.HasPrefix(w.Schedule, "@every") {
		return errors.New("@every schedules are not supported for sync windows")
	}

	if w.Duration < MinDuration {
		return fmt.Errorf("duration must be at least %s", MinDuration)
	}

	w.Timezone = strings.TrimSpace(w.Timezone)

	w.location = time.Local
	if w.Timezone != "" {
		location, err := time.LoadLocation(w.Timezone)
		if err != nil {
			return fmt.Errorf("invalid timezone %q: %v", w.Timezone, err)
		}

		w.location = location
	}

	schedule, err := cronexpr.Parse(w.Schedule, w.location)
	if err != nil {
		return fmt.Errorf("invalid schedule %q: %v", w.Schedule, err)
	}

	w.schedule = schedule

	if w.repositories, err = compilePatterns("repositories", w.Repositories); err != nil {
		return err
	}

	if w.deployments, err = compilePatterns("deployments", w.Deployments); err != nil {
		return err
	}

	if w.contexts, err = compilePatterns("contexts", w.Contexts); err != nil {
		return err
	}

	return nil
}

// compilePatterns turns glob patterns into case-insensitive regular
// expressions. "*" matches any sequence of characters, including "/", and "?"
// matches a single character.
func compilePatterns(field string, patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))

	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, fmt.Errorf("%s must not contain empty patterns", field)
		}

		var expr strings.Builder

		expr.WriteString("(?i)^")

		for _, r := range pattern {
			switch r {
			case '*':
				expr.WriteString(".*")
			case '?':
				expr.WriteString(".")
			default:
				expr.WriteString(regexp.QuoteMeta(string(r)))
			}
		}

		expr.WriteString("$")

		re, err := regexp.Compile(expr.String())
		if err != nil {
			return nil, fmt.Errorf("invalid %s pattern %q: %v", field, pattern, err)
		}

		compiled = append(compiled, re)
	}

	return compiled, nil
}

func matchesAny(patterns []*regexp.Regexp, value string) bool {
	if len(patterns) == 0 {
		return true
	}

	for _, pattern := range patterns {
		if pattern.MatchString(value) {
			return true
		}
	}

	return false
}

func (w *Window) matches(target Target) bool {
	contextName := strings.TrimSpace(target.Context)
	if contextName == "" {
		contextName = "default"
	}

	repository := matchesAny(w.repositories, strings.TrimSpace(target.Repository))
	if !repository && strings.TrimSpace(target.ConfigRepository) != "" {
		repository = matchesAny(w.repositories, strings.TrimSpace(target.ConfigRepository))
	}

	return repository &&
		matchesAny(w.deployments, strings.TrimSpace(target.Deployment)) &&
		matchesAny(w.contexts, contextName)
}

// activeSince returns the latest occurrence at or before now whose window
// still covers now, and whether the window is active at now. A window is
// active in [start, start+duration).
func (w *Window) activeSince(now time.Time) (time.Time, bool) {
	start := w.schedule.Next(now.Add(-w.Duration))
	if start.IsZero() || start.After(now) {
		return time.Time{}, false
	}

	// Overlapping occurrences extend the window, so move to the latest start.
	for range maxOccurrenceSteps {
		next := w.schedule.Next(start)
		if next.IsZero() || next.After(now) {
			break
		}

		start = next
	}

	return start, true
}

// Active reports whether the window is active at now.
func (w *Window) Active(now time.Time) bool {
	_, active := w.activeSince(now)

	return active
}

// Location returns the timezone the window's schedule is evaluated in.
func (w *Window) Location() *time.Location {
	return w.location
}

// Empty reports whether the policy has no windows.
func (p *Policy) Empty() bool {
	return p == nil || len(p.windows) == 0
}

// Windows returns copies of the policy's windows in definition order.
func (p *Policy) Windows() []Window {
	if p == nil {
		return nil
	}

	windows := make([]Window, 0, len(p.windows))
	for _, w := range p.windows {
		windows = append(windows, *w)
	}

	return windows
}

// Evaluate decides whether a deployment of target started by origin may
// change the target at now.
func (p *Policy) Evaluate(now time.Time, target Target, origin Origin) Decision {
	if p.Empty() || origin == OriginReconciliation {
		return Decision{Allowed: true}
	}

	matching := p.matching(target)
	if len(matching) == 0 {
		return Decision{Allowed: true}
	}

	blocking := blockingAt(now, matching)
	if len(blocking) == 0 {
		return Decision{Allowed: true}
	}

	names := windowNames(blocking)

	if origin == OriginManual && allManualSync(blocking) {
		return Decision{Allowed: true, ManualOverride: true, Windows: names}
	}

	return Decision{
		Allowed:  false,
		Windows:  names,
		NextOpen: nextOpen(now, matching),
	}
}

func (p *Policy) matching(target Target) []*Window {
	var matching []*Window

	for _, w := range p.windows {
		if w.matches(target) {
			matching = append(matching, w)
		}
	}

	return matching
}

// blockingAt returns the windows that block a deployment at now: the active
// deny windows, plus all allow windows if none of them is active.
func blockingAt(now time.Time, windows []*Window) []*Window {
	var (
		activeDeny  []*Window
		allows      []*Window
		allowActive bool
	)

	for _, w := range windows {
		active := w.Active(now)

		switch w.Kind {
		case KindDeny:
			if active {
				activeDeny = append(activeDeny, w)
			}
		case KindAllow:
			allows = append(allows, w)
			allowActive = allowActive || active
		}
	}

	// Inactive allow windows block alongside active deny windows, so that a
	// manual deployment needs manual_sync on both to bypass them.
	if len(allows) > 0 && !allowActive {
		return append(activeDeny, allows...)
	}

	return activeDeny
}

// nextOpen searches for the earliest time after now at which no window
// blocks. Candidates are the next start of every allow window and the end of
// every active deny window. It returns the zero time if nothing is found
// within the search bounds.
func nextOpen(now time.Time, windows []*Window) time.Time {
	at := now
	horizon := now.Add(nextOpenHorizon)

	for range maxNextOpenSteps {
		var candidate time.Time

		for _, w := range windows {
			var next time.Time

			switch w.Kind {
			case KindAllow:
				next = w.schedule.Next(at)
			case KindDeny:
				if start, active := w.activeSince(at); active {
					next = start.Add(w.Duration)
				}
			}

			if !next.IsZero() && next.After(at) && (candidate.IsZero() || next.Before(candidate)) {
				candidate = next
			}
		}

		if candidate.IsZero() || candidate.After(horizon) {
			return time.Time{}
		}

		if len(blockingAt(candidate, windows)) == 0 {
			return candidate
		}

		at = candidate
	}

	return time.Time{}
}

func allManualSync(windows []*Window) bool {
	for _, w := range windows {
		if !w.ManualSync {
			return false
		}
	}

	return true
}

func windowNames(windows []*Window) []string {
	names := make([]string, 0, len(windows))
	for _, w := range windows {
		names = append(names, w.Name)
	}

	return names
}

// Status describes a window at a point in time.
type Status struct {
	Name         string     `json:"name"`
	Kind         Kind       `json:"kind"`
	Schedule     string     `json:"schedule"`
	Duration     string     `json:"duration"`
	Timezone     string     `json:"timezone"`
	Repositories []string   `json:"repositories"`
	Deployments  []string   `json:"deployments"`
	Contexts     []string   `json:"contexts"`
	ManualSync   bool       `json:"manual_sync"`
	Active       bool       `json:"active"`
	ActiveSince  *time.Time `json:"active_since,omitempty"`
	ActiveUntil  *time.Time `json:"active_until,omitempty"`
	NextStart    *time.Time `json:"next_start,omitempty"`
}

// Statuses returns the state of every window at now.
func (p *Policy) Statuses(now time.Time) []Status {
	if p == nil {
		return []Status{}
	}

	statuses := make([]Status, 0, len(p.windows))

	for _, w := range p.windows {
		status := Status{
			Name:         w.Name,
			Kind:         w.Kind,
			Schedule:     w.Schedule,
			Duration:     w.Duration.String(),
			Timezone:     w.location.String(),
			Repositories: nonNil(w.Repositories),
			Deployments:  nonNil(w.Deployments),
			Contexts:     nonNil(w.Contexts),
			ManualSync:   w.ManualSync,
		}

		if start, active := w.activeSince(now); active {
			end := start.Add(w.Duration)
			status.Active = true
			status.ActiveSince = &start
			status.ActiveUntil = &end
		}

		if next := w.schedule.Next(now); !next.IsZero() {
			status.NextStart = &next
		}

		statuses = append(statuses, status)
	}

	return statuses
}

// Report is the state of a policy at a point in time.
type Report struct {
	Time    time.Time `json:"time"`
	Windows []Status  `json:"windows"`
	// Decision is the decision for the requested target, if any.
	Decision *Decision `json:"decision,omitempty"`
}

// Report returns the state of every window at now and, if target is not
// nil, the decision for deploying target with origin at now.
func (p *Policy) Report(now time.Time, target *Target, origin Origin) Report {
	report := Report{Time: now, Windows: p.Statuses(now)}

	if target != nil {
		decision := p.Evaluate(now, *target, origin)
		decision.Windows = nonNil(decision.Windows)
		report.Decision = &decision
	}

	return report
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}

	return values
}
