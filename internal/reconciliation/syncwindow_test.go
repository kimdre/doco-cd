package reconciliation

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

// syncWindowTestNow is inside the "freeze" and "ops" windows of
// syncWindowTestPolicy, which both close at 11:00 UTC.
var syncWindowTestNow = time.Date(2026, time.March, 10, 10, 0, 0, 0, time.UTC)

const syncWindowTestPolicy = `
- name: freeze
  kind: deny
  schedule: "0 9 * * *"
  duration: 2h
  timezone: UTC
  deployments: ["web*"]
- name: ops
  kind: deny
  schedule: "0 9 * * *"
  duration: 2h
  timezone: UTC
  deployments: ["ops"]
  manual_sync: true
`

func newSyncWindowTestManager(t *testing.T, policyYAML string) *Manager {
	t.Helper()

	policy, err := syncwindow.Parse(policyYAML)
	if err != nil {
		t.Fatalf("syncwindow.Parse() failed: %v", err)
	}

	return &Manager{appConfig: &app.Config{SyncWindows: policy}}
}

func syncWindowTestLogger() (*slog.Logger, *bytes.Buffer) {
	var logs bytes.Buffer

	return slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), &logs
}

func syncWindowTestRequest(revision string, origin syncwindow.Origin, dcs ...*deployConfig.Config) DeployRequest {
	return DeployRequest{
		Repository: stages.RepositoryData{
			Name:     "github.com/acme/app",
			Revision: revision,
		},
		DeployConfigs: dcs,
		Origin:        origin,
	}
}

func TestNewSyncWindowGate_NilWithoutWindows(t *testing.T) {
	t.Parallel()

	dc := &deployConfig.Config{Name: "web"}

	for name, manager := range map[string]*Manager{
		"nil manager":    nil,
		"nil app config": {},
		"nil policy":     {appConfig: &app.Config{}},
		"empty policy":   newSyncWindowTestManager(t, ""),
	} {
		gate := manager.newSyncWindowGate(syncWindowTestRequest("rev", "", dc), syncWindowTestNow)
		if gate != nil {
			t.Fatalf("%s: expected nil gate", name)
		}
	}

	// A nil gate allows everything.
	var gate *syncWindowGate

	log, _ := syncWindowTestLogger()

	if gate.blocks(dc) {
		t.Fatal("nil gate blocks")
	}

	if err := gate.admit(log, dc, nil); err != nil {
		t.Fatalf("nil gate admit() = %v", err)
	}

	if gate.removalPredicate("") != nil {
		t.Fatal("nil gate returned a removal predicate")
	}

	if !gate.allowRemoval(log, "", "web") {
		t.Fatal("nil gate rejects removal")
	}

	gate.record(dc, nil)

	if deferred := gate.deferred(); deferred != nil {
		t.Fatalf("nil gate deferred() = %v", deferred)
	}
}

