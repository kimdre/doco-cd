package reconciliation

import (
	"errors"
	"log/slog"
	"sync"
	"testing"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func pauseTestRequest(dc *deployConfig.Config) DeployRequest {
	return DeployRequest{
		Logger:        slog.New(slog.DiscardHandler),
		Repository:    stages.RepositoryData{Name: "repo", Revision: "deployed"},
		DeployConfigs: []*deployConfig.Config{dc},
	}
}

func TestPauseAdmissionScopes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pause    bool
		origin   syncwindow.Origin
		revision *stackRevision
		blocked  bool
	}{
		{name: "normal update"},
		{name: "deployment freeze", pause: true, blocked: true},
		{name: "manual update", origin: syncwindow.OriginManual, pause: true, blocked: true},
		{name: "freeze permits recovery", pause: true, origin: syncwindow.OriginReconciliation},
		{name: "freeze rejects newer revision", pause: true, origin: syncwindow.OriginReconciliation, revision: &stackRevision{own: true, revision: "new", deployed: "old"}, blocked: true},
		{name: "freeze rejects missing deployed revision", pause: true, origin: syncwindow.OriginReconciliation, revision: &stackRevision{own: true, revision: "new"}, blocked: true},
		{name: "freeze permits matching deployed revision", pause: true, origin: syncwindow.OriginReconciliation, revision: &stackRevision{own: true, revision: "old", deployed: "old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &Manager{}
			dc := &deployConfig.Config{Name: "web", Pause: tc.pause, Reconciliation: deployConfig.ReconciliationConfig{Enabled: true}}
			req := pauseTestRequest(dc)
			req.Origin = tc.origin

			err := manager.admitPause(req, dc, tc.revision)
			if errors.Is(err, stages.ErrDeploymentPaused) != tc.blocked {
				t.Fatalf("admitPause() = %v, want blocked=%v", err, tc.blocked)
			}
		})
	}
}

func TestPauseObservationUsesCurrentIntentAndIsolatesContexts(t *testing.T) {
	manager := &Manager{}
	dc := &deployConfig.Config{Name: "web"}
	oldReq := pauseTestRequest(dc)
	oldReq.pauseGeneration = manager.observePause(oldReq)
	paused := &deployConfig.Config{Name: "web", Pause: true}
	current := pauseTestRequest(paused)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			manager.observePause(current)
			_ = manager.admitPause(oldReq, dc, nil)
			manager.currentCleanupRequest(oldReq)
			manager.newerPauseObservation(oldReq)
		})
	}

	wg.Wait()

	paused.Pause = false

	if err := manager.admitPause(oldReq, dc, nil); !errors.Is(err, stages.ErrDeploymentPaused) {
		t.Fatal("an older request ignored the latest pause")
	}

	if !manager.newerPauseObservation(oldReq) {
		t.Fatal("older completion was not detected")
	}

	remote := &deployConfig.Config{Name: "web", Context: "remote"}
	if err := manager.admitPause(oldReq, remote, nil); err != nil {
		t.Fatalf("pause leaked to another context: %v", err)
	}
}

func TestPauseObservationRejectsStaleRevision(t *testing.T) {
	repo, older, newer := newTestRepoWithTwoCommits(t)
	manager := &Manager{}
	dc := &deployConfig.Config{Name: "web", Pause: true}
	req := pauseTestRequest(dc)
	req.Repository.MirrorDir, req.Repository.ResolvedReference = repo, "main"
	req.Repository.Revision = newer
	manager.observePause(req)
	req.Repository.Revision = older

	req.DeployConfigs = []*deployConfig.Config{{Name: "web"}}
	if got := manager.observePause(req); got != 0 {
		t.Fatal("accepted a stale resume instruction")
	}

	if !manager.newerPauseObservation(req) {
		t.Fatal("rejected stale request can replace newer recovery state")
	}

	if !manager.pauseFor(req, dc) {
		t.Fatal("stale instruction cleared pause")
	}
}

