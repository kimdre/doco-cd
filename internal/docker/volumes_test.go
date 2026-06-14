package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

func TestVolumeConfigMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing *types.VolumeConfig
		desired  *types.VolumeConfig
		want     bool
	}{
		{
			name:     "both nil",
			existing: nil,
			desired:  nil,
			want:     true,
		},
		{
			name:     "existing nil, desired not nil",
			existing: nil,
			desired:  &types.VolumeConfig{Driver: "local"},
			want:     false,
		},
		{
			name:     "existing not nil, desired nil",
			existing: &types.VolumeConfig{Driver: "local"},
			desired:  nil,
			want:     false,
		},
		{
			name:     "same driver local",
			existing: &types.VolumeConfig{Driver: "local"},
			desired:  &types.VolumeConfig{Driver: "local"},
			want:     true,
		},
		{
			name:     "same driver empty defaults to local",
			existing: &types.VolumeConfig{Driver: ""},
			desired:  &types.VolumeConfig{Driver: "local"},
			want:     true,
		},
		{
			name:     "different drivers",
			existing: &types.VolumeConfig{Driver: "nfs"},
			desired:  &types.VolumeConfig{Driver: "cifs"},
			want:     false,
		},
		{
			name: "same driver with matching options",
			existing: &types.VolumeConfig{
				Driver: "nfs",
				DriverOpts: types.Options{
					"addr": "192.168.1.1",
					"path": "/mnt/data",
				},
			},
			desired: &types.VolumeConfig{
				Driver: "nfs",
				DriverOpts: types.Options{
					"addr": "192.168.1.1",
					"path": "/mnt/data",
				},
			},
			want: true,
		},
		{
			name: "same driver with different option values",
			existing: &types.VolumeConfig{
				Driver: "nfs",
				DriverOpts: types.Options{
					"addr": "192.168.1.1",
				},
			},
			desired: &types.VolumeConfig{
				Driver: "nfs",
				DriverOpts: types.Options{
					"addr": "192.168.1.2",
				},
			},
			want: false,
		},
		{
			name: "existing has extra option",
			existing: &types.VolumeConfig{
				Driver: "tmpfs",
				DriverOpts: types.Options{
					"size": "512m",
					"mode": "1777",
				},
			},
			desired: &types.VolumeConfig{
				Driver: "tmpfs",
				DriverOpts: types.Options{
					"size": "512m",
				},
			},
			want: false,
		},
		{
			name: "desired has extra option",
			existing: &types.VolumeConfig{
				Driver: "tmpfs",
				DriverOpts: types.Options{
					"size": "512m",
				},
			},
			desired: &types.VolumeConfig{
				Driver: "tmpfs",
				DriverOpts: types.Options{
					"size": "512m",
					"mode": "1777",
				},
			},
			want: false,
		},
		{
			name: "tmpfs size changed",
			existing: &types.VolumeConfig{
				Driver: "tmpfs",
				DriverOpts: types.Options{
					"size": "256m",
				},
			},
			desired: &types.VolumeConfig{
				Driver: "tmpfs",
				DriverOpts: types.Options{
					"size": "512m",
				},
			},
			want: false,
		},
		{
			name:     "no options in both",
			existing: &types.VolumeConfig{Driver: "local"},
			desired:  &types.VolumeConfig{Driver: "local"},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := volumeConfigMatch(tt.existing, tt.desired)
			if got != tt.want {
				t.Errorf("volumeConfigMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsRecreatableVolumeType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *types.VolumeConfig
		want bool
	}{
		{name: "nil", cfg: nil, want: false},
		{name: "empty driver without type", cfg: &types.VolumeConfig{}, want: false},
		{name: "driver local", cfg: &types.VolumeConfig{Driver: "local"}, want: false},
		{name: "empty driver with unsupported type", cfg: &types.VolumeConfig{DriverOpts: types.Options{"type": "zfs"}}, want: false},
		{name: "local with unsupported type", cfg: &types.VolumeConfig{Driver: "local", DriverOpts: types.Options{"type": "zfs"}}, want: false},
		{
			name: "local with type nfs",
			cfg:  &types.VolumeConfig{Driver: "local", DriverOpts: types.Options{"type": "nfs"}},
			want: true,
		},
		{
			name: "empty driver with type cifs",
			cfg:  &types.VolumeConfig{DriverOpts: types.Options{"type": "cifs"}},
			want: true,
		},
		{
			name: "local with type tmpfs",
			cfg:  &types.VolumeConfig{Driver: "local", DriverOpts: types.Options{"type": "tmpfs"}},
			want: true,
		},
		{
			name: "local with trimmed upper-case type tmpfs",
			cfg:  &types.VolumeConfig{Driver: "local", DriverOpts: types.Options{"type": " TMPFS "}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRecreatableVolumeType(tt.cfg)
			if got != tt.want {
				t.Errorf("isRecreatableVolumeType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveMismatchedRecreatableVolumes_SkipsUnsupportedVolumeTypes(t *testing.T) {
	t.Parallel()

	var deleteCalled bool

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/volumes") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(volume.ListResponse{
				Volumes: []volume.Volume{
					{
						Name:   "non-nfs-volume",
						Driver: "local",
						Labels: map[string]string{
							api.ProjectLabel: "stack-a",
						},
						Options: map[string]string{
							"type": "tmpfs",
							"size": "64m",
						},
					},
				},
			})
		case strings.Contains(r.URL.Path, "/volumes/") && r.Method == http.MethodDelete:
			deleteCalled = true

			w.WriteHeader(http.StatusOK)

			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cli, err := client.New(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithAPIVersion("1.52"),
	)
	if err != nil {
		t.Fatalf("failed to create docker api client: %v", err)
	}

	project := &types.Project{
		Volumes: types.Volumes{
			"non-nfs-volume": {
				Name:   "non-nfs-volume",
				Driver: "local",
				DriverOpts: types.Options{
					"type": "zfs",
					"size": "128m",
				},
			},
		},
	}

	if err := removeMismatchedRecreatableVolumes(context.Background(), cli, "stack-a", project); err != nil {
		t.Fatalf("removeMismatchedRecreatableVolumes() unexpected error: %v", err)
	}

	if deleteCalled {
		t.Fatal("expected VolumeRemove to never be called for unsupported volume types")
	}
}

func TestRemoveMismatchedRecreatableVolumes_RemovesMismatchedTmpfsVolumes(t *testing.T) {
	t.Parallel()

	var deleteCalled bool

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/volumes") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(volume.ListResponse{
				Volumes: []volume.Volume{{
					Name:   "tmpfs-volume",
					Driver: "local",
					Labels: map[string]string{api.ProjectLabel: "stack-a"},
					Options: map[string]string{
						"type": "tmpfs",
						"size": "64m",
					},
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/containers/json") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case strings.Contains(r.URL.Path, "/volumes/tmpfs-volume") && r.Method == http.MethodDelete:
			deleteCalled = true

			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cli, err := client.New(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithAPIVersion("1.52"),
	)
	if err != nil {
		t.Fatalf("failed to create docker api client: %v", err)
	}

	project := &types.Project{
		Volumes: types.Volumes{
			"tmpfs-volume": {
				Name:   "tmpfs-volume",
				Driver: "local",
				DriverOpts: types.Options{
					"type": "tmpfs",
					"size": "128m",
				},
			},
		},
	}

	if err := removeMismatchedRecreatableVolumes(context.Background(), cli, "stack-a", project); err != nil {
		t.Fatalf("removeMismatchedRecreatableVolumes() unexpected error: %v", err)
	}

	if !deleteCalled {
		t.Fatal("expected VolumeRemove to be called for changed tmpfs volume")
	}
}

func TestRemoveMismatchedRecreatableVolumes_StopsRunningContainersBeforeRemove(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		stopped     []string
		deleted     []string
		requestSeen []string
	)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()

		requestSeen = append(requestSeen, r.Method+" "+r.URL.Path)
		mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"Id":     "container-1",
					"Names":  []string{"/stack-a-app-1"},
					"State":  "running",
					"Labels": map[string]string{api.ProjectLabel: "stack-a"},
					"Mounts": []map[string]any{{"Name": "shared-volume", "Source": "shared-volume"}},
				},
			})
		case strings.Contains(r.URL.Path, "/containers/container-1/stop") && r.Method == http.MethodPost:
			mu.Lock()

			stopped = append(stopped, "container-1")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/containers/container-1") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/volumes") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(volume.ListResponse{
				Volumes: []volume.Volume{{
					Name:   "shared-volume",
					Driver: "local",
					Labels: map[string]string{api.ProjectLabel: "stack-a"},
					Options: map[string]string{
						"type":   "nfs",
						"o":      "addr=10.0.0.1,vers=4",
						"device": ":/exports/data",
					},
				}},
			})
		case strings.Contains(r.URL.Path, "/volumes/shared-volume") && r.Method == http.MethodDelete:
			mu.Lock()

			deleted = append(deleted, "shared-volume")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cli, err := client.New(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithAPIVersion("1.52"),
	)
	if err != nil {
		t.Fatalf("failed to create docker api client: %v", err)
	}

	project := &types.Project{
		Volumes: types.Volumes{
			"shared-volume": {
				Name:   "shared-volume",
				Driver: "local",
				DriverOpts: types.Options{
					"type":   "nfs",
					"o":      "addr=10.0.0.2,vers=4",
					"device": ":/exports/data",
				},
			},
		},
	}

	if err := removeMismatchedRecreatableVolumes(context.Background(), cli, "stack-a", project); err != nil {
		t.Fatalf("removeMismatchedRecreatableVolumes() unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(stopped) != 1 || stopped[0] != "container-1" {
		t.Fatalf("expected running container to be stopped first, got %#v", stopped)
	}

	if len(deleted) != 1 || deleted[0] != "shared-volume" {
		t.Fatalf("expected volume removal after stopping container, got %#v", deleted)
	}

	stopIdx, deleteIdx := -1, -1

	for i, req := range requestSeen {
		if strings.Contains(req, "POST ") && strings.Contains(req, "/containers/container-1/stop") {
			stopIdx = i
		}

		if strings.Contains(req, "DELETE ") && strings.Contains(req, "/volumes/shared-volume") {
			deleteIdx = i
		}
	}

	if stopIdx == -1 || deleteIdx == -1 || stopIdx > deleteIdx {
		t.Fatalf("expected stop request before delete request, got sequence=%v", requestSeen)
	}
}

func TestRemoveMismatchedRecreatableVolumes_DoesNotStopContainerOnSourcePathOnlyMatch(t *testing.T) {
	t.Parallel()

	var (
		stopCalled   bool
		deleteCalled bool
	)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"Id":     "container-1",
					"Names":  []string{"/stack-a-app-1"},
					"State":  "running",
					"Labels": map[string]string{api.ProjectLabel: "stack-a"},
					// Source can contain host paths for bind mounts; this must not be used for named-volume matching.
					"Mounts": []map[string]any{{"Name": "", "Source": "shared-volume"}},
				},
			})
		case strings.Contains(r.URL.Path, "/containers/container-1/stop") && r.Method == http.MethodPost:
			stopCalled = true

			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/volumes") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(volume.ListResponse{
				Volumes: []volume.Volume{{
					Name:   "shared-volume",
					Driver: "local",
					Labels: map[string]string{api.ProjectLabel: "stack-a"},
					Options: map[string]string{
						"type": "nfs",
						"o":    "addr=10.0.0.1,vers=4",
					},
				}},
			})
		case strings.Contains(r.URL.Path, "/volumes/shared-volume") && r.Method == http.MethodDelete:
			deleteCalled = true

			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cli, err := client.New(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithAPIVersion("1.52"),
	)
	if err != nil {
		t.Fatalf("failed to create docker api client: %v", err)
	}

	project := &types.Project{
		Volumes: types.Volumes{
			"shared-volume": {
				Name:   "shared-volume",
				Driver: "local",
				DriverOpts: types.Options{
					"type": "nfs",
					"o":    "addr=10.0.0.2,vers=4",
				},
			},
		},
	}

	if err := removeMismatchedRecreatableVolumes(context.Background(), cli, "stack-a", project); err != nil {
		t.Fatalf("removeMismatchedRecreatableVolumes() unexpected error: %v", err)
	}

	if stopCalled {
		t.Fatal("expected container stop to be skipped when mount name does not match volume name")
	}

	if !deleteCalled {
		t.Fatal("expected volume removal for changed recreatable volume")
	}
}