func TestSyncWindowGate_AdmitBlocksAndDedupesNotices(t *testing.T) {
	t.Parallel()

	manager := newSyncWindowTestManager(t, syncWindowTestPolicy)
	web := &deployConfig.Config{Name: "web"}
	db := &deployConfig.Config{Name: "db"}

	admit := func(revision string) ([]string, string, error) {
		t.Helper()

		gate := manager.newSyncWindowGate(syncWindowTestRequest(revision, syncwindow.OriginAutomatic, web, db), syncWindowTestNow)
		if gate == nil {
			t.Fatal("expected a gate")
		}

		if !gate.blocks(web) || gate.blocks(db) {
			t.Fatalf("blocks(web) = %v, blocks(db) = %v, want true, false", gate.blocks(web), gate.blocks(db))
		}

		log, logs := syncWindowTestLogger()

		if err := gate.admit(log, db, func(string) { t.Fatal("posted a status for an allowed stack") }); err != nil {
			t.Fatalf("admit(db) = %v", err)
		}

		var statuses []string

		err := gate.admit(log, web, func(description string) { statuses = append(statuses, description) })

		return statuses, logs.String(), err
	}

	statuses, logs, err := admit("rev-1")

	blockedErr, ok := errors.AsType[*stages.SyncWindowBlockedError](err)
	if !ok {
		t.Fatalf("admit(web) = %v, want *stages.SyncWindowBlockedError", err)
	}

	if !errors.Is(err, stages.ErrSyncWindowBlocked) || !errors.Is(err, stages.ErrSkipDeployment) {
		t.Fatalf("admit(web) error %v does not wrap ErrSyncWindowBlocked and ErrSkipDeployment", err)
	}

	wantNextOpen := time.Date(2026, time.March, 10, 11, 0, 0, 0, time.UTC)
	if !blockedErr.NextOpen.Equal(wantNextOpen) {
		t.Fatalf("NextOpen = %v, want %v", blockedErr.NextOpen, wantNextOpen)
	}

	if strings.Join(blockedErr.Stacks, ",") != "web" || strings.Join(blockedErr.Windows, ",") != "freeze" {
		t.Fatalf("got stacks %v and windows %v, want [web] and [freeze]", blockedErr.Stacks, blockedErr.Windows)
	}

	if len(statuses) != 1 || statuses[0] != "Deferred by sync window until 2026-03-10T11:00:00Z" {
		t.Fatalf("posted statuses %q, want one deferred status", statuses)
	}

	if !strings.Contains(logs, "level=INFO msg=\"deployment deferred by sync window\"") {
		t.Fatalf("expected an info log for the first deferral, got:\n%s", logs)
	}

	// The same revision is only reported once.
	statuses, logs, err = admit("rev-1")
	if !errors.Is(err, stages.ErrSyncWindowBlocked) {
		t.Fatalf("second admit(web) = %v, want ErrSyncWindowBlocked", err)
	}

	if len(statuses) != 0 {
		t.Fatalf("posted statuses %q for an already reported revision", statuses)
	}

	if !strings.Contains(logs, "level=DEBUG msg=\"deployment deferred by sync window\"") || strings.Contains(logs, "level=INFO") {
		t.Fatalf("expected only a debug log for a repeated deferral, got:\n%s", logs)
	}

	// A new revision is reported again.
	statuses, _, _ = admit("rev-2")
	if len(statuses) != 1 {
		t.Fatalf("posted %d statuses for a new revision, want 1", len(statuses))
	}
}

func TestSyncWindowGate_RecordResetsNotices(t *testing.T) {
	t.Parallel()

	for name, outcome := range map[string]error{
		"deployed":  nil,
		"unchanged": stages.ErrSkipDeployment,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			manager := newSyncWindowTestManager(t, syncWindowTestPolicy)
			web := &deployConfig.Config{Name: "web"}
			log, _ := syncWindowTestLogger()

			posted := 0
			postStatus := func(string) { posted++ }

			gate := manager.newSyncWindowGate(syncWindowTestRequest("rev-1", "", web), syncWindowTestNow)
			_ = gate.admit(log, web, postStatus)

			// The window opened and the stack was deployed (or found unchanged).
			open := manager.newSyncWindowGate(syncWindowTestRequest("rev-1", "", web), syncWindowTestNow.Add(2*time.Hour))
			open.record(web, outcome)

			// The revision is deferred again later, which is a new deferral.
			gate = manager.newSyncWindowGate(syncWindowTestRequest("rev-1", "", web), syncWindowTestNow.Add(24*time.Hour))
			_ = gate.admit(log, web, postStatus)

			if posted != 2 {
				t.Fatalf("posted %d statuses, want 2", posted)
			}
		})
	}
}