func TestPauseObservationPreservesExcludedScopes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		stack  string
		ref    string
	}{
		{name: "another target", target: "development", stack: "api", ref: "refs/heads/main"},
		{name: "filtered webhook", target: "production", stack: "web", ref: "refs/heads/feature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, pause := range []bool{true, false} {
				manager := &Manager{}
				dc := &deployConfig.Config{Name: "web", Pause: pause, WebhookEventFilter: "^refs/heads/main$"}
				dc.Internal.ConfigTarget = "production"
				req := pauseTestRequest(dc)
				req.ConfigTarget = "production"
				req.JobTrigger = stages.JobTriggerWebhook
				req.Payload = &webhook.ParsedPayload{Ref: "refs/heads/main"}
				manager.observePause(req)

				changed := req
				changed.ConfigTarget = tc.target
				changed.Payload = &webhook.ParsedPayload{Ref: tc.ref}
				changedDC := *dc
				changedDC.Name = tc.stack
				changedDC.Pause = !pause
				changedDC.Internal.ConfigTarget = tc.target
				changed.DeployConfigs = []*deployConfig.Config{&changedDC}
				manager.observePause(changed)

				if manager.pauseFor(req, dc) != pause {
					t.Fatal("an excluded request changed the deployment's pause")
				}

				labels := map[string]string{
					docker.DocoCDLabels.Source.Name:             "repo",
					docker.DocoCDLabels.Deployment.Name:         "web",
					docker.DocoCDLabels.Deployment.ConfigTarget: "production",
				}
				if manager.AdmitCertificateRotation("", labels) == pause {
					t.Fatal("certificate rotation ignored the preserved pause")
				}
			}
		})
	}
}

func TestPauseJobRetainsDeployedSnapshotAndClosesOldListener(t *testing.T) {
	manager := &Manager{jobs: jobRegistry{jobs: make(map[string]*job)}}
	dc := &deployConfig.Config{Name: "web", Reconciliation: deployConfig.ReconciliationConfig{
		Enabled: true, Events: []string{"die"},
	}}
	deployed := pauseTestRequest(dc)
	old := newReconciliationJob(manager, deployed, nil, nil)
	manager.jobs.jobs["repo"] = old
	admittedDC := &deployConfig.Config{Name: "web", Reconciliation: dc.Reconciliation}
	admitted := pauseTestRequest(admittedDC)
	admitted.Repository.Revision = "admitted"
	admitted.pauseGeneration = manager.observePause(admitted)
	paused := &deployConfig.Config{
		Name: "web", Pause: true,
		Reconciliation: deployConfig.ReconciliationConfig{Enabled: false},
	}
	req := pauseTestRequest(paused)
	req.Repository.Revision = "not-deployed"
	req.pauseGeneration = manager.observePause(req)
	manager.addJob(t.Context(), req, nil, nil)

	current := manager.jobs.jobs["repo"]
	if current == nil || current == old || len(current.deployConfigGroupByEvent) != 0 {
		t.Fatal("pause did not replace the active listener with dormant recovery metadata")
	}

	select {
	case <-old.closeChan:
	default:
		t.Fatal("old listener was not closed")
	}

	if source := current.requestFor(current.info.DeployConfigs[0]); source.Repository.Revision != "deployed" {
		t.Fatalf("pause replaced the recovery revision: %s", source.Repository.Revision)
	}

	manager.addJob(t.Context(), admitted, nil, map[*deployConfig.Config]struct{}{admittedDC: {}})

	current = manager.jobs.jobs["repo"]
	if current == nil || len(current.info.DeployConfigs) != 1 ||
		current.requestFor(current.info.DeployConfigs[0]).Repository.Revision != "admitted" {
		t.Fatal("late completion did not retain the actual deployed revision")
	}

	if !manager.pauseFor(admitted, admittedDC) || len(current.deployConfigGroupByEvent) != 0 ||
		len(current.cleanupConfigs) != 1 || !current.cleanupConfigs[0].Pause {
		t.Fatal("late completion resumed reconciliation or restored obsolete cleanup intent")
	}

	disabled := pauseTestRequest(&deployConfig.Config{
		Name: "web", Pause: true,
		Reconciliation: deployConfig.ReconciliationConfig{Enabled: false},
	})
	manager.observePause(disabled)

	if grouped := manager.recoveryGroupByEvent(disabled, current.info.DeployConfigs); len(grouped) != 0 {
		t.Fatal("pause enabled recovery despite reconciliation being disabled")
	}

	resume := pauseTestRequest(&deployConfig.Config{Name: "web", Pause: true, Reconciliation: dc.Reconciliation})
	manager.observePause(resume)

	next := newReconciliationJob(manager, resume, map[*deployConfig.Config]struct{}{resume.DeployConfigs[0]: {}}, current)
	if len(next.deployConfigGroupByEvent["die"]) != 1 {
		t.Fatal("enabling reconciliation during pause did not restore recovery")
	}

	if source := next.requestFor(next.info.DeployConfigs[0]); source.Repository.Revision != "admitted" {
		t.Fatal("resume lost the last deployed revision")
	}
}

