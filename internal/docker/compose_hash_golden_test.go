package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// composeV571DococdHash is Compose v5.7.1 ServiceHash for Name doco-cd and image ghcr.io/kimdre/doco-cd:1.2.3.
// The literal stays fixed so a local change to the hash bytes fails here.
const composeV571DococdHash = "fea97d9cf80d1745aafa7e93dc3f087ae386ccfc2df91af438263f6913fb710a"

func TestComposeServiceHashMatchesComposeV571(t *testing.T) {
	t.Parallel()

	got, err := composeServiceHash(types.ServiceConfig{
		Name:  "doco-cd",
		Image: "ghcr.io/kimdre/doco-cd:1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got != composeV571DococdHash {
		t.Fatalf("composeServiceHash() = %s; want the Compose v5.7.1 hash %s", got, composeV571DococdHash)
	}
}
