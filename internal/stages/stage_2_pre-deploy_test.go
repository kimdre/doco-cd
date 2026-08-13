package stages

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestAutoDiscoveryConfigLabelDriftServices(t *testing.T) {
	expected := "{enabled: true, depth: 0, delete: false, remove_volumes: true, remove_images: true}"

	disabled := "{enabled: false, depth: 0, delete: false, remove_volumes: false, remove_images: true}"

	tests := []struct {
		name           string
		expected       string // defaults to expected when empty
		status         map[docker.Service]docker.ServiceStatus
		wantServices   []string
		wantFirstLabel string
	}{
		{
			name: "matching labels",
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: expected,
					},
				},
			},
			wantServices:   nil,
			wantFirstLabel: expected,
		},
		{
			name: "mismatched labels",
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: true, depth: 0, delete: true, remove_volumes: true, remove_images: true}",
					},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "{enabled: true, depth: 0, delete: true, remove_volumes: true, remove_images: true}",
		},
		{
			name: "missing label",
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "",
		},
		{
			name: "multiple services sorted",
			status: map[docker.Service]docker.ServiceStatus{
				"z-api": {
					Labels: docker.Labels{},
				},
				"a-web": {
					Labels: docker.Labels{},
				},
			},
			wantServices:   []string{"a-web", "z-api"},
			wantFirstLabel: "",
		},
		{
			// A changed default must not recreate the stack while auto-discovery is off,
			// because the label steers auto-discovery cleanup only.
			name:     "disabled on both sides, only a default differs",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: false, depth: 0, delete: true, remove_volumes: false, remove_images: true}",
					},
				},
			},
			wantServices:   nil,
			wantFirstLabel: disabled,
		},
		{
			name:     "disabled now, deployed while enabled",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: true, depth: 0, delete: true, remove_volumes: false, remove_images: true}",
					},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "{enabled: true, depth: 0, delete: true, remove_volumes: false, remove_images: true}",
		},
		{
			name:     "disabled with no label yet",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{},
				},
			},
			wantServices:   nil,
			wantFirstLabel: disabled,
		},
		{
			name:     "disabled now, legacy deployment was enabled",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscovery: "true",
					},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expected := expected
			if tt.expected != "" {
				expected = tt.expected
			}

			gotServices, gotFirst := autoDiscoveryConfigLabelDriftServices(tt.status, expected)
			if !slices.Equal(gotServices, tt.wantServices) {
				t.Fatalf("autoDiscoveryConfigLabelDriftServices() services = %v, want %v", gotServices, tt.wantServices)
			}

			if gotFirst != tt.wantFirstLabel {
				t.Fatalf("autoDiscoveryConfigLabelDriftServices() first label = %q, want %q", gotFirst, tt.wantFirstLabel)
			}
		})
	}
}

func TestShouldSkipDeployment(t *testing.T) {
	tests := []struct {
		name                      string
		composeChanged            bool
		autoDiscoveryLabelChanged bool
		changedServices           []docker.Change
		ignoredInfo               docker.IgnoredInfo
		imagesChanged             bool
		mismatchServices          []docker.ServiceMismatch
		want                      bool
	}{
		{
			name:                      "no changes",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      true,
		},
		{
			name:                      "compose file changed",
			composeChanged:            true,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      false,
		},
		{
			name:                      "services changed",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices: []docker.Change{{
				Type:     "configs",
				Services: []string{"web"},
			}},
			ignoredInfo:      docker.IgnoredInfo{},
			imagesChanged:    false,
			mismatchServices: nil,
			want:             false,
		},
		{
			name:                      "ignored changes",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{Ignored: []string{"web"}},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      true,
		},
		{
			name:                      "ignored changes but need send signal",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo: docker.IgnoredInfo{NeedSendSignal: []docker.SignalService{
				{ServiceName: "web", Signal: "SIGHUP"},
			}},
			imagesChanged:    false,
			mismatchServices: nil,
			want:             false,
		},
		{
			name:                      "images changed",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             true,
			mismatchServices:          nil,
			want:                      false,
		},
		{
			name:                      "missing services",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices: []docker.ServiceMismatch{
				{
					ServiceName: "web",
					Reasons: []docker.ServiceMismatchReason{
						{
							Reason: docker.ServiceMismatchReasonNotDeployed,
						},
					},
				},
			},
			want: false,
		},
		{
			name:                      "auto discovery label changed",
			composeChanged:            false,
			autoDiscoveryLabelChanged: true,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipDeployment(tt.composeChanged, tt.autoDiscoveryLabelChanged, tt.changedServices, tt.ignoredInfo, tt.imagesChanged, tt.mismatchServices)
			if got != tt.want {
				t.Errorf("shouldSkipDeployment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldSkipOCIDeployment(t *testing.T) {
	tests := []struct {
		name          string
		forceRecreate bool
		deployed      string
		resolved      string
		want          bool
	}{
		{
			name:          "skip when digest unchanged",
			forceRecreate: false,
			deployed:      "sha256:abc",
			resolved:      "sha256:abc",
			want:          true,
		},
		{
			name:          "do not skip when digest changed",
			forceRecreate: false,
			deployed:      "sha256:abc",
			resolved:      "sha256:def",
			want:          false,
		},
		{
			name:          "do not skip when deployed digest missing",
			forceRecreate: false,
			deployed:      "",
			resolved:      "sha256:def",
			want:          false,
		},
		{
			name:          "do not skip when resolved digest missing",
			forceRecreate: false,
			deployed:      "sha256:def",
			resolved:      "",
			want:          false,
		},
		{
			name:          "force recreate disables skip",
			forceRecreate: true,
			deployed:      "sha256:abc",
			resolved:      "sha256:abc",
			want:          false,
		},
		{
			name:          "trims surrounding whitespace",
			forceRecreate: false,
			deployed:      "  sha256:abc  ",
			resolved:      "sha256:abc",
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipOCIDeployment(tt.forceRecreate, tt.deployed, tt.resolved)
			if got != tt.want {
				t.Errorf("shouldSkipOCIDeployment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldRecoverFromMissingDeployedCommit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "object not found",
			err:  plumbing.ErrObjectNotFound,
			want: true,
		},
		{
			name: "wrapped object not found",
			err:  fmt.Errorf("wrapped: %w", plumbing.ErrObjectNotFound),
			want: true,
		},
		{
			name: "reference not found",
			err:  plumbing.ErrReferenceNotFound,
			want: true,
		},
		{
			name: "generic error",
			err:  errors.New("some other git error"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRecoverFromMissingDeployedCommit(tt.err)
			if got != tt.want {
				t.Fatalf("shouldRecoverFromMissingDeployedCommit() = %v, want %v", got, tt.want)
			}
		})
	}
}
