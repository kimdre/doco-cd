package stages

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestShouldSkipDeployment(t *testing.T) {
	tests := []struct {
		name            string
		composeChanged  bool
		changedServices []docker.Change
		ignoredInfo     docker.IgnoredInfo
		imagesChanged   bool
		missingServices []string
		want            bool
	}{
		{
			name:            "no changes",
			composeChanged:  false,
			changedServices: nil,
			ignoredInfo:     docker.IgnoredInfo{},
			imagesChanged:   false,
			missingServices: nil,
			want:            true,
		},
		{
			name:            "compose file changed",
			composeChanged:  true,
			changedServices: nil,
			ignoredInfo:     docker.IgnoredInfo{},
			imagesChanged:   false,
			missingServices: nil,
			want:            false,
		},
		{
			name:           "services changed",
			composeChanged: false,
			changedServices: []docker.Change{{
				Type:     "configs",
				Services: []string{"web"},
			}},
			ignoredInfo:     docker.IgnoredInfo{},
			imagesChanged:   false,
			missingServices: nil,
			want:            false,
		},
		{
			name:            "ignored changes",
			composeChanged:  false,
			changedServices: nil,
			ignoredInfo:     docker.IgnoredInfo{Ignored: []string{"web"}},
			imagesChanged:   false,
			missingServices: nil,
			want:            true,
		},
		{
			name:            "ignored changes but need send signal",
			composeChanged:  false,
			changedServices: nil,
			ignoredInfo: docker.IgnoredInfo{NeedSendSignal: []docker.SignalService{
				{ServiceName: "web", Signal: "SIGHUP"},
			}},
			imagesChanged:   false,
			missingServices: nil,
			want:            false,
		},
		{
			name:            "images changed",
			composeChanged:  false,
			changedServices: nil,
			ignoredInfo:     docker.IgnoredInfo{},
			imagesChanged:   true,
			missingServices: nil,
			want:            false,
		},
		{
			name:            "missing services",
			composeChanged:  false,
			changedServices: nil,
			ignoredInfo:     docker.IgnoredInfo{},
			imagesChanged:   false,
			missingServices: []string{"web"},
			want:            false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipDeployment(tt.composeChanged, tt.changedServices, tt.ignoredInfo, tt.imagesChanged, tt.missingServices)
			if got != tt.want {
				t.Errorf("shouldSkipDeployment() = %v, want %v", got, tt.want)
			}
		})
	}
}
