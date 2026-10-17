package reconciliation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/stages"
)

func cleanupRetryTestRequest() DeployRequest {
	return DeployRequest{
		Logger:     slog.New(slog.DiscardHandler),
		JobTrigger: stages.JobTriggerWebhook,
		Repository: stages.RepositoryData{
			Name: "example.com/organization/repository", ResolvedReference: "main", Revision: "old",
		},
	}
}

func seedCleanupRetry(t *testing.T, m *Manager, req DeployRequest) DeployRequest {
	t.Helper()

	req.pauseGeneration = m.observePause(req)
	key := cleanupRetryKey(req)
	m.cleanupRetries.pending = map[string]*cleanupRetry{
		key: {request: req, next: time.Unix(0, 0), delay: cleanupRetryInitialDelay},
	}
	m.cleanupRetries.generations = map[string]uint64{key: req.pauseGeneration}
	t.Cleanup(m.stopCleanupRetries)

	return req
}

func startCleanupRetryTestLoop(t *testing.T, m *Manager,
	cleanup func(context.Context, DeployRequest, *syncWindowGate) (bool, error),
) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m.cleanupRetries.cancel = cancel
	m.cleanupRetries.wake = make(chan struct{}, 1)
	m.cleanupRetries.done = make(chan struct{})
	t.Cleanup(m.stopCleanupRetries)

	go m.runCleanupRetries(ctx, cleanup)
}

func TestCleanupRetryScheduling(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"complete", "no refresh", "test run", "unobserved", "stopped"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := &Manager{}
			req := cleanupRetryTestRequest()
			req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
				t.Fatal("ineligible request refreshed")

				return DeployRequest{}, nil, nil
			}
			req.pauseGeneration = m.observePause(req)
			pending := true

			switch name {
			case "complete":
				pending = false
			case "no refresh":
				req.RefreshCleanup = nil
			case "test run":
				req.TestName = "test"
			case "unobserved":
				req.pauseGeneration = 0
			case "stopped":
				m.stopCleanupRetries()
			}

			m.scheduleCleanupRetry(t.Context(), req, pending)
			m.stopCleanupRetries()

			if len(m.cleanupRetries.pending) != 0 || m.cleanupRetries.done != nil {
				t.Fatal("ineligible request started the cleanup worker")
			}
		})
	}
}

func TestCleanupRetryRepeatedObservationKeepsBackoff(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	req := cleanupRetryTestRequest()
	req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) { return req, nil, nil }
	req = seedCleanupRetry(t, m, req)
	entry := m.cleanupRetries.pending[cleanupRetryKey(req)]
	entry.delay = cleanupRetryMaxDelay
	entry.next = time.Now().Add(entry.delay)
	next := entry.next

	m.scheduleCleanupRetry(t.Context(), req, true)

	if m.cleanupRetries.pending[cleanupRetryKey(req)] != entry ||
		entry.next != next || entry.delay != cleanupRetryMaxDelay {
		t.Fatal("repeated event replaced or postponed pending cleanup")
	}
}

func TestCleanupRetryRefreshesAndReleasesAfterCleanup(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	req := cleanupRetryTestRequest()
	req.Repository.OCITrusted = true
	released, refreshed, cleaned := 0, 0, 0
	req.RefreshCleanup = func(ctx context.Context) (DeployRequest, func(), error) {
		refreshed++

		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > cleanupRetryTimeout {
			t.Fatal("source refresh has no bounded deadline")
		}

		fresh := req
		fresh.Repository.Revision = "new"
		fresh.Repository.OCITrusted = false
		fresh.DeployConfigs = []*deployConfig.Config{{Name: "new", Pause: true}}
		fresh.RefreshCleanup = nil

		return fresh, func() { released++ }, nil
	}
	req = seedCleanupRetry(t, m, req)

	cleanup := func(ctx context.Context, fresh DeployRequest, _ *syncWindowGate) (bool, error) {
		cleaned++

		if ctx.Err() != nil || released != 0 {
			t.Fatal("source released before cleanup finished")
		}

		if fresh.Repository.Revision != "new" || fresh.Repository.OCITrusted ||
			len(fresh.DeployConfigs) != 1 || !fresh.DeployConfigs[0].Pause ||
			fresh.RefreshCleanup == nil || fresh.pauseGeneration <= req.pauseGeneration {
			t.Fatalf("cleanup did not receive the refreshed source: %+v", fresh)
		}

		if current := m.currentCleanupRequest(fresh); current.Repository.Revision != "new" {
			t.Fatal("cleanup still observes old source policy")
		}

		return false, nil
	}
	if !m.runDueCleanupRetry(t.Context(), time.Now(), cleanup) {
		t.Fatal("due cleanup was not attempted")
	}

	if refreshed != 1 || cleaned != 1 || released != 1 || len(m.cleanupRetries.pending) != 0 {
		t.Fatalf("refreshes=%d cleanups=%d releases=%d pending=%d",
			refreshed, cleaned, released, len(m.cleanupRetries.pending))
	}

	if len(m.jobs.jobs) != 0 {
		t.Fatal("cleanup-only retry registered a reconciliation job")
	}
}