func TestSyncWindowGate_Origins(t *testing.T) {
	t.Parallel()

	manager := newSyncWindowTestManager(t, syncWindowTestPolicy)
	web := &deployConfig.Config{Name: "web"}
	ops := &deployConfig.Config{Name: "ops"}

	tests := []struct {
		name       string
		origin     syncwindow.Origin
		wantWeb    bool // whether web is blocked
		wantOps    bool // whether ops is blocked
		wantBypass bool // whether a manual_sync bypass is logged
	}{
		{name: "default is automatic", origin: "", wantWeb: true, wantOps: true},
		{name: "automatic", origin: syncwindow.OriginAutomatic, wantWeb: true, wantOps: true},
		{name: "manual bypasses manual_sync windows only", origin: syncwindow.OriginManual, wantWeb: true, wantOps: false, wantBypass: true},
		{name: "reconciliation restores the request revision", origin: syncwindow.OriginReconciliation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gate := manager.newSyncWindowGate(syncWindowTestRequest("rev", tt.origin, web, ops), syncWindowTestNow)

			if got := gate.blocks(web); got != tt.wantWeb {
				t.Fatalf("blocks(web) = %v, want %v", got, tt.wantWeb)
			}

			if got := gate.blocks(ops); got != tt.wantOps {
				t.Fatalf("blocks(ops) = %v, want %v", got, tt.wantOps)
			}

			log, logs := syncWindowTestLogger()

			err := gate.admit(log, ops, nil)
			if (err != nil) != tt.wantOps {
				t.Fatalf("admit(ops) = %v, want blocked = %v", err, tt.wantOps)
			}

			if got := strings.Contains(logs.String(), "sync window bypassed by manual deployment"); got != tt.wantBypass {
				t.Fatalf("bypass logged = %v, want %v, logs:\n%s", got, tt.wantBypass, logs)
			}

			if _, deferred := gate.deferred()[ops]; deferred != tt.wantOps {
				t.Fatalf("ops deferred = %v, want %v", deferred, tt.wantOps)
			}
		})
	}
}

