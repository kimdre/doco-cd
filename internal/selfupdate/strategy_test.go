package selfupdate

import (
	"errors"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func baseService() types.ServiceConfig {
	one := 1

	return types.ServiceConfig{
		Name:    "app",
		Image:   "ghcr.io/kimdre/doco-cd:latest",
		Restart: "unless-stopped",
		Scale:   &one,
	}
}

func TestSelect(t *testing.T) {
	t.Parallel()

	withPorts := baseService()
	withPorts.Ports = []types.ServicePortConfig{{Target: 80, Published: "80"}}

	withName := baseService()
	withName.ContainerName = "doco-cd"

	hostNet := baseService()
	hostNet.NetworkMode = "host"

	twoReplicas := baseService()
	two := 2
	twoReplicas.Scale = &two

	noRestart := baseService()
	noRestart.Restart = ""

	restartNo := baseService()
	restartNo.Restart = "no"

	onFailure := baseService()
	onFailure.Restart = "on-failure:3"

	tests := []struct {
		name        string
		svc         types.ServiceConfig
		sourceType  string
		contextName string
		requested   Strategy
		constraints Constraints
		want        Strategy
		wantReason  string
		wantErr     error
	}{
		{name: "plain service picks scale out", svc: baseService(), want: StrategyScaleOut},
		{name: "on-failure restart is accepted", svc: onFailure, want: StrategyScaleOut},
		{name: "container_name forces applier", svc: withName, want: StrategyApplier, wantReason: "container_name"},
		{name: "host ports force applier", svc: withPorts, want: StrategyApplier, wantReason: "host ports"},
		{name: "host network forces applier", svc: hostNet, want: StrategyApplier, wantReason: "network_mode"},
		{name: "network drift forces applier", svc: baseService(), constraints: Constraints{NetworkDrift: true}, want: StrategyApplier, wantReason: "network must be recreated"},
		{name: "applier can be requested for a plain service", svc: baseService(), requested: StrategyApplier, want: StrategyApplier},

		{name: "two replicas are unsupported", svc: twoReplicas, wantErr: ErrUnsupported},
		{name: "missing restart policy is unsupported", svc: noRestart, wantErr: ErrUnsupported},
		{name: "restart no is unsupported", svc: restartNo, wantErr: ErrUnsupported},
		{name: "oci source is unsupported", svc: baseService(), sourceType: "oci", wantErr: ErrUnsupported},
		{name: "non-default context is unsupported", svc: baseService(), contextName: "remote", wantErr: ErrUnsupported},
		{name: "requesting scale out with ports errors", svc: withPorts, requested: StrategyScaleOut, wantErr: ErrUnsupported, wantReason: "host ports"},
		{name: "unknown strategy errors", svc: baseService(), requested: Strategy("magic"), wantErr: ErrUnsupported},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, reasons, err := Select(tt.svc, tt.sourceType, tt.contextName, tt.requested, tt.constraints)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}

				if tt.wantReason != "" && !strings.Contains(err.Error(), tt.wantReason) {
					t.Errorf("error %q does not mention %q", err, tt.wantReason)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tt.want {
				t.Errorf("strategy = %q, want %q", got, tt.want)
			}

			if tt.wantReason != "" && !strings.Contains(strings.Join(reasons, "; "), tt.wantReason) {
				t.Errorf("reasons %v do not mention %q", reasons, tt.wantReason)
			}

			if tt.want == StrategyScaleOut && len(reasons) > 0 {
				t.Errorf("scale out was chosen but reasons are non-empty: %v", reasons)
			}
		})
	}
}

func TestSelectDeployRestartPolicyIsAccepted(t *testing.T) {
	t.Parallel()

	svc := baseService()
	svc.Restart = ""
	svc.Deploy = &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "any"}}

	got, _, err := Select(svc, "", "", StrategyAuto, Constraints{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got != StrategyScaleOut {
		t.Errorf("strategy = %q, want %q", got, StrategyScaleOut)
	}
}
