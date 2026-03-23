package docker

import (
	"reflect"
	"testing"
)

func Test_getLatestServiceLabels(t *testing.T) {
	tests := []struct {
		name          string
		serviceLabels map[Service]Labels
		repoName      string
		want          Labels
	}{
		{
			name:          "empty serviceLabels",
			serviceLabels: map[Service]Labels{},
			repoName:      "repo",
			want:          nil,
		},
		{
			name: "signal service with no timestamp",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name: "repo",
				},
			},
			repoName: "repo",
			want: Labels{
				DocoCDLabels.Repository.Name: "repo",
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
			want: Labels{
				DocoCDLabels.Repository.Name:      "repo",
				DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
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
			want:     nil,
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
			want:     nil,
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
			want: Labels{
				DocoCDLabels.Repository.Name:      "",
				DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
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
			want: Labels{
				DocoCDLabels.Repository.Name:      "repo",
				DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLatestServiceLabels(tt.serviceLabels, tt.repoName)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getLatestServiceLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}