func TestStackRevisionRestoresDeployed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rev  stackRevision
		want bool
	}{
		{name: "request revision", rev: stackRevision{revision: "new", deployed: "old"}, want: true},
		{name: "request revision without deployed revision", rev: stackRevision{revision: "rev"}, want: true},
		{name: "own reference at deployed revision", rev: stackRevision{revision: "rev", deployed: " rev ", own: true}, want: true},
		{name: "own reference at newer revision", rev: stackRevision{revision: "new", deployed: "old", own: true}},
		{name: "own reference without deployed revision", rev: stackRevision{revision: "rev", own: true}},
		{name: "own reference without revisions", rev: stackRevision{own: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.rev.restoresDeployed(); got != tt.want {
				t.Fatalf("restoresDeployed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncWindowGate_ReconciliationGatesRevisionChanges(t *testing.T) {
	t.Parallel()

	manager := newSyncWindowTestManager(t, syncWindowTestPolicy)
	web := &deployConfig.Config{Name: "web", Reference: "release"}
	ops := &deployConfig.Config{Name: "ops", Reference: "release"}
	log, logs := syncWindowTestLogger()

	gate := manager.newSyncWindowGate(syncWindowTestRequest("snapshot", syncwindow.OriginReconciliation, web, ops), syncWindowTestNow)
	if gate == nil {
		t.Fatal("expected a gate")
	}

	// Restoring a known revision, including a destroy, is never blocked.
	if gate.blocks(web) {
		t.Fatal("blocks(web) = true, want false for a reconciliation")
	}

	if err := gate.admit(log, web, nil); err != nil {
		t.Fatalf("admit(web) = %v, want nil for the request revision", err)
	}

	if err := gate.admitStack(log, web, stackRevision{revision: "r1", deployed: "r1", own: true}, nil); err != nil {
		t.Fatalf("admitStack(web) = %v, want nil for the deployed revision", err)
	}

	if err := gate.admitStack(log, web, stackRevision{revision: "snapshot", deployed: "r1", own: true}, nil); err == nil {
		t.Fatal("admitStack(web) = nil, want blocked for a revision that is only the request revision by chance")
	}

	// A newer revision of the stack's own reference is a change.
	var statuses []string

	err := gate.admitStack(log, web, stackRevision{revision: "r2", deployed: "r1", own: true}, func(description string) {
		statuses = append(statuses, description)
	})

	blockedErr, ok := errors.AsType[*stages.SyncWindowBlockedError](err)
	if !ok || strings.Join(blockedErr.Windows, ",") != "freeze" {
		t.Fatalf("admitStack(web) = %v, want blocked by freeze", err)
	}

	// Notices are deduplicated by the stack's revision, so the first
	// deferral of r2 is reported even though "snapshot" was reported before.
	if len(statuses) != 1 {
		t.Fatalf("posted statuses %q, want one", statuses)
	}

	// Reconciliation is automatic, so manual_sync does not apply.
	if err := gate.admitStack(log, ops, stackRevision{revision: "r2", own: true}, nil); !errors.Is(err, stages.ErrSyncWindowBlocked) {
		t.Fatalf("admitStack(ops) = %v, want blocked", err)
	}

	if strings.Contains(logs.String(), "sync window bypassed by manual deployment") {
		t.Fatalf("reconciliation logged a manual bypass:\n%s", logs)
	}

	if deferred := gate.deferred(); deferred != nil {
		t.Fatalf("deferred() = %v, want nil for a reconciliation", deferred)
	}
}

func TestSyncWindowGate_Deferred(t *testing.T) {
	t.Parallel()

	manager := newSyncWindowTestManager(t, syncWindowTestPolicy)
	unchanged := &deployConfig.Config{Name: "web-unchanged"}
	blocked := &deployConfig.Config{Name: "web-blocked"}
	unrecorded := &deployConfig.Config{Name: "web-unrecorded"}
	filtered := &deployConfig.Config{Name: "web-filtered"}
	failed := &deployConfig.Config{Name: "web-failed"}
	allowed := &deployConfig.Config{Name: "db"}

	gate := manager.newSyncWindowGate(syncWindowTestRequest("rev", "",
		unchanged, blocked, unrecorded, filtered, failed, allowed), syncWindowTestNow)

	gate.record(unchanged, stages.ErrSkipDeployment)
	gate.record(blocked, &stages.SyncWindowBlockedError{Stacks: []string{blocked.Name}})
	gate.record(filtered, stages.ErrWebhookFilterMismatch)
	gate.record(failed, errors.New("boom"))
	gate.record(allowed, nil)

	deferred := gate.deferred()

	for _, dc := range []*deployConfig.Config{blocked, unrecorded, filtered, failed} {
		if _, ok := deferred[dc]; !ok {
			t.Errorf("expected %s to be deferred", dc.Name)
		}
	}

	for _, dc := range []*deployConfig.Config{unchanged, allowed} {
		if _, ok := deferred[dc]; ok {
			t.Errorf("expected %s not to be deferred", dc.Name)
		}
	}
}

func TestSyncWindowGate_AllowRemoval(t *testing.T) {
	t.Parallel()

	manager := newSyncWindowTestManager(t, syncWindowTestPolicy)
	req := syncWindowTestRequest("rev", syncwindow.OriginAutomatic)

	gate := manager.newSyncWindowGate(req, syncWindowTestNow)

	allow := gate.removalPredicate("")
	if allow == nil {
		t.Fatal("expected a removal predicate")
	}

	log, logs := syncWindowTestLogger()

	if allow(log, "web-old") {
		t.Fatal("removal of a stack in a deny window was allowed")
	}

	if !allow(log, "db-old") {
		t.Fatal("removal of a stack outside of any window was rejected")
	}

	if !strings.Contains(logs.String(), "removal of obsolete auto-discovered stack deferred by sync window") {
		t.Fatalf("expected a deferred removal log, got:\n%s", logs)
	}

	// Removals outside the window are allowed.
	gate = manager.newSyncWindowGate(req, syncWindowTestNow.Add(2*time.Hour))
	if !gate.allowRemoval(log, "", "web-old") {
		t.Fatal("removal outside the window was rejected")
	}
}

func TestSyncWindowTarget(t *testing.T) {
	t.Parallel()

	repository := stages.RepositoryData{Name: "github.com/acme/config"}

	got := syncWindowTarget(repository, &deployConfig.Config{Name: "web", Context: "prod"})

	want := syncwindow.Target{Repository: "github.com/acme/config", Deployment: "web", Context: "prod"}
	if got != want {
		t.Fatalf("syncWindowTarget() = %+v, want %+v", got, want)
	}

	got = syncWindowTarget(repository, &deployConfig.Config{Name: "web", RepositoryUrl: "https://github.com/acme/app.git"})

	want = syncwindow.Target{
		Repository:       "github.com/acme/app",
		ConfigRepository: "github.com/acme/config",
		Deployment:       "web",
		Context:          "default",
	}
	if got != want {
		t.Fatalf("syncWindowTarget() with repository_url = %+v, want %+v", got, want)
	}
}

func TestSummarizeDeployResults(t *testing.T) {
	t.Parallel()

	nextOpen := time.Date(2026, time.March, 10, 11, 0, 0, 0, time.UTC)
	blockedWeb := &stages.SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}, NextOpen: nextOpen.Add(time.Hour)}
	blockedAPI := &stages.SyncWindowBlockedError{Stacks: []string{"api"}, Windows: []string{"freeze"}, NextOpen: nextOpen}
	failure := errors.New("boom")

	tests := []struct {
		name    string
		results []error
		want    error
	}{
		{name: "all deployed", results: []error{nil, nil}, want: nil},
		{name: "deployed and deferred", results: []error{nil, blockedWeb}, want: nil},
		{name: "all unchanged", results: []error{stages.ErrSkipDeployment, stages.ErrSkipDeployment}, want: stages.ErrSkipDeployment},
		{name: "all filtered", results: []error{stages.ErrWebhookFilterMismatch}, want: stages.ErrWebhookFilterMismatch},
		{name: "failure wins over deferral", results: []error{failure, blockedWeb}, want: failure},
		{name: "deferred, unchanged and filtered", results: []error{blockedWeb, stages.ErrSkipDeployment, stages.ErrWebhookFilterMismatch, blockedAPI}, want: stages.ErrSyncWindowBlocked},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := summarizeDeployResults(tt.results, len(tt.results))

			if tt.want == nil {
				if got != nil {
					t.Fatalf("summarizeDeployResults() = %v, want nil", got)
				}

				return
			}

			if !errors.Is(got, tt.want) {
				t.Fatalf("summarizeDeployResults() = %v, want %v", got, tt.want)
			}

			if errors.Is(tt.want, stages.ErrSyncWindowBlocked) {
				merged, ok := errors.AsType[*stages.SyncWindowBlockedError](got)
				if !ok {
					t.Fatalf("summarizeDeployResults() = %v, want *stages.SyncWindowBlockedError", got)
				}

				if strings.Join(merged.Stacks, ",") != "api,web" || !merged.NextOpen.Equal(nextOpen) {
					t.Fatalf("got merged stacks %v until %v, want [api web] until %v", merged.Stacks, merged.NextOpen, nextOpen)
				}
			}
		})
	}
}

