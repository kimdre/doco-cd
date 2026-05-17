package stages

import (
	"slices"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestAutoDiscoveryConfigLabelDriftServices(t *testing.T) {
	expected := "{enabled: true, depth: 0, delete: false, remove_volumes: true, remove_images: true}"

	tests := []struct {
		name           string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