func TestPauseEmptyRootKeepsRecoveryAndRemovalProtection(t *testing.T) {
	origin := deployConfig.AutoDiscoveryOrigin{
		WorkingDirectory:      "services",
		Settings:              deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true, ScanDepth: 1},
		ReconciliationEnabled: true,
	}
	dc := cleanupTestDiscovered("web", origin)
	dc.WorkingDirectory = "services/web"
	dc.Reconciliation = deployConfig.ReconciliationConfig{Enabled: true, Events: []string{"die"}}
	manager := &Manager{jobs: jobRegistry{jobs: make(map[string]*job)}}
	previous := pauseTestRequest(dc)
	manager.observePause(previous)
	manager.jobs.jobs["repo"] = newReconciliationJob(manager, previous, nil, nil)

	current := pauseTestRequest(nil)
	current.DeployConfigs = nil
	current.Repository.Revision = "paused"
	origin.Pause = true
	current.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{origin}
	current.pauseGeneration = manager.observePause(current)
	manager.addJob(t.Context(), current, nil, nil)

	j := manager.jobs.jobs["repo"]
	if j == nil || len(j.info.DeployConfigs) != 1 || len(j.deployConfigGroupByEvent["die"]) != 1 {
		t.Fatal("empty paused root lost its deployed snapshot or enabled recovery")
	}

	if manager.removalPredicate(current, dc.Context, nil)(current.Logger, dc.Name) {
		t.Fatal("empty paused root allowed removal of a previously observed child")
	}

	manager.observePause(current)

	if !manager.pauseFor(current, &deployConfig.Config{Name: dc.Name, Context: dc.Context}) {
		t.Fatal("repeated empty root scans lost removal protection")
	}

	if len(manager.recoveryGroupByEvent(current, j.info.DeployConfigs)["die"]) != 1 {
		t.Fatal("paused root blocked deployed-state recovery")
	}

	current.AutoDiscoveryOrigins[0].ReconciliationEnabled = false
	manager.observePause(current)

	if len(manager.recoveryGroupByEvent(current, j.info.DeployConfigs)) != 0 {
		t.Fatal("empty root ignored reconciliation:false")
	}

	current.AutoDiscoveryOrigins = nil
	manager.observePause(current)

	if manager.pauseFor(current, dc) {
		t.Fatal("removing the root left a sticky pause")
	}
}

func TestPauseDeploymentResultsPreserveRealFailures(t *testing.T) {
	paused := stackResult{name: "paused", err: stages.DeploymentPaused()}
	if err := summarizeDeployResults([]stackResult{paused}, 1); !errors.Is(err, stages.ErrDeploymentPaused) {
		t.Fatalf("pause identity lost: %v", err)
	}

	if err := summarizeDeployResults([]stackResult{paused, {name: "active"}}, 2); err != nil {
		t.Fatalf("paused sibling prevented success: %v", err)
	}

	failure := errors.New("deployment failed")
	if err := summarizeDeployResults([]stackResult{paused, {name: "broken", err: failure}}, 2); !errors.Is(err, failure) || errors.Is(err, stages.ErrDeploymentPaused) {
		t.Fatalf("pause masked a real failure: %v", err)
	}

	if isUnchangedOutcome(stages.DeploymentPaused()) {
		t.Fatal("pause was treated as proof of an unchanged deployed state")
	}

	suppressed := &stages.RetrySuppressedError{Stacks: []stages.RetrySuppressedStack{{Name: "web", Reason: "budget_exhausted"}}}
	if err := summarizeDeployResults([]stackResult{paused, {name: "web", err: suppressed}}, 2); !errors.Is(err, stages.ErrDeploymentPaused) || !errors.Is(err, stages.ErrRetrySuppressed) {
		t.Fatalf("pause or retry identity was lost: %v", err)
	}
}

func TestPauseSkipsUnavailableContextAndDestructionBeforeDocker(t *testing.T) {
	manager := &Manager{}
	dc := &deployConfig.Config{
		Name: "web", Context: "unavailable", ForceRecreate: true, ForceImagePull: true,
		Destroy: deployConfig.DestroyConfig{Enabled: true},
		Pause:   true,
	}
	req := pauseTestRequest(dc)
	gate := &syncWindowGate{outcomes: make(map[*deployConfig.Config]error)}

	results, err := manager.deploy(t.Context(), req, gate)
	if !errors.Is(err, stages.ErrDeploymentPaused) || len(results) != 1 {
		t.Fatalf("paused request attempted Docker work: results=%v, err=%v", results, err)
	}
}