func TestReconciliationJobInfo(t *testing.T) {
	t.Parallel()

	oldWeb := &deployConfig.Config{Name: "web"}
	oldDB := &deployConfig.Config{Name: "db"}
	oldOther := &deployConfig.Config{Name: "web", Context: "other"}
	previous := &job{info: DeployRequest{
		Repository:    stages.RepositoryData{Name: "github.com/acme/app", Revision: "rev-1"},
		DeployConfigs: []*deployConfig.Config{oldWeb, oldDB, oldOther},
	}}

	newWeb := &deployConfig.Config{Name: "web"}
	newDB := &deployConfig.Config{Name: "db"}
	newStack := &deployConfig.Config{Name: "new"}
	req := DeployRequest{
		Repository:    stages.RepositoryData{Name: "github.com/acme/app", Revision: "rev-2"},
		DeployConfigs: []*deployConfig.Config{newWeb, newDB, newStack},
	}

	t.Run("nothing deferred", func(t *testing.T) {
		t.Parallel()

		info, carried, pinned := reconciliationJobInfo(req, nil, previous)
		if carried != nil || pinned != nil || len(info.DeployConfigs) != 3 || info.DeployConfigs[0] != newWeb {
			t.Fatalf("got configs %v, carried %v and pinned %v, want the request unchanged", info.DeployConfigs, carried, pinned)
		}
	})

	t.Run("deferred without previous job", func(t *testing.T) {
		t.Parallel()

		info, carried, pinned := reconciliationJobInfo(req, map[*deployConfig.Config]struct{}{newWeb: {}}, nil)
		if len(carried) != 0 {
			t.Fatalf("carried = %v, want none", carried)
		}

		if len(info.DeployConfigs) != 2 || info.DeployConfigs[0] != newDB || info.DeployConfigs[1] != newStack {
			t.Fatalf("got configs %v, want the deferred stack to be excluded", info.DeployConfigs)
		}

		if len(pinned) != 1 || pinned[0] != newWeb {
			t.Fatalf("pinned = %v, want the deferred web config", pinned)
		}
	})

	t.Run("deferred stacks carry the previous revision", func(t *testing.T) {
		t.Parallel()

		info, carried, pinned := reconciliationJobInfo(req,
			map[*deployConfig.Config]struct{}{newWeb: {}, newStack: {}}, previous)

		if info.Repository.Revision != "rev-2" {
			t.Fatalf("job revision = %q, want rev-2", info.Repository.Revision)
		}

		// web on the default context is carried; web on "other" is a
		// different stack; the new stack is unknown to the previous job.
		if len(info.DeployConfigs) != 2 || info.DeployConfigs[0] != oldWeb || info.DeployConfigs[1] != newDB {
			t.Fatalf("got configs %v, want [old web, new db]", info.DeployConfigs)
		}

		source, ok := carried[oldWeb]
		if !ok || len(carried) != 1 {
			t.Fatalf("carried = %v, want only the old web config", carried)
		}

		if len(pinned) != 1 || pinned[0] != newStack {
			t.Fatalf("pinned = %v, want the new stack unknown to the previous job", pinned)
		}

		if source.Repository.Revision != "rev-1" || source.DeployConfigs != nil {
			t.Fatalf("carried source = %+v, want a snapshot of rev-1 without deploy configs", source)
		}

		// A job built from the result restores web from rev-1.
		next := &job{info: info, carried: carried}
		if got := next.requestFor(oldWeb).Repository.Revision; got != "rev-1" {
			t.Fatalf("requestFor(old web) revision = %q, want rev-1", got)
		}

		if got := next.requestFor(newDB).Repository.Revision; got != "rev-2" {
			t.Fatalf("requestFor(new db) revision = %q, want rev-2", got)
		}

		// A later deferral keeps the original source instead of the job's own request.
		latest := DeployRequest{
			Repository:    stages.RepositoryData{Name: "github.com/acme/app", Revision: "rev-3"},
			DeployConfigs: []*deployConfig.Config{{Name: "web"}, {Name: "db"}},
		}

		chained, chainedCarried, _ := reconciliationJobInfo(latest,
			map[*deployConfig.Config]struct{}{latest.DeployConfigs[0]: {}}, next)
		if chained.DeployConfigs[0] != oldWeb || chainedCarried[oldWeb] != source {
			t.Fatalf("chained carry = %v, want the old web config from rev-1", chainedCarried)
		}
	})

	t.Run("duplicate deferred configs are carried once", func(t *testing.T) {
		t.Parallel()

		duplicate := &deployConfig.Config{Name: "web"}
		dupReq := DeployRequest{DeployConfigs: []*deployConfig.Config{newWeb, duplicate}}

		info, carried, pinned := reconciliationJobInfo(dupReq,
			map[*deployConfig.Config]struct{}{newWeb: {}, duplicate: {}}, previous)
		if len(info.DeployConfigs) != 1 || len(carried) != 1 || len(pinned) != 0 {
			t.Fatalf("got configs %v, carried %v and pinned %v, want the old web config once", info.DeployConfigs, carried, pinned)
		}
	})
}

