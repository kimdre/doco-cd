package mcp

import (
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

func TestMCPListSyncWindows(t *testing.T) {
	// A deny window that is always active: it starts every minute and lasts
	// two minutes.
	policy, err := syncwindow.New([]syncwindow.Window{{
		Name:         "freeze",
		Kind:         syncwindow.KindDeny,
		Schedule:     "* * * * *",
		Duration:     2 * time.Minute,
		Repositories: []string{"github.com/acme/*"},
		ManualSync:   true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{log: logger.New(logger.LevelCritical), syncWindows: policy}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	var report syncwindow.Report
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_sync_windows", map[string]any{}), &report)

	if len(report.Windows) != 1 || report.Windows[0].Name != "freeze" || !report.Windows[0].Active || report.Decision != nil {
		t.Fatalf("report without target = %+v", report)
	}

	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_sync_windows", map[string]any{
		"repository": "github.com/acme/app",
	}), &report)

	if report.Decision == nil || report.Decision.Allowed || len(report.Decision.Windows) != 1 {
		t.Fatalf("automatic decision = %+v", report.Decision)
	}

	report = syncwindow.Report{}
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_sync_windows", map[string]any{
		"repository": "github.com/acme/app",
		"manual":     true,
	}), &report)

	if report.Decision == nil || !report.Decision.Allowed || !report.Decision.ManualOverride {
		t.Fatalf("manual decision = %+v", report.Decision)
	}
}

func TestMCPListSyncWindowsWithoutPolicy(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	var report syncwindow.Report
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_sync_windows", map[string]any{
		"deployment": "web",
	}), &report)

	if report.Windows == nil || len(report.Windows) != 0 || report.Decision == nil || !report.Decision.Allowed {
		t.Fatalf("report without policy = %+v", report)
	}
}
