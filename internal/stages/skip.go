package stages

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kimdre/doco-cd/internal/docker"
)

// Short reasons for skipped deployments, shown in commit statuses.
const (
	SkipReasonWebhookFilter  = "webhook filter did not match"
	SkipReasonNoChanges      = "no changes detected"
	SkipReasonStale          = "a newer commit is already deployed"
	SkipReasonSelfUpdate     = "self-update to this commit failed before"
	SkipReasonNothingDestroy = "nothing to destroy"
)

// maxSkippedStacksInSummary bounds the stacks listed in a skipped run summary.
const maxSkippedStacksInSummary = 100

// SkipError explains why a deployment was skipped. It wraps ErrSkipDeployment,
// or a more specific sentinel wrapping it such as ErrWebhookFilterMismatch, and
// keeps its message.
type SkipError struct {
	// Reason is a short, plain text reason, e.g. "no changes detected".
	Reason string
	// Detail optionally adds specifics as Markdown, e.g. the filter that did not match.
	Detail string

	err error
}

func (e *SkipError) Error() string {
	return e.err.Error()
}

func (e *SkipError) Unwrap() error {
	return e.err
}

// skipDeployment returns an ErrSkipDeployment with a reason.
func skipDeployment(reason, detail string) error {
	return &SkipError{Reason: reason, Detail: detail, err: ErrSkipDeployment}
}

// NothingToDestroy returns an ErrSkipDeployment for a stack to destroy that
// does not exist.
func NothingToDestroy() error {
	return skipDeployment(SkipReasonNothingDestroy, "the stack does not exist")
}

// WebhookFilterMismatch returns an ErrWebhookFilterMismatch naming the
// deployment's webhook filter and the webhook reference.
func (s *StageManager) WebhookFilterMismatch() error {
	ref := ""
	if s.Payload != nil {
		ref = s.Payload.Ref
	}

	return &SkipError{
		Reason: SkipReasonWebhookFilter,
		Detail: fmt.Sprintf("%s does not match %s", MarkdownCode(s.DeployConfig.WebhookEventFilter), MarkdownCode(ref)),
		err:    ErrWebhookFilterMismatch,
	}
}

// SkippedStack is a stack skipped by a run, with the reason.
type SkippedStack struct {
	Name    string
	Context string
	Reason  string
	Detail  string
}

// NewSkippedStack describes the stack name in context, skipped with err.
func NewSkippedStack(name, context string, err error) SkippedStack {
	stack := SkippedStack{Name: name, Context: context}

	if skipErr, ok := errors.AsType[*SkipError](err); ok {
		stack.Reason, stack.Detail = skipErr.Reason, skipErr.Detail
	} else if errors.Is(err, ErrWebhookFilterMismatch) {
		stack.Reason = SkipReasonWebhookFilter
	}

	return stack
}

// SkippedRunError reports that no stack of a run needed a deployment, with the
// reason for each stack. It wraps ErrWebhookFilterMismatch when every stack
// was filtered out, and ErrSkipDeployment otherwise.
type SkippedRunError struct {
	Stacks []SkippedStack

	err error
}

// NewSkippedRunError returns the result of a run that skipped every stack.
func NewSkippedRunError(stacks []SkippedStack) *SkippedRunError {
	err := ErrSkipDeployment
	if len(stacks) > 0 && !slices.ContainsFunc(stacks, func(s SkippedStack) bool { return s.Reason != SkipReasonWebhookFilter }) {
		err = ErrWebhookFilterMismatch
	}

	stacks = slices.Clone(stacks)
	slices.SortFunc(stacks, func(a, b SkippedStack) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Context, b.Context))
	})

	return &SkippedRunError{Stacks: stacks, err: err}
}

func (e *SkippedRunError) Error() string {
	return e.err.Error()
}

func (e *SkippedRunError) Unwrap() error {
	return e.err
}

// SkippedCommitStatusDescription returns the commit status description of a
// skipped run: its reason when every stack shares one, e.g. "Skipped: no
// changes detected", and "Skipped" otherwise.
func SkippedCommitStatusDescription(err error) string {
	run, ok := errors.AsType[*SkippedRunError](err)
	if !ok || len(run.Stacks) == 0 {
		return "Skipped"
	}

	reason := run.Stacks[0].Reason
	for _, stack := range run.Stacks[1:] {
		if stack.Reason != reason {
			return "Skipped"
		}
	}

	if reason == "" {
		return "Skipped"
	}

	return "Skipped: " + reason
}

// SkippedCommitStatusSummary returns a Markdown summary of a skipped run for
// the webhook reference and commit, listing every stack with its reason. It
// returns "" when the stacks are unknown.
func SkippedCommitStatusSummary(err error, ref, commitSHA string) string {
	run, ok := errors.AsType[*SkippedRunError](err)
	if !ok || len(run.Stacks) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("No stack needed a deployment")

	if ref = strings.TrimSpace(ref); ref != "" {
		b.WriteString(" for " + MarkdownCode(ref))
	}

	if commitSHA = strings.TrimSpace(commitSHA); commitSHA != "" {
		b.WriteString(" at " + MarkdownCode(shortCommit(commitSHA)))
	}

	b.WriteString(".\n\n| Stack | Reason |\n| --- | --- |\n")

	for i, stack := range run.Stacks {
		if i == maxSkippedStacksInSummary {
			fmt.Fprintf(&b, "\n…and %d more stacks.\n", len(run.Stacks)-i)

			break
		}

		fmt.Fprintf(&b, "| %s | %s |\n", markdownTableCell(stackLabel(stack)), markdownTableCell(stackReason(stack)))
	}

	return b.String()
}

func stackLabel(stack SkippedStack) string {
	label := MarkdownCode(stack.Name)

	if contextName := docker.NormalizeContextName(stack.Context); contextName != "" {
		label += " on " + MarkdownCode(contextName)
	}

	return label
}

func stackReason(stack SkippedStack) string {
	reason := stack.Reason
	if reason == "" {
		reason = "skipped"
	}

	reason = strings.ToUpper(reason[:1]) + reason[1:]

	if stack.Detail != "" {
		reason += ": " + stack.Detail
	}

	return reason
}

func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}

	return sha
}

// MarkdownCode formats s as a Markdown code span, whatever backticks it contains.
func MarkdownCode(s string) string {
	if s == "" {
		return "` `"
	}

	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}

	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}

	return fence + s + fence
}

// markdownTableCell keeps s on one line and escapes pipes, which would
// otherwise end the table cell even inside a code span.
func markdownTableCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")

	return strings.ReplaceAll(s, "|", `\|`)
}
