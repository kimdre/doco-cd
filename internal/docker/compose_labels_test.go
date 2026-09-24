package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestAddComposeServiceLabels_ScheduledJobOwner(t *testing.T) {
	for _, tc := range []struct {
		name          string
		instanceID    string
		enabled       string
		explicitOwner string
		customOwner   string
		wantOwner     string
		wantStamped   bool
	}{
		{name: "configured instance", instanceID: "instance-b", enabled: "true", wantOwner: "instance-b", wantStamped: true},
		{name: "unconfigured instance", enabled: "true"},
		{name: "compose owner overrides rotation instance", instanceID: "instance-b", enabled: "true", explicitOwner: "instance-a", wantOwner: "instance-a"},
		{name: "existing custom owner is retained", instanceID: "instance-b", enabled: "true", customOwner: "instance-a", wantOwner: "instance-a", wantStamped: true},
		{name: "disabled job", instanceID: "instance-b", enabled: "false"},
		{name: "invalid job flag", instanceID: "instance-b", enabled: "invalid"},
		{name: "ordinary service", instanceID: "instance-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := types.Labels{"user.label": "keep-me"}
			if tc.enabled != "" {
				labels[DocoCDJobLabels.JobEnabled] = tc.enabled
				labels[DocoCDJobLabels.JobSchedule] = "@hourly"
			}

			if tc.explicitOwner != "" {
				labels[DocoCDJobLabels.JobOwner] = tc.explicitOwner
			}

			service := types.ServiceConfig{Name: "backup", Labels: labels}
			if tc.customOwner != "" {
				service.CustomLabels = types.Labels{DocoCDJobLabels.JobOwner: tc.customOwner}
			}

			project := &types.Project{
				Name: "stack",
				Services: types.Services{
					"backup": service,
					"web":    {Name: "web", Labels: types.Labels{"user.label": "untouched"}},
				},
			}
			addComposeServiceLabels(project, &deploy.Config{Name: "stack"}, &webhook.ParsedPayload{},
				"", "/repo", "dev", "2026-01-01T00:00:00Z", ComposeVersion, "", "", tc.instanceID)

			job := project.Services["backup"]
			if got := getServiceSchedulerLabels(job)[DocoCDJobLabels.JobOwner]; got != tc.wantOwner {
				t.Errorf("effective owner = %q, want %q", got, tc.wantOwner)
			}

			if got, ok := job.CustomLabels[DocoCDJobLabels.JobOwner]; ok != tc.wantStamped ||
				(tc.wantStamped && got != tc.wantOwner) {
				t.Errorf("custom owner = %q (present=%t), want %q (present=%t)", got, ok, tc.wantOwner, tc.wantStamped)
			}

			if _, ok := project.Services["web"].CustomLabels[DocoCDJobLabels.JobOwner]; ok {
				t.Error("ordinary service received a job owner")
			}

			if job.Labels["user.label"] != "keep-me" {
				t.Error("compose-defined labels changed")
			}
		})
	}
}
