// Command healthcheck checks the health endpoint of the doco-cd server running
// in the same container. The image HEALTHCHECK runs it every 30 seconds.
//
// It is a separate binary because starting the doco-cd binary runs the init
// code of all of its dependencies before main, which reads tens of MiB of the
// binary into the page cache on every check. Keep its dependencies small.
package main

import (
	"context"
	"os"

	"github.com/kimdre/doco-cd/cmd/doco-cd/healthcheck"
)

func main() {
	if err := healthcheck.Run(context.Background(), os.LookupEnv); err != nil {
		os.Exit(1)
	}
}
