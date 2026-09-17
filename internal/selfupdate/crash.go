package selfupdate

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// CrashEnvVar names the journal state at which the process must die, to test
// crash recovery. Never set it outside tests.
const CrashEnvVar = "SELF_UPDATE_CRASH_AT"

// crashExitCode is a self-exit, so Docker's restart policy brings the
// container back and the recovery path can run.
const crashExitCode = 137

// MaybeCrash exits the process when the configured crash point is reached for
// the first time. The marker on the data volume makes it fire once, so the
// restarted actor can make progress.
func MaybeCrash(dataMountPath, point string, log *slog.Logger) {
	maybeCrash(dataMountPath, point, log, os.Getenv(CrashEnvVar), os.Exit)
}

func maybeCrash(dataMountPath, point string, log *slog.Logger, want string, exit func(int)) {
	if want == "" || want != point {
		return
	}

	root := filepath.Join(dataMountPath, "self-update")
	marker := filepath.Join(root, "crash-"+point)

	if _, err := os.Stat(marker); err == nil {
		return
	}

	if err := os.MkdirAll(root, filesystem.PermDir); err != nil {
		return
	}

	if err := os.WriteFile(marker, []byte(point), filesystem.PermOwner); err != nil {
		return
	}

	if log != nil {
		log.Error("self-update: crash hook fired", slog.String("point", point))
	}

	exit(crashExitCode)
}
