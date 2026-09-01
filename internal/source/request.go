package source

import (
	"log/slog"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// Request holds the per-call input for Preparer.Prepare: the source location
// and its trigger/reference/visibility, an optional custom deploy target,
// poll configuration (used only for poll-triggered requests), the parsed
// webhook payload (zero value for non-webhook triggers), and the data mount
// point used to compute safe internal/external filesystem paths.
type Request struct {
	Logger         *slog.Logger      `validate:"required,nostructlevel"`
	JobTrigger     stages.JobTrigger `validate:"required,oneof=webhook poll"`
	SourceType     config.SourceType
	SourceRef      string `validate:"required"`
	Ref            string
	Private        bool
	CustomTarget   string
	PollConfig     poll.Config
	Payload        webhook.ParsedPayload
	DataMountPoint container.MountPoint `validate:"required"`
}