func TestCleanupRetryFailuresBackOffAndKeepCallback(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	req := cleanupRetryTestRequest()

	var logs bytes.Buffer

	req.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	refreshes, releases := 0, 0
	req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
		refreshes++

		return DeployRequest{}, func() { releases++ }, errors.New("source unavailable")
	}
	req = seedCleanupRetry(t, m, req)
	cleanup := func(context.Context, DeployRequest, *syncWindowGate) (bool, error) {
		t.Fatal("cleanup ran after source refresh failed")

		return false, nil
	}

	for _, delay := range []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute,
		cleanupRetryMaxDelay, cleanupRetryMaxDelay,
	} {
		entry := m.cleanupRetries.pending[cleanupRetryKey(req)]
		if !m.runDueCleanupRetry(t.Context(), entry.next, cleanup) {
			t.Fatal("due failure retry not attempted")
		}

		if entry.delay != delay || entry.request.RefreshCleanup == nil {
			t.Fatalf("delay=%s, want %s; refresh retained=%v",
				entry.delay, delay, entry.request.RefreshCleanup != nil)
		}

		if m.runDueCleanupRetry(t.Context(), entry.next.Add(-time.Nanosecond), cleanup) {
			t.Fatal("retry ran before its backoff elapsed")
		}
	}

	if refreshes != 5 || releases != 5 || !strings.Contains(logs.String(), "source unavailable") {
		t.Fatalf("refreshes=%d releases=%d logs=%s", refreshes, releases, logs.String())
	}
}

func TestCleanupRetryRetainsPendingAndFailedCleanup(t *testing.T) {
	t.Parallel()

	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "deferred", true: "failed"}[failure], func(t *testing.T) {
			t.Parallel()

			m := &Manager{}
			req := cleanupRetryTestRequest()
			refreshes, attempts, releases := 0, 0, 0
			req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
				refreshes++
				fresh := req
				fresh.Repository.Revision = "refreshed"
				fresh.RefreshCleanup = nil

				return fresh, func() { releases++ }, nil
			}
			req = seedCleanupRetry(t, m, req)
			cleanup := func(context.Context, DeployRequest, *syncWindowGate) (bool, error) {
				attempts++
				if attempts == 1 {
					if failure {
						return false, errors.New("removal failed")
					}

					return true, nil
				}

				return false, nil
			}
			m.runDueCleanupRetry(t.Context(), time.Now(), cleanup)

			entry := m.cleanupRetries.pending[cleanupRetryKey(req)]
			if entry == nil || entry.request.Repository.Revision != "refreshed" {
				t.Fatal("pending cleanup lost its refreshed request")
			}

			m.runDueCleanupRetry(t.Context(), entry.next, cleanup)

			if refreshes != 2 || attempts != 2 || releases != 2 || len(m.cleanupRetries.pending) != 0 {
				t.Fatalf("refreshes=%d attempts=%d releases=%d pending=%d",
					refreshes, attempts, releases, len(m.cleanupRetries.pending))
			}
		})
	}
}

func TestCleanupRetryUsesLatestRefreshCallback(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	req := cleanupRetryTestRequest()
	oldCalls, latestCalls := 0, 0
	latest := func(context.Context) (DeployRequest, func(), error) {
		latestCalls++
		fresh := req
		fresh.RefreshCleanup = nil

		return fresh, nil, nil
	}
	req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
		oldCalls++
		fresh := req
		fresh.RefreshCleanup = latest

		return fresh, nil, nil
	}
	req = seedCleanupRetry(t, m, req)
	pending := func(context.Context, DeployRequest, *syncWindowGate) (bool, error) { return true, nil }
	m.runDueCleanupRetry(t.Context(), time.Now(), pending)
	m.runDueCleanupRetry(t.Context(), m.cleanupRetries.pending[cleanupRetryKey(req)].next, pending)
	m.runDueCleanupRetry(t.Context(), m.cleanupRetries.pending[cleanupRetryKey(req)].next, pending)

	if oldCalls != 1 || latestCalls != 2 {
		t.Fatalf("old callback calls=%d latest callback calls=%d", oldCalls, latestCalls)
	}
}

func TestCleanupRetryRechecksSyncWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newSyncWindowTestManager(t, syncWindowTestPolicy)
		req := cleanupRetryTestRequest()
		refreshes, removals := 0, 0
		req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
			refreshes++

			return req, nil, nil
		}
		req = seedCleanupRetry(t, m, req)
		cleanup := func(_ context.Context, fresh DeployRequest, gate *syncWindowGate) (bool, error) {
			if !gate.allowRemoval(fresh.Logger, "", "web") {
				return true, nil
			}

			removals++

			return false, nil
		}

		time.Sleep(10 * time.Hour)
		m.runDueCleanupRetry(t.Context(), time.Now(), cleanup)

		if removals != 0 || len(m.cleanupRetries.pending) != 1 {
			t.Fatal("closed window did not keep cleanup pending")
		}

		time.Sleep(time.Hour)
		m.runDueCleanupRetry(t.Context(), time.Now(), cleanup)

		if refreshes != 2 || removals != 1 || len(m.cleanupRetries.pending) != 0 {
			t.Fatalf("refreshes=%d removals=%d pending=%d", refreshes, removals, len(m.cleanupRetries.pending))
		}
	})
}

func TestCleanupRetryCannotActOnSupersededRefresh(t *testing.T) {
	t.Parallel()

	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "new observation", true: "canceled entry"}[cancel], func(t *testing.T) {
			t.Parallel()

			m := &Manager{}
			req := cleanupRetryTestRequest()
			releases := 0

			var newer DeployRequest

			req.RefreshCleanup = func(ctx context.Context) (DeployRequest, func(), error) {
				newer = req
				newer.Repository.Revision = "newer"

				newer.pauseGeneration = m.observePause(newer)
				if cancel {
					m.cancelCleanupRetry(newer)
					m.scheduleCleanupRetry(ctx, newer, true)
				}

				return req, func() { releases++ }, nil
			}
			req = seedCleanupRetry(t, m, req)
			m.runDueCleanupRetry(t.Context(), time.Now(),
				func(context.Context, DeployRequest, *syncWindowGate) (bool, error) {
					t.Fatal("superseded source reached cleanup")

					return false, nil
				})

			if releases != 1 || m.currentCleanupRequest(req).Repository.Revision != "newer" {
				t.Fatal("superseded refresh replaced current source or leaked its release")
			}

			if cancel && m.cleanupRetries.pending[cleanupRetryKey(req)].request.pauseGeneration != newer.pauseGeneration {
				t.Fatal("stale attempt changed the newer pending request")
			}
		})
	}
}

func TestCleanupRetryScopesAndGenerationWatermarks(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	t.Cleanup(m.stopCleanupRetries)

	main := cleanupRetryTestRequest()
	main.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) { return main, nil, nil }
	main.pauseGeneration = m.observePause(main)
	alias := main

	alias.Repository.ResolvedReference = "refs/heads/main"
	if cleanupRetryKey(alias) != cleanupRetryKey(main) {
		t.Fatal("branch aliases created independent cleanup scopes")
	}

	m.cancelCleanupRetry(main)
	m.scheduleCleanupRetry(t.Context(), main, true)

	dev := main
	dev.Repository.ResolvedReference = "dev"
	dev.pauseGeneration = m.observePause(dev)
	m.cancelCleanupRetry(dev)
	m.scheduleCleanupRetry(t.Context(), dev, true)

	target := main
	target.ConfigTarget = "production"
	target.pauseGeneration = m.observePause(target)
	m.cancelCleanupRetry(target)
	m.scheduleCleanupRetry(t.Context(), target, true)

	newer := main
	newer.pauseGeneration = m.observePause(newer)
	m.cancelCleanupRetry(newer)
	m.scheduleCleanupRetry(t.Context(), newer, true)
	m.cancelCleanupRetry(main)
	m.scheduleCleanupRetry(t.Context(), main, false)

	if len(m.cleanupRetries.pending) != 3 ||
		m.cleanupRetries.pending[cleanupRetryKey(main)].request.pauseGeneration != newer.pauseGeneration {
		t.Fatal("stale request canceled newer work or another ref/target scope")
	}

	m.scheduleCleanupRetry(t.Context(), newer, false)
	dev.pauseGeneration = m.observePause(dev)
	m.cancelCleanupRetry(dev)
	m.scheduleCleanupRetry(t.Context(), dev, true)

	if !m.cleanupRetrySuperseded(main) || m.cleanupRetrySuperseded(target) {
		t.Fatal("source generation watermarks were lost or another scope superseded cleanup")
	}

	m.scheduleCleanupRetry(t.Context(), main, true)

	if len(m.cleanupRetries.pending) != 2 || m.cleanupRetries.pending[cleanupRetryKey(main)] != nil {
		t.Fatal("late request resurrected completed work")
	}

	freshManager := &Manager{}
	if len(freshManager.cleanupRetries.pending) != 0 {
		t.Fatal("a new manager inherited pending cleanup")
	}
}

