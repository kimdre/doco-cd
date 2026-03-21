package docker

import (
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func Test_parseRecreateIgnore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    map[changeScope]changeIgnoreRule
		wantErr bool
	}{
		{
			name:  "valid config",
			input: `configs=app|nginx,secrets=db,bindMounts`,
			want: map[changeScope]changeIgnoreRule{
				changeScopeConfigs: {
					Items: []string{"app", "nginx"},
				},
				changeScopeSecrets: {
					Items: []string{"db"},
				},
				changeScopeBindMounts: {
					Items: nil,
				},
			},
			wantErr: false,
		},
		{
			name:    "valid empty config",
			input:   ` `,
			want:    map[changeScope]changeIgnoreRule{},
			wantErr: false,
		},
		{
			name:    "empty config",
			input:   ``,
			want:    map[changeScope]changeIgnoreRule{},
			wantErr: false,
		},
		{
			name:  "valid config with empty items",
			input: `configs= ,secrets`,
			want: map[changeScope]changeIgnoreRule{
				changeScopeConfigs: {},
				changeScopeSecrets: {},
			},
			wantErr: false,
		},
		{
			name:    "duplicate scopes",
			input:   `configs=,configs=a`,
			want:    nil,
			wantErr: true,
		},
		{
			name:    "duplicate scope items",
			input:   `configs=a|a`,
			want:    map[changeScope]changeIgnoreRule{},
			wantErr: true,
		},
		{
			name:  "bindMounts",
			input: `bindMounts`,
			want: map[changeScope]changeIgnoreRule{
				changeScopeBindMounts: {},
			},
			wantErr: false,
		},
		{
			name:  "bindMounts with paths",
			input: `bindMounts=/|/data`,
			want: map[changeScope]changeIgnoreRule{
				changeScopeBindMounts: {
					Items: []string{"/", "/data"},
				},
			},
			wantErr: false,
		},
		{
			name:    "wrong scope",
			input:   `envFiles`,
			wantErr: true,
		},
		{
			name:    "wrong scope",
			input:   `buildFiles`,
			wantErr: true,
		},
		{
			name:    "unknown scope",
			input:   `xxxx`,
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
				t.Errorf("parseRecreateIgnore() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_getIgnoreRecrateCfgFromProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string // description of this test case
		// Named input parameters for target function.
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
			want: projectIgnoreCfg{
				"svc2": {},
			},
			wantErr: false,
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
							DocoCDLabels.Deployment.RecreateIgnore: "configs=app,secrets",
						},
					},
				},
			},
			want: projectIgnoreCfg{
				"svc2": {
					changeScopeConfigs: {
						Items: []string{"app"},
					},
					changeScopeSecrets: {},
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, gotErr := getIgnoreRecrateCfgFromProject(tt.project)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("getIgnoreRecrateCfgFromProject() failed: %v", gotErr)
				}

				return
			}

			if tt.wantErr {
				t.Fatal("getIgnoreRecrateCfgFromProject() succeeded unexpectedly")
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getIgnoreRecrateCfgFromProject() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_checkIsIgnoreByCfg(t *testing.T) {
	t.Parallel()

	cfg := projectIgnoreCfg{
		"svc1": {
			changeScopeConfigs: {
				Items: []string{"app"},
			},
			changeScopeSecrets: {},
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
			name:      "svc scope found",
			ignoreCfg: cfg,
			svc:       "svc1",
			scope:     changeScopeConfigs,
			item:      "app2",
			want:      false,
		},
		{
			name:      "svc scope found with empty",
			ignoreCfg: cfg,
			svc:       "svc1",
			scope:     changeScopeSecrets,
			want:      true,
		},
		{
			name: "svc scope found with empty",
			ignoreCfg: map[string]map[changeScope]changeIgnoreRule{
				"svc1": {
					changeScopeBindMounts: {
						Items: []string{"/"},
					},
				},
			},
			svc:   "svc1",
			item:  "/",
			scope: changeScopeBindMounts,
			want:  true,
		},
		{
			name: "svc scope found with empty",
			ignoreCfg: map[string]map[changeScope]changeIgnoreRule{
				"svc1": {
					changeScopeBindMounts: {
						Items: []string{"/"},
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