func TestPauseEmptyDiscoveryRootDoesNotResolveDocker(t *testing.T) {
	manager := &Manager{}
	req := pauseTestRequest(nil)
	req.DeployConfigs = nil
	req.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{{
		Context: "unavailable",
		Pause:   true,
	}}

	results, err := manager.deploy(t.Context(), req, nil)
	if !errors.Is(err, stages.ErrDeploymentPaused) || len(results) != 0 {
		t.Fatalf("empty paused root lost its skip: results=%v, err=%v", results, err)
	}
}

func TestPauseCertificateRotationHonorsCurrentDeploymentScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pause bool
		allow bool
	}{
		{name: "unpaused", allow: true},
		{name: "deployment pause", pause: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &Manager{}
			req := pauseTestRequest(&deployConfig.Config{Name: "web", Pause: tc.pause})
			manager.observePause(req)

			labels := map[string]string{
				docker.DocoCDLabels.Source.Name:     "repo",
				docker.DocoCDLabels.Deployment.Name: "web",
			}
			if got := manager.AdmitCertificateRotation("", labels); got != tc.allow {
				t.Fatalf("AdmitCertificateRotation() = %v, want %v", got, tc.allow)
			}

			if !manager.AdmitCertificateRotation("remote", labels) {
				t.Fatal("pause leaked to another Docker context")
			}

			labels[docker.DocoCDLabels.Source.Name] = "another-repo"
			if !manager.AdmitCertificateRotation("", labels) {
				t.Fatal("pause leaked to another source")
			}
		})
	}
}

func TestPauseCertificateRotationKeepsSourcesIsolatedAcrossHosts(t *testing.T) {
	manager := &Manager{}
	github := pauseTestRequest(&deployConfig.Config{Name: "web", Pause: true})
	github.Repository.Name = "github.com/owner/repo"
	github.Repository.ConfigSourceUrl = "https://github.com/owner/repo.git"
	github.Payload = &webhook.ParsedPayload{FullName: "owner/repo"}
	manager.observePause(github)

	gitlab := pauseTestRequest(&deployConfig.Config{Name: "web"})
	gitlab.Repository.Name = "gitlab.com/owner/repo"
	gitlab.Repository.ConfigSourceUrl = "https://gitlab.com/owner/repo.git"
	gitlab.Payload = &webhook.ParsedPayload{FullName: "owner/repo"}
	manager.observePause(gitlab)

	labels := map[string]string{
		docker.DocoCDLabels.Source.Name:     "owner/repo",
		docker.DocoCDLabels.Source.URL:      "git@gitlab.com:owner/repo.git",
		docker.DocoCDLabels.Deployment.Name: "web",
	}

	if !manager.AdmitCertificateRotation("", labels) {
		t.Fatal("pause crossed repository hosts")
	}

	labels[docker.DocoCDLabels.Source.URL] = "git@github.com:owner/repo.git"
	if manager.AdmitCertificateRotation("", labels) {
		t.Fatal("transport aliases bypassed deployment pause")
	}
}

func TestPauseCertificateRotationHonorsEmptyRoot(t *testing.T) {
	manager := &Manager{}
	req := pauseTestRequest(nil)
	req.DeployConfigs = nil
	req.Repository.SourceUrl = cleanupTestRepoURL
	req.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{{
		WorkingDirectory: "services", Pause: true,
		Settings: deployConfig.AutoDiscoveryConfig{Enabled: true, ScanDepth: 1},
	}}
	manager.observePause(req)

	for _, tc := range []struct {
		directory string
		context   string
		target    string
		allow     bool
	}{
		{directory: "services/web"},
		{directory: "databases/web", allow: true},
		{directory: "services/nested/web", allow: true},
		{directory: "services/web", context: "remote", allow: true},
		{directory: "services/web", target: "another", allow: true},
	} {
		stack := cleanupTestStack("web", tc.directory, map[string]string{
			docker.DocoCDLabels.Source.Name:             "repo",
			docker.DocoCDLabels.Deployment.ConfigTarget: tc.target,
		})
		if manager.AdmitCertificateRotation(tc.context, stack.Labels) != tc.allow {
			t.Errorf("unexpected rotation admission for %+v", tc)
		}
	}
}
