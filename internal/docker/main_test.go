package docker

import (
	"os"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// TestMain shortens the recovery retry pause so failure-path tests do not
// spend most of their time sleeping.
func TestMain(m *testing.M) {
	selfupdate.RetryPause = time.Millisecond

	os.Exit(m.Run())
}