func TestJobGroupByRequest(t *testing.T) {
	t.Parallel()

	own := &deployConfig.Config{Name: "own"}
	carriedA := &deployConfig.Config{Name: "a"}
	carriedB := &deployConfig.Config{Name: "b"}
	carriedC := &deployConfig.Config{Name: "c"}
	sourceOne := &DeployRequest{Repository: stages.RepositoryData{Revision: "rev-1"}}
	sourceTwo := &DeployRequest{Repository: stages.RepositoryData{Revision: "rev-2"}}

	j := &job{
		info: DeployRequest{Repository: stages.RepositoryData{Revision: "rev-3"}},
		carried: map[*deployConfig.Config]*DeployRequest{
			carriedA: sourceOne,
			carriedB: sourceTwo,
			carriedC: sourceOne,
		},
	}

	groups := j.groupByRequest([]*deployConfig.Config{carriedA, own, carriedB, carriedC})
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}

	for i, want := range []struct {
		revision string
		configs  []*deployConfig.Config
	}{
		{revision: "rev-3", configs: []*deployConfig.Config{own}},
		{revision: "rev-1", configs: []*deployConfig.Config{carriedA, carriedC}},
		{revision: "rev-2", configs: []*deployConfig.Config{carriedB}},
	} {
		group := groups[i]
		if group.request.Repository.Revision != want.revision || len(group.configs) != len(want.configs) {
			t.Fatalf("group %d = %s with %d configs, want %s with %d", i,
				group.request.Repository.Revision, len(group.configs), want.revision, len(want.configs))
		}

		for k := range want.configs {
			if group.configs[k] != want.configs[k] {
				t.Fatalf("group %d config %d = %s, want %s", i, k, group.configs[k].Name, want.configs[k].Name)
			}
		}
	}

	// The job's own request is used even without configs.
	groups = j.groupByRequest(nil)
	if len(groups) != 1 || groups[0].request.Repository.Revision != "rev-3" || len(groups[0].configs) != 0 {
		t.Fatalf("groupByRequest(nil) = %+v, want only the job's own request", groups)
	}

	groups = j.groupByRequest([]*deployConfig.Config{carriedB})
	if len(groups) != 1 || groups[0].request.Repository.Revision != "rev-2" {
		t.Fatalf("groupByRequest(carried only) = %+v, want only the carried source", groups)
	}
}

