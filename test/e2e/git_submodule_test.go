//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestGitSubmoduleDeployment checks that a repository whose stack depends on a
// file inside a git submodule deploys: the compose config source lives in the
// submodule, so the stack cannot be built unless the submodule was
// materialized during checkout. The submodule is registered with a relative
// URL, which historically required the git binary to resolve.
func TestGitSubmoduleDeployment(t *testing.T) {
	t.Parallel()

	const (
		stack   = "e2e-git-submodule"
		service = "app"
	)

	h := NewHarness(t, "git-submodule")

	shared := h.NewSideRepo("git-submodule-shared")
	shared.Write(map[string]string{"shared.txt": "from the submodule\n"})
	h.SetSubmoduleGitlink("shared", shared.Commit("e2e: submodule fixture"))

	h.Start()

	h.WaitFor(2*time.Minute, "app container from submodule-backed stack", func() bool {
		return h.ContainerID(stack, service) != ""
	})
}
