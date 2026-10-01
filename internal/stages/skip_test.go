package stages

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestSkipErrorKeepsSentinels(t *testing.T) {
	t.Parallel()

	skip := skipDeployment(SkipReasonNoChanges, "")
	if !errors.Is(skip, ErrSkipDeployment) || errors.Is(skip, ErrWebhookFilterMismatch) {
		t.Fatalf("skipDeployment() = %v, want ErrSkipDeployment only", skip)
	}

	if skip.Error() != ErrSkipDeployment.Error() {
		t.Fatalf("Error() = %q, want %q", skip.Error(), ErrSkipDeployment.Error())
	}

	stageMgr := &StageManager{
		DeployConfig: &deployConfig.Config{WebhookEventFilter: "^refs/heads/main$"},
		Payload:      &webhook.ParsedPayload{Ref: "refs/heads/renovate/x"},
	}

	filtered := stageMgr.WebhookFilterMismatch()
	if !errors.Is(filtered, ErrWebhookFilterMismatch) || !errors.Is(filtered, ErrSkipDeployment) {
		t.Fatalf("WebhookFilterMismatch() = %v, want ErrWebhookFilterMismatch", filtered)
	}

	stack := NewSkippedStack("web", "", filtered)
	if stack.Reason != SkipReasonWebhookFilter || stack.Detail != "`^refs/heads/main$` does not match `refs/heads/renovate/x`" {
		t.Fatalf("NewSkippedStack() = %+v", stack)
	}
}

func TestNewSkippedRunErrorSentinel(t *testing.T) {
	t.Parallel()

	filtered := NewSkippedRunError([]SkippedStack{{Name: "web", Reason: SkipReasonWebhookFilter}})
	if !errors.Is(filtered, ErrWebhookFilterMismatch) {
		t.Fatalf("all filtered run = %v, want ErrWebhookFilterMismatch", filtered)
	}

	mixed := NewSkippedRunError([]SkippedStack{
		{Name: "web", Reason: SkipReasonWebhookFilter},
		{Name: "api", Reason: SkipReasonNoChanges},
	})
	if errors.Is(mixed, ErrWebhookFilterMismatch) || !errors.Is(mixed, ErrSkipDeployment) {
		t.Fatalf("mixed run = %v, want ErrSkipDeployment only", mixed)
	}

	if mixed.Stacks[0].Name != "api" {
		t.Fatalf("stacks = %+v, want them sorted by name", mixed.Stacks)
	}
}

func TestSkippedCommitStatusDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "plain skip", err: ErrSkipDeployment, want: "Skipped"},
		{name: "one reason", err: NewSkippedRunError([]SkippedStack{
			{Name: "web", Reason: SkipReasonNoChanges},
			{Name: "api", Reason: SkipReasonNoChanges},
		}), want: "Skipped: no changes detected"},
		{name: "mixed reasons", err: NewSkippedRunError([]SkippedStack{
			{Name: "web", Reason: SkipReasonNoChanges},
			{Name: "api", Reason: SkipReasonWebhookFilter},
		}), want: "Skipped"},
		{name: "unknown reason", err: NewSkippedRunError([]SkippedStack{{Name: "web"}}), want: "Skipped"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := SkippedCommitStatusDescription(tt.err); got != tt.want {
				t.Fatalf("SkippedCommitStatusDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSkippedCommitStatusSummary(t *testing.T) {
	t.Parallel()

	if got := SkippedCommitStatusSummary(ErrSkipDeployment, "refs/heads/main", "0123456789"); got != "" {
		t.Fatalf("summary without stacks = %q, want empty", got)
	}

	run := NewSkippedRunError([]SkippedStack{
		{Name: "web", Reason: SkipReasonWebhookFilter, Detail: MarkdownCode("^(main|dev)$") + " does not match " + MarkdownCode("refs/heads/x")},
		{Name: "api", Context: "remote", Reason: SkipReasonNoChanges},
		{Name: "db", Context: "default"},
	})

	want := "No stack needed a deployment for `refs/heads/x` at `0123456`.\n\n" +
		"| Stack | Reason |\n| --- | --- |\n" +
		"| `api` on `remote` | No changes detected |\n" +
		"| `db` | Skipped |\n" +
		"| `web` | Webhook filter did not match: `^(main\\|dev)$` does not match `refs/heads/x` |\n"

	if got := SkippedCommitStatusSummary(run, "refs/heads/x", "0123456789"); got != want {
		t.Fatalf("SkippedCommitStatusSummary() =\n%s\nwant\n%s", got, want)
	}
}

func TestSkippedCommitStatusSummaryLimitsStacks(t *testing.T) {
	t.Parallel()

	stacks := make([]SkippedStack, maxSkippedStacksInSummary+5)
	for i := range stacks {
		stacks[i] = SkippedStack{Name: fmt.Sprintf("stack-%03d", i), Reason: SkipReasonNoChanges}
	}

	got := SkippedCommitStatusSummary(NewSkippedRunError(stacks), "", "")

	if rows := strings.Count(got, "| No changes detected |"); rows != maxSkippedStacksInSummary {
		t.Fatalf("summary has %d rows, want %d", rows, maxSkippedStacksInSummary)
	}

	if !strings.Contains(got, "…and 5 more stacks.") {
		t.Fatalf("summary = %q, want the number of omitted stacks", got)
	}
}

func TestMarkdownCode(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"main":    "`main`",
		"a`b":     "``a`b``",
		"`a":      "`` `a ``",
		"a``b`":   "``` a``b` ```",
		"":        "` `",
		"x|y":     "`x|y`",
		"refs/x/": "`refs/x/`",
	}

	for in, want := range tests {
		if got := MarkdownCode(in); got != want {
			t.Errorf("MarkdownCode(%q) = %q, want %q", in, got, want)
		}
	}
}