func TestCleanupRetryOCITagsHaveIndependentScopes(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	t.Cleanup(m.stopCleanupRetries)

	prod := cleanupRetryTestRequest()
	prod.Repository.Source = config.SourceTypeOCI
	prod.Repository.ResolvedReference = ""
	prod.Repository.ConfigSourceUrl = "oci://registry.example.com/organization/repository:production"
	prod.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
		return prod, nil, nil
	}
	prod.pauseGeneration = m.observePause(prod)
	m.cancelCleanupRetry(prod)
	m.scheduleCleanupRetry(t.Context(), prod, true)

	dev := prod
	dev.Repository.ConfigSourceUrl = "oci://registry.example.com/organization/repository:development"
	dev.pauseGeneration = m.observePause(dev)
	m.cancelCleanupRetry(dev)
	m.scheduleCleanupRetry(t.Context(), dev, true)

	if len(m.cleanupRetries.pending) != 2 || m.cleanupRetrySuperseded(prod) {
		t.Fatal("another OCI tag canceled or superseded pending cleanup")
	}

	alias := prod
	alias.Repository.SourceUrl = strings.TrimPrefix(prod.Repository.ConfigSourceUrl, "oci://")

	alias.Repository.ConfigSourceUrl = ""
	if cleanupRetryKey(alias) != cleanupRetryKey(prod) {
		t.Fatal("OCI source URL aliases created independent scopes")
	}

	newer := prod
	newer.pauseGeneration = m.observePause(newer)
	m.cancelCleanupRetry(newer)
	m.scheduleCleanupRetry(t.Context(), newer, false)
	dev.pauseGeneration = m.observePause(dev)
	m.cancelCleanupRetry(dev)
	m.scheduleCleanupRetry(t.Context(), dev, true)
	m.scheduleCleanupRetry(t.Context(), prod, true)

	if len(m.cleanupRetries.pending) != 1 || !m.cleanupRetrySuperseded(prod) ||
		m.cleanupRetrySuperseded(dev) {
		t.Fatal("completed OCI source lost its generation watermark or affected another tag")
	}
}

func TestCleanupRetryRejectsChangedRefreshScope(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	req := cleanupRetryTestRequest()
	released := false
	req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
		fresh := req
		fresh.Repository.ResolvedReference = "other"

		return fresh, func() { released = true }, nil
	}
	req = seedCleanupRetry(t, m, req)
	m.runDueCleanupRetry(t.Context(), time.Now(),
		func(context.Context, DeployRequest, *syncWindowGate) (bool, error) {
			t.Fatal("cleanup ran with a different source scope")

			return false, nil
		})

	if !released || len(m.cleanupRetries.pending) != 1 ||
		m.currentCleanupRequest(req).Repository.ResolvedReference != "main" {
		t.Fatal("changed source scope was accepted or not released")
	}
}

func TestCleanupRetryRejectsUntrustedArtifactBeforeObservation(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	req := cleanupRetryTestRequest()
	req.Repository.Source = config.SourceTypeOCI
	req.Repository.OCITrusted = true
	req.Repository.ConfigSourceUrl = "oci://registry.example.com/repository:production"
	released := false
	req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
		fresh := req
		fresh.Repository.OCITrusted = false
		fresh.Repository.Revision = "untrusted"

		return fresh, func() { released = true }, nil
	}
	req = seedCleanupRetry(t, m, req)
	m.runDueCleanupRetry(t.Context(), time.Now(),
		func(context.Context, DeployRequest, *syncWindowGate) (bool, error) {
			t.Fatal("untrusted artifact reached cleanup")
			return false, nil
		})

	if !released || len(m.cleanupRetries.pending) != 1 ||
		m.currentCleanupRequest(req).Repository.Revision != req.Repository.Revision {
		t.Fatal("untrusted refresh changed observed intent, leaked its release, or lost its retry")
	}
}

func TestCleanupRetryLoopBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &Manager{}
		startCleanupRetryTestLoop(t, m,
			func(context.Context, DeployRequest, *syncWindowGate) (bool, error) {
				t.Fatal("cleanup ran after failed refresh")

				return false, nil
			})

		req := cleanupRetryTestRequest()

		var refreshes atomic.Int32

		req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
			refreshes.Add(1)

			return DeployRequest{}, nil, errors.New("unavailable")
		}
		req.pauseGeneration = m.observePause(req)
		m.scheduleCleanupRetry(t.Context(), req, true)
		time.Sleep(cleanupRetryInitialDelay)
		synctest.Wait()

		if got := refreshes.Load(); got != 1 {
			t.Fatalf("refreshes=%d, want 1", got)
		}

		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()

		if refreshes.Load() != 1 {
			t.Fatal("retry ignored its backoff")
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()

		if got := refreshes.Load(); got != 2 {
			t.Fatalf("refreshes=%d, want 2", got)
		}

		m.stopCleanupRetries()
	})
}

func TestCleanupRetryStopCancelsAndJoins(t *testing.T) {
	for _, phase := range []string{"refresh", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := &Manager{}
				started, released := false, false

				startCleanupRetryTestLoop(t, m,
					func(ctx context.Context, _ DeployRequest, _ *syncWindowGate) (bool, error) {
						started = true

						<-ctx.Done()

						return true, ctx.Err()
					})

				req := cleanupRetryTestRequest()
				req.RefreshCleanup = func(ctx context.Context) (DeployRequest, func(), error) {
					if phase == "refresh" {
						started = true

						<-ctx.Done()

						return req, func() { released = true }, ctx.Err()
					}

					return req, func() { released = true }, nil
				}
				req.pauseGeneration = m.observePause(req)
				m.scheduleCleanupRetry(t.Context(), req, true)
				time.Sleep(cleanupRetryInitialDelay)
				synctest.Wait()

				if !started {
					t.Fatal("retry did not reach blocking operation")
				}

				m.stopCleanupRetries()

				if !released || len(m.cleanupRetries.pending) != 0 {
					t.Fatal("stop did not release and clear in-flight cleanup")
				}

				m.scheduleCleanupRetry(t.Context(), req, true)

				if len(m.cleanupRetries.pending) != 0 {
					t.Fatal("stopped manager accepted new cleanup")
				}
			})
		})
	}
}

func TestCleanupRetryAttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &Manager{}
		req := cleanupRetryTestRequest()

		var logs bytes.Buffer

		req.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		released := false

		startCleanupRetryTestLoop(t, m,
			func(context.Context, DeployRequest, *syncWindowGate) (bool, error) {
				t.Fatal("cleanup ran after source refresh timed out")

				return false, nil
			})

		req.RefreshCleanup = func(ctx context.Context) (DeployRequest, func(), error) {
			<-ctx.Done()

			return req, func() { released = true }, ctx.Err()
		}
		req.pauseGeneration = m.observePause(req)
		m.scheduleCleanupRetry(t.Context(), req, true)
		time.Sleep(cleanupRetryInitialDelay + cleanupRetryTimeout)
		synctest.Wait()

		if !released || len(m.cleanupRetries.pending) != 1 ||
			!strings.Contains(logs.String(), context.DeadlineExceeded.Error()) {
			t.Fatal("timed-out refresh was not released, logged, and kept pending")
		}

		m.stopCleanupRetries()
	})
}

func TestCleanupRetryConcurrentStopAndRequests(t *testing.T) {
	t.Parallel()

	m := &Manager{}
	req := cleanupRetryTestRequest()
	req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) { return req, nil, nil }
	req.pauseGeneration = m.observePause(req)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				m.cancelCleanupRetry(req)
				m.scheduleCleanupRetry(t.Context(), req, true)
			}
		})
	}

	wg.Go(m.stopCleanupRetries)
	wg.Wait()
	m.stopCleanupRetries()

	if len(m.cleanupRetries.pending) != 0 {
		t.Fatal("concurrent requests recreated stopped cleanup")
	}
}