func TestRemoveMismatchedRecreatableVolumes_ReturnsErrorWhenStoppingContainerFails(t *testing.T) {
	t.Parallel()

	var deleteCalled bool

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"Id":     "container-1",
					"Names":  []string{"/stack-a-app-1"},
					"State":  "running",
					"Labels": map[string]string{api.ProjectLabel: "stack-a"},
					"Mounts": []map[string]any{{"Name": "shared-volume", "Source": "shared-volume"}},
				},
			})
		case strings.Contains(r.URL.Path, "/containers/container-1/stop") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"stop failed"}`))
		case strings.HasSuffix(r.URL.Path, "/volumes") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(volume.ListResponse{
				Volumes: []volume.Volume{{
					Name:    "shared-volume",
					Driver:  "local",
					Labels:  map[string]string{api.ProjectLabel: "stack-a"},
					Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.1"},
				}},
			})
		case strings.Contains(r.URL.Path, "/volumes/shared-volume") && r.Method == http.MethodDelete:
			deleteCalled = true

			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cli, err := client.New(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithAPIVersion("1.52"),
	)
	if err != nil {
		t.Fatalf("failed to create docker api client: %v", err)
	}

	project := &types.Project{
		Volumes: types.Volumes{
			"shared-volume": {
				Name:       "shared-volume",
				Driver:     "local",
				DriverOpts: types.Options{"type": "nfs", "o": "addr=10.0.0.2"},
			},
		},
	}

	err = removeMismatchedRecreatableVolumes(context.Background(), cli, "stack-a", project)
	if err == nil {
		t.Fatal("expected error when stopping container fails")
	}

	if !strings.Contains(err.Error(), "failed to stop container") {
		t.Fatalf("expected stop-container error, got: %v", err)
	}

	if deleteCalled {
		t.Fatal("expected volume delete to be skipped when container stop fails")
	}
}

func TestRemoveMismatchedRecreatableVolumes_RemovesExitedContainersBeforeVolumeDelete(t *testing.T) {
	t.Parallel()

	var (
		stopCalled          bool
		containerDeleteCall bool
		volumeDeleteCall    bool
	)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"Id":     "container-1",
					"Names":  []string{"/stack-a-app-1"},
					"State":  "exited",
					"Labels": map[string]string{api.ProjectLabel: "stack-a"},
					"Mounts": []map[string]any{{"Name": "shared-volume", "Source": "shared-volume"}},
				},
			})
		case strings.Contains(r.URL.Path, "/containers/container-1/stop") && r.Method == http.MethodPost:
			stopCalled = true

			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/containers/container-1") && r.Method == http.MethodDelete:
			containerDeleteCall = true

			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/volumes") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(volume.ListResponse{
				Volumes: []volume.Volume{{
					Name:    "shared-volume",
					Driver:  "local",
					Labels:  map[string]string{api.ProjectLabel: "stack-a"},
					Options: map[string]string{"type": "tmpfs", "o": "size=64m"},
				}},
			})
		case strings.Contains(r.URL.Path, "/volumes/shared-volume") && r.Method == http.MethodDelete:
			volumeDeleteCall = true

			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cli, err := client.New(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithAPIVersion("1.52"),
	)
	if err != nil {
		t.Fatalf("failed to create docker api client: %v", err)
	}

	project := &types.Project{
		Volumes: types.Volumes{
			"shared-volume": {
				Name:       "shared-volume",
				Driver:     "local",
				DriverOpts: types.Options{"type": "tmpfs", "o": "size=128m"},
			},
		},
	}

	err = removeMismatchedRecreatableVolumes(context.Background(), cli, "stack-a", project)
	if err != nil {
		t.Fatalf("removeMismatchedRecreatableVolumes() unexpected error: %v", err)
	}

	if stopCalled {
		t.Fatal("expected stop to be skipped for exited containers")
	}

	if !containerDeleteCall {
		t.Fatal("expected exited container to be removed before volume delete")
	}

	if !volumeDeleteCall {
		t.Fatal("expected volume delete for changed recreatable volume")
	}
}