func TestCleanupObsoleteAutoDiscoveredContainers_KeepsStacksRejectedByPredicate(t *testing.T) {
	t.Parallel()

	// The fake client only lists containers, so removing the stack would panic.
	apiClient := &cleanupTestClient{
		containers: []container.Summary{
			{
				Names: []string{"/web-old-app"},
				Labels: map[string]string{
					docker.DocoCDLabels.Deployment.Name:          "web-old",
					docker.DocoCDLabels.Deployment.AutoDiscovery: "true",
					docker.DocoCDLabels.Source.URL:               "https://example.com/organization/repository.git",
				},
			},
		},
	}

	var checked []string

	err := cleanupObsoleteAutoDiscoveredContainers(
		t.Context(),
		slog.New(slog.DiscardHandler),
		cleanupTestCLI{apiClient: apiClient},
		false,
		"",
		"https://example.com/organization/repository.git",
		nil,
		notification.Metadata{},
		nil,
		func(_ *slog.Logger, stackName string) bool {
			checked = append(checked, stackName)

			return false
		},
	)
	if err != nil {
		t.Fatalf("cleanupObsoleteAutoDiscoveredContainers() error = %v", err)
	}

	if strings.Join(checked, ",") != "web-old" {
		t.Fatalf("predicate checked %v, want [web-old]", checked)
	}
}

func TestJobCleanupConfigsIncludePinnedStacks(t *testing.T) {
	t.Parallel()

	own := &deployConfig.Config{Name: "own"}
	pinned := &deployConfig.Config{Name: "pinned"}
	pinnedOther := &deployConfig.Config{Name: "pinned-other", Context: "other"}

	j := &job{
		info:   DeployRequest{DeployConfigs: []*deployConfig.Config{own}},
		pinned: []*deployConfig.Config{pinned, pinnedOther},
	}

	got := j.cleanupConfigsForContextMode("", false)
	if len(got) != 2 || got[0] != own || got[1] != pinned {
		t.Fatalf("cleanupConfigsForContextMode() = %v, want [own pinned]", got)
	}

	// Pinned stacks are kept from cleanup, not reconciled.
	if reconciled := j.deployConfigsForContextMode("", false); len(reconciled) != 1 || reconciled[0] != own {
		t.Fatalf("deployConfigsForContextMode() = %v, want [own]", reconciled)
	}
}
