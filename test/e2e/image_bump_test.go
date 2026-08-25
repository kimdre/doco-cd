//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestImageBump(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scenario   string
		stack      string
		oldImage   string
		newImage   string
		commitText string
	}{
		{
			name:       "tag",
			scenario:   "image-tag-bump",
			stack:      "e2e-image-tag-bump",
			oldImage:   "alpine:3.21",
			newImage:   "alpine:3.22",
			commitText: "bump app image tag",
		},
		{
			name:       "digest",
			scenario:   "image-digest-bump",
			stack:      "e2e-image-digest-bump",
			oldImage:   "sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d",
			newImage:   "sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce",
			commitText: "bump app image digest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := NewHarness(t, tc.scenario)
			h.Start()

			h.WaitFor(120*time.Second, "initial app container", func() bool {
				return h.ContainerID(tc.stack, "app") != ""
			})

			h.ReplaceInWorktree("deploy/compose.yaml", tc.oldImage, tc.newImage)
			h.RepoPush(tc.commitText)

			h.WaitForLog(`"msg":"service image reference changed"`, 120*time.Second)
		})
	}
}
