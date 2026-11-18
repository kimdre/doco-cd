package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func TestSwarmOneOffServiceName(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 1730000000000000000)
	suffix := "-doco-job-1730000000000000000"

	testCases := []struct {
		name              string
		sourceServiceName string
		want              string
	}{
		{
			name:              "keeps short source service name",
			sourceServiceName: "stack_backup",
			want:              "stack_backup" + suffix,
		},
		{
			name:              "truncates source service name to Docker limit",
			sourceServiceName: strings.Repeat("a", maxSwarmServiceNameLength),
			want:              strings.Repeat("a", maxSwarmServiceNameLength-len(suffix)) + suffix,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := swarmOneOffServiceName(tc.sourceServiceName, now)

			if got != tc.want {
				t.Errorf("swarmOneOffServiceName() = %q, want %q", got, tc.want)
			}

			if len(got) > maxSwarmServiceNameLength {
				t.Errorf("swarmOneOffServiceName() length = %d, want <= %d", len(got), maxSwarmServiceNameLength)
			}
		})
	}
}

func TestRunSwarmJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if !resolveTestSwarmMode(t.Context(), t, dockerCli.Client()) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	testCases := []struct {
		mode    swarm.DeployMode
		command []string
		title   string
		wantErr string
	}{
		{mode: swarm.DeployModeGlobalJob, command: []string{"docker", "info"}, title: "global-docker-info"},
		{mode: swarm.DeployModeReplicatedJob, command: []string{"docker", "info"}, title: "replicated-docker-info"},
		{mode: swarm.DeployModeReplicatedJob, command: []string{"sh", "-c", "exit 7"}, title: "replicated-exit-7", wantErr: "exit code 7"},
	}

	for _, tc := range testCases {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()

			t.Logf("Running job with mode: %s, command: %v, title: %s", tc.mode, tc.command, tc.title)

			err := RunSwarmJob(t.Context(), dockerCli, tc.mode, tc.command, tc.title)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("RunSwarmJob failed: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("RunSwarmJob error = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

const fakeDockerScript = `#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_LOG"
case "$1 $2" in
"image prune") exit "$FAKE_PRUNE_EXIT" ;;
"image ls") printf '%s\n' "$FAKE_IMAGES" ;;
"image inspect")
	case "$5" in
	*fresh*) echo "$(date +%s) $5 $5" ;;
	*gone*) exit 1 ;;
	*graphdriver*) echo "1700000000 $5" ;;
	*aliased*) echo "1700000000 $5 other.example/app:1.0 $5 other.example/app@sha256:hhh" ;;
	*mirrored*) echo "1700000000 $5 mirror.example/app@sha256:iii $5 mirror.example/app@sha256:iii" ;;
	*) echo "1700000000 $5 $5" ;;
	esac ;;
"image rm")
	case "$3" in
	*inuse*) echo "Error response from daemon: conflict: unable to delete $3 (must be forced)" >&2; exit 1 ;;
	*broken*) echo "Error response from daemon: broken store" >&2; exit 1 ;;
	esac ;;
esac
`

func TestImagePruneScript(t *testing.T) {
	t.Parallel()

	images := strings.Join([]string{
		"<none> registry.example/replaced@sha256:aaa",
		"<none> registry.example/graphdriver@sha256:ggg",
		"<none> registry.example/inuse@sha256:bbb",
		"<none> registry.example/broken@sha256:ccc",
		"<none> registry.example/fresh@sha256:eee",
		"<none> registry.example/gone@sha256:fff",
		"<none> registry.example/aliased@sha256:hhh",
		"<none> registry.example/mirrored@sha256:iii",
		"1.0 registry.example/tagged@sha256:ddd",
		"<none> <none>@<none>",
	}, "\n")

	inspect := "image inspect --format {{.Metadata.LastTagTime.Unix}}{{range .RepoTags}} {{.}}{{end}}{{range .RepoDigests}} {{.}}{{end}} "

	testCases := []struct {
		name      string
		pruneExit string
		wantCalls []string
		wantErr   bool
		wantLog   string
	}{
		{
			name:      "removes only unused images known by a single digest name and pulled over an hour ago",
			pruneExit: "0",
			wantCalls: []string{
				"image prune --force",
				"image ls --digests --format {{.Tag}} {{.Repository}}@{{.Digest}}",
				inspect + "registry.example/replaced@sha256:aaa",
				"image rm registry.example/replaced@sha256:aaa",
				inspect + "registry.example/graphdriver@sha256:ggg",
				"image rm registry.example/graphdriver@sha256:ggg",
				inspect + "registry.example/inuse@sha256:bbb",
				"image rm registry.example/inuse@sha256:bbb",
				inspect + "registry.example/broken@sha256:ccc",
				"image rm registry.example/broken@sha256:ccc",
				inspect + "registry.example/fresh@sha256:eee",
				inspect + "registry.example/gone@sha256:fff",
				inspect + "registry.example/aliased@sha256:hhh",
				inspect + "registry.example/mirrored@sha256:iii",
			},
			wantLog: "broken store",
		},
		{
			name:      "stops when prune fails",
			pruneExit: "1",
			wantCalls: []string{"image prune --force"},
			wantErr:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			logPath := filepath.Join(dir, "calls.log")

			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDockerScript), 0o700); err != nil { // #nosec G306
				t.Fatalf("write fake docker: %v", err)
			}

			cmd := exec.CommandContext(t.Context(), "sh", "-c", imagePruneScript) // #nosec G204

			cmd.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_DOCKER_LOG="+logPath,
				"FAKE_PRUNE_EXIT="+tc.pruneExit,
				"FAKE_IMAGES="+images,
			)

			var stderr strings.Builder

			cmd.Stderr = &stderr

			err := cmd.Run()
			if (err != nil) != tc.wantErr {
				t.Fatalf("script error = %v, wantErr %v, stderr %q", err, tc.wantErr, stderr.String())
			}

			calls, err := os.ReadFile(logPath) // #nosec G304
			if err != nil {
				t.Fatalf("read calls: %v", err)
			}

			if got := strings.Split(strings.TrimSpace(string(calls)), "\n"); !slices.Equal(got, tc.wantCalls) {
				t.Errorf("docker calls = %q, want %q", got, tc.wantCalls)
			}

			if strings.Contains(stderr.String(), "conflict") {
				t.Errorf("stderr reports an in-use conflict: %q", stderr.String())
			}

			if !strings.Contains(stderr.String(), tc.wantLog) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantLog)
			}
		})
	}
}

func TestRunImagePruneJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if !resolveTestSwarmMode(t.Context(), t, dockerCli.Client()) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	err = RunImagePruneJob(t.Context(), dockerCli)
	if err != nil {
		t.Errorf("RunImagePruneJob failed: %v", err)
	}
}
