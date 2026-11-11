//go:build e2e

package e2e

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
)

const (
	legacySelfImage = "ghcr.io/kimdre/doco-cd:0.133.2@sha256:8ca3cf6701f70ac3af4c5d726e542fe3a854a038149375f87222ec2719b3c975"
	legacyAPIKey    = "e2e-legacy-api-key" // #nosec G101 -- this is a fixed test credential.
	readerAPIKey    = "e2e-read-api-key"   // #nosec G101 -- this is a fixed test credential.
	legacyAPIEnv    = "      API_SECRET: e2e-legacy-api-key\n"
	roleAPIEnv      = "      API_KEYS: |\n        - key: e2e-legacy-api-key\n          role: full\n        - key: e2e-read-api-key\n          role: read\n"
)

func TestSelfUpdateAPIKeyMigration(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	h := NewHarness(t, "self-update-api-keys")
	h.EnableSelfUpdate("e2e-self-update-api-keys", "doco-cd")
	h.SetSelfUpdatePredecessorImage(legacySelfImage)
	h.Start()

	t.Cleanup(func() {
		if t.Failed() {
			h.dumpSelfState()
		}
	})

	if h.SelfImageID("v1") == h.SelfImageID("v2") {
		t.Fatal("predecessor and successor use the same image")
	}

	oldID := h.SelfContainerID()
	assertSelfAPIEnvironment(t, h, oldID, true)
	assertSelfAPIStatus(t, h, oldID, http.MethodGet, "/v1/api/runs", legacyAPIKey, http.StatusOK)
	assertSelfAPIStatus(t, h, oldID, http.MethodGet, "/v1/api/runs", readerAPIKey, http.StatusUnauthorized)

	mark := h.LogMark()
	h.WaitForLogOccurrencesAfter("no changes detected, skipping deployment", mark, 2, 90*time.Second)

	h.ReplaceInWorktree("deploy/compose.yaml", ":v1", ":v2")
	h.ReplaceInWorktree("deploy/compose.yaml", legacyAPIEnv, legacyAPIEnv+roleAPIEnv)
	mark = h.LogMark()
	h.RepoPush("attempt an upgrade with both legacy and new settings")

	h.WaitFor(4*time.Minute, "the failed migration is rolled back", func() bool {
		logs := h.logsSince(mark)

		return strings.Contains(logs, "self-update rolled back, this instance keeps running") ||
			strings.Contains(logs, "self-update: rolled back")
	})
	h.WaitFor(2*time.Minute, "the legacy container is healthy after rollback", func() bool {
		running := h.SelfContainers(false)

		return len(running) == 1 && len(h.SelfContainers(true)) == 1 &&
			len(h.SelfAppliers(true)) == 0 && h.ContainerHealthy(running[0].ID)
	})
	oldID = h.SelfContainerID()

	if !h.RunsSelfImage(oldID, "v1") {
		t.Fatal("failed migration did not restore the predecessor image")
	}

	assertSelfAPIEnvironment(t, h, oldID, true)
	assertSelfAPIStatus(t, h, oldID, http.MethodGet, "/v1/api/runs", legacyAPIKey, http.StatusOK)
	assertSelfAPIStatus(t, h, oldID, http.MethodGet, "/v1/api/runs", readerAPIKey, http.StatusUnauthorized)

	h.ReplaceInWorktree("deploy/compose.yaml", legacyAPIEnv+roleAPIEnv, roleAPIEnv)
	mark = h.LogMark()
	h.RepoPush("migrate API keys with the image")

	newID := waitForSelfAPIHandover(t, h, oldID, mark)
	assertSelfAPIEnvironment(t, h, newID, false)
	assertSelfAPIStatus(t, h, newID, http.MethodGet, "/v1/api/runs", legacyAPIKey, http.StatusOK)
	assertSelfAPIStatus(t, h, newID, http.MethodGet, "/v1/api/runs", readerAPIKey, http.StatusOK)
	assertSelfAPIStatus(t, h, newID, http.MethodDelete, "/v1/api/project/e2e-nonexistent?timeout=invalid", readerAPIKey, http.StatusForbidden)
	assertSelfAPIStatus(t, h, newID, http.MethodDelete, "/v1/api/project/e2e-nonexistent?timeout=invalid", legacyAPIKey, http.StatusBadRequest)

	h.ReplaceInWorktree("deploy/compose.yaml", ":v2", ":v1")
	h.ReplaceInWorktree("deploy/compose.yaml", roleAPIEnv, legacyAPIEnv)
	mark = h.LogMark()
	h.RepoPush("restore the legacy image and API secret")

	rollbackID := waitForSelfAPIHandover(t, h, newID, mark)
	assertSelfAPIEnvironment(t, h, rollbackID, true)
	assertSelfAPIStatus(t, h, rollbackID, http.MethodGet, "/v1/api/runs", legacyAPIKey, http.StatusOK)
	assertSelfAPIStatus(t, h, rollbackID, http.MethodGet, "/v1/api/runs", readerAPIKey, http.StatusUnauthorized)
	h.assertNoSelfLeftovers()
}

func waitForSelfAPIHandover(t *testing.T, h *Harness, previousID string, mark int) string {
	t.Helper()

	h.WaitFor(4*time.Minute, "self-update successor is healthy", func() bool {
		running := h.SelfContainers(false)
		return len(running) == 1 && running[0].ID != previousID && h.ContainerHealthy(running[0].ID)
	})
	h.WaitForLogAfter("self-update finalised", mark, 2*time.Minute)
	h.WaitFor(time.Minute, "self-update predecessor is removed", func() bool {
		return !h.ContainerExists(previousID) && len(h.SelfContainers(true)) == 1
	})

	return h.SelfContainerID()
}

func assertSelfAPIEnvironment(t *testing.T, h *Harness, containerID string, legacy bool) {
	t.Helper()

	result, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect self-update container: %v", err)
	}

	var hasLegacy, hasKeys bool

	for _, entry := range result.Container.Config.Env {
		hasLegacy = hasLegacy || strings.HasPrefix(entry, "API_SECRET=") || strings.HasPrefix(entry, "API_SECRET_FILE=")
		hasKeys = hasKeys || strings.HasPrefix(entry, "API_KEYS=") || strings.HasPrefix(entry, "API_KEYS_FILE=")
	}

	if hasLegacy != legacy || hasKeys == legacy {
		t.Fatalf("self-update environment: legacy=%t, keys=%t, want legacy=%t", hasLegacy, hasKeys, legacy)
	}
}

func assertSelfAPIStatus(t *testing.T, h *Harness, containerID, method, path, key string, want int) {
	t.Helper()

	result, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect self-update container: %v", err)
	}

	if result.Container.NetworkSettings == nil || result.Container.NetworkSettings.Networks[h.net.Name] == nil {
		t.Fatal("self-update container has no scenario network")
	}

	address := result.Container.NetworkSettings.Networks[h.net.Name].IPAddress.String()

	output, err := docker.ExecContext(h.ctx, h.docker, h.gitSrv.GetContainerID(),
		"curl", "--silent", "--show-error", "--output", "/dev/null", "--write-out", "%{http_code}",
		"--max-time", "10", "--request", method, "--header", "x-api-key: "+key, "http://"+address+path)
	if err != nil {
		t.Fatalf("probe self-update API: %v", err)
	}

	status, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || status != want {
		t.Fatalf("self-update API %s %s: status = %q, want %d", method, path, output, want)
	}
}
