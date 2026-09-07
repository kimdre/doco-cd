package docker

import (
	"errors"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestSelectRecreateServices(t *testing.T) {
	project := &types.Project{
		Name: "example",
		Services: types.Services{
			"web": {
				Name: "web",
				DependsOn: types.DependsOnConfig{
					"db": {},
				},
			},
			"db":       {Name: "db"},
			"inactive": {Name: "inactive"},
		},
	}

	t.Run("specific service includes dependencies", func(t *testing.T) {
		selected, targets, err := selectRecreateServices(project, "web", nil)
		if err != nil {
			t.Fatal(err)
		}

		if len(selected.Services) != 2 {
			t.Fatalf("selected services = %v, want web and db", selected.ServiceNames())
		}

		if len(targets) != 1 || targets[0] != "web" {
			t.Fatalf("recreate targets = %v, want [web]", targets)
		}
	})

	t.Run("whole project excludes inactive profiles", func(t *testing.T) {
		selected, targets, err := selectRecreateServices(project, "", []string{"web"})
		if err != nil {
			t.Fatal(err)
		}

		if len(selected.Services) != 2 {
			t.Fatalf("selected services = %v, want web and db", selected.ServiceNames())
		}

		if targets != nil {
			t.Fatalf("recreate targets = %v, want nil", targets)
		}
	})

	t.Run("missing service", func(t *testing.T) {
		_, _, err := selectRecreateServices(project, "missing", nil)
		if !errors.Is(err, ErrComposeServiceNotFound) {
			t.Fatalf("error = %v, want ErrComposeServiceNotFound", err)
		}
	})
}
