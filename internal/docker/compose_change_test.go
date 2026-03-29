package docker

import (
	"reflect"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestParseRecreateIgnore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    ignoreCfg
		wantErr bool
	}{
		{
			name:  "valid config",
			input: `{configs: [app, nginx], secrets: [db], bindMounts: []}`,
			want: ignoreCfg{
				changeScopeConfigs:    {"app", "nginx"},
				changeScopeSecrets:    {"db"},
				changeScopeBindMounts: {},
			},
			wantErr: false,
		},
		{
			name: "valid multiline config",
			input: `
configs:
  - app
  - nginx
secrets:
  - db
bindMounts: []
`,
			want: ignoreCfg{
				changeScopeConfigs:    {"app", "nginx"},
				changeScopeSecrets:    {"db"},
				changeScopeBindMounts: {},
			},
			wantErr: false,
		},
		{
			name:    "invalid yaml",
			input:   `configs=app,nginx`,
			want:    nil,
			wantErr: true,
		},
		{
			name:    "valid empty config",
			input:   ``,
			want:    ignoreCfg{},
			wantErr: false,
		},
		{
			name:    "empty config",
			input:   ``,
			want:    ignoreCfg{},
			wantErr: false,
		},
		{
			name:  "valid config with empty items",
			input: `{configs: [], secrets: []}`,
			want: ignoreCfg{
				changeScopeConfigs: {},
				changeScopeSecrets: {},
			},
			wantErr: false,
		},
		{
			name:  "valid config with null",
			input: `{configs: null, secrets: null}`,
			want: ignoreCfg{
				changeScopeConfigs: nil,
				changeScopeSecrets: nil,
			},
			wantErr: false,
		},
		{
			name:    "duplicate scope items",
			input:   `{configs: [app, app]}`,
			want:    ignoreCfg{},
			wantErr: true,
		},
		{
			name:  "bindMounts",
			input: `{bindMounts: []}`,
			want: ignoreCfg{
				changeScopeBindMounts: {},
			},
			wantErr: false,
		},
		{
			name:  "bindMounts with paths",
			input: `{bindMounts: [/, /data]}`,
			want: ignoreCfg{
				changeScopeBindMounts: {"/", "/data"},
			},
			wantErr: false,
		},
		{
			name:    "wrong scope",
			input:   `{envFiles: []} `,
			wantErr: true,
		},
		{
			name:    "wrong scope",
			input:   `{buildFiles: []} `,
			wantErr: true,
		},
		{
			name:    "unknown scope",
			input:   `{unknownScope: []}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, gotErr := parseRecreateIgnore(tt.input)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("parseRecreateIgnore() failed: %v", gotErr)
				}

				return
			}

			if tt.wantErr {
				t.Fatal("parseRecreateIgnore() succeeded unexpectedly")
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseRecreateIgnore() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGetIgnoreRecreateCfgFromProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		project *types.Project
		want    projectIgnoreCfg
		wantErr bool
	}{
		{
			name:    "empty project",
			project: &types.Project{},
			want:    projectIgnoreCfg{},
			wantErr: false,
		},
		{
			name: "no services have recreate-ignore config",
			project: &types.Project{
				Services: types.Services{
					"svc1": types.ServiceConfig{
						Name: "svc1",
					},
				},
			},
			want:    projectIgnoreCfg{},
			wantErr: false,
		},
		{
			name: "two services, one service have empty recreate-ignore config",
			project: &types.Project{
				Services: types.Services{
					"svc1": types.ServiceConfig{
						Name: "svc1",
						Labels: map[string]string{
							"label.a.b.c.d": "",
						},
					},
					"svc2": types.ServiceConfig{
						Name: "svc2",
						Labels: map[string]string{
							DocoCDLabels.Deployment.RecreateIgnore: "",
						},
					},
				},
			},
			want:    projectIgnoreCfg{},
			wantErr: true,
		},
		{
			name: "two services, one service have no-empty recreate-ignore config",
			project: &types.Project{
				Services: types.Services{
					"svc1": types.ServiceConfig{
						Name: "svc1",
						Labels: map[string]string{
							"label.a.b.c.d": "",
						},
					},
					"svc2": types.ServiceConfig{
						Name: "svc2",
						Labels: map[string]string{
							DocoCDLabels.Deployment.RecreateIgnore:       "{configs: [app], secrets: []}",
							DocoCDLabels.Deployment.RecreateIgnoreSignal: "SIGHUP",
						},
					},
				},
			},
			want: projectIgnoreCfg{
				"svc2": {
					signal: "SIGHUP",
					ignoreMap: ignoreCfg{
						changeScopeConfigs: []string{"app"},
						changeScopeSecrets: {},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "ignoreSignal is empty",
			project: &types.Project{
				Services: types.Services{
					"svc2": types.ServiceConfig{
						Name: "svc2",
						Labels: map[string]string{
							DocoCDLabels.Deployment.RecreateIgnore:       "{configs: [app], secrets: []}",
							DocoCDLabels.Deployment.RecreateIgnoreSignal: " ",
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "ignore is empty, but ignoreSignal is exist",
			project: &types.Project{
				Services: types.Services{
					"svc2": types.ServiceConfig{
						Name: "svc2",
						Labels: map[string]string{
							DocoCDLabels.Deployment.RecreateIgnore:       " ",
							DocoCDLabels.Deployment.RecreateIgnoreSignal: "SIGHUP",
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "ignore is not exist, but ignoreSignal is exist",
			project: &types.Project{
				Services: types.Services{
					"svc2": types.ServiceConfig{
						Name: "svc2",
						Labels: map[string]string{
							DocoCDLabels.Deployment.RecreateIgnoreSignal: "SIGHUP",
						},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, gotErr := getIgnoreRecreateCfgFromProject(tt.project)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("getIgnoreRecreateCfgFromProject() failed: %v", gotErr)
				}

				return
			}

			if tt.wantErr {
				t.Fatal("getIgnoreRecreateCfgFromProject() succeeded unexpectedly")
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getIgnoreRecreateCfgFromProject() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckIsIgnoreByCfg(t *testing.T) {
	t.Parallel()

	cfg := projectIgnoreCfg{
		"svc1": {
			ignoreMap: ignoreCfg{
				changeScopeConfigs: []string{"app"},
				changeScopeSecrets: {},
			},
		},
	}

	tests := []struct {
		name      string
		ignoreCfg projectIgnoreCfg
		svc       string
		scope     changeScope
		item      string
		want      bool
	}{
		{
			name:      "svc not found",
			ignoreCfg: cfg,
			svc:       "svc2",
			scope:     changeScopeConfigs,
			want:      false,
		},

		{
			name:      "svc scope not found",
			ignoreCfg: cfg,
			svc:       "svc1",
			scope:     changeScopeBindMounts,
			want:      false,
		},
		{
			name:      "svc scope found",
			ignoreCfg: cfg,
			svc:       "svc1",
			scope:     changeScopeConfigs,
			item:      "app",
			want:      true,
		},
		{
			name:      "svc scope found not match",
			ignoreCfg: cfg,
			svc:       "svc1",
			scope:     changeScopeConfigs,
			item:      "app2",
			want:      false,
		},
		{
			name:      "svc scope ignore all",
			ignoreCfg: cfg,
			svc:       "svc1",
			scope:     changeScopeSecrets,
			item:      "abcd",
			want:      true,
		},
		{
			name:      "svc scope ignore all",
			ignoreCfg: cfg,
			svc:       "svc1",
			scope:     changeScopeSecrets,
			want:      true,
		},
		{
			name: "svc bindMounts is ignore",
			ignoreCfg: projectIgnoreCfg{
				"svc1": {
					ignoreMap: ignoreCfg{
						changeScopeBindMounts: []string{"/"},
					},
				},
			},
			svc:   "svc1",
			item:  "/",
			scope: changeScopeBindMounts,
			want:  true,
		},
		{
			name: "svc bindMounts is not ignore",
			ignoreCfg: projectIgnoreCfg{
				"svc1": {
					ignoreMap: ignoreCfg{
						changeScopeBindMounts: []string{"/"},
					},
				},
			},
			svc:   "svc1",
			item:  "/data",
			scope: changeScopeBindMounts,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkIsIgnoreByCfg(tt.ignoreCfg, tt.svc, tt.scope, tt.item)

			if got != tt.want {
				t.Errorf("getIgnoreCfgByServiceNameAndScope() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetChangeAndIgnore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		changed     []string
		ignored     []string
		wantChanged []string
		wantIgnored []string
	}{
		{
			name:        "only changed",
			changed:     []string{"1"},
			ignored:     []string{},
			wantChanged: []string{"1"},
			wantIgnored: []string{},
		},
		{
			name:        "only ignored",
			changed:     []string{},
			ignored:     []string{"1"},
			wantChanged: []string{},
			wantIgnored: []string{"1"},
		},
		{
			name:        "both changed and ignored",
			changed:     []string{"1"},
			ignored:     []string{"2"},
			wantChanged: []string{"1"},
			wantIgnored: []string{"2"},
		},
		{
			name:        "changed include all ignored",
			changed:     []string{"1", "2"},
			ignored:     []string{"1"},
			wantChanged: []string{"1", "2"},
			wantIgnored: []string{},
		},
		{
			name:        "ignored include all changed",
			changed:     []string{"1", "2"},
			ignored:     []string{"1", "2", "3"},
			wantChanged: []string{"1", "2"},
			wantIgnored: []string{"3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, got2 := getChangeAndIgnore(tt.changed, tt.ignored)
			slices.Sort(got)
			slices.Sort(got2)
			slices.Sort(tt.wantChanged)
			slices.Sort(tt.wantIgnored)

			if !reflect.DeepEqual(got, tt.wantChanged) {
				t.Errorf("getChangeAndIgnore() = %v, want %v", got, tt.wantChanged)
			}

			if !reflect.DeepEqual(got2, tt.wantIgnored) {
				t.Errorf("getChangeAndIgnore() = %v, want %v", got2, tt.wantIgnored)
			}
		})
	}
}
