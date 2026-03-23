package docker

import (
	"reflect"
	"slices"
	"testing"
)

func Test_getLatestServiceState(t *testing.T) {
	tests := []struct {
		name          string
		serviceLabels map[Service]Labels
		repoName      string
		want          LatestServiceState
	}{
		{
			name:          "empty serviceLabels",
			serviceLabels: map[Service]Labels{},
			repoName:      "repo",
			want:          LatestServiceState{},
		},
		{
			name: "signal service with no timestamp",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name: "repo",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name: "repo",
				},
				DeployedServicesName: []string{"svc1"},
			},
		},
		{
			name: "signal service with timestamp",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1"},
			},
		},
		{
			name: "two service with timestamp but repo not match",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo-2",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "repo-2",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want:     LatestServiceState{},
		},
		{
			name: "two service with timestamp but repo mixed",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "repo-2",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1"},
			},
		},
		{
			name: "two service with timestamp but no repo",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want:     LatestServiceState{},
		},
		{
			name: "two service with timestamp but empty repo",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1", "svc2"},
			},
		},
		{
			name: "two service with timestamp",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1", "svc2"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLatestServiceState(tt.serviceLabels, tt.repoName)
			slices.Sort(tt.want.DeployedServicesName)
			slices.Sort(got.DeployedServicesName)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLatestServiceState() = %v, want %v", got, tt.want)
			}
		})
	}
}
