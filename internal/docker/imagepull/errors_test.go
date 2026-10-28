package imagepull

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsTransientError(t *testing.T) {
	t.Parallel()

	errExtract := errors.New(`failed to extract layer (application/vnd.oci.image.layer.v1.tar+gzip ` +
		`sha256:86ef36ea04a771290b854b5d946f6e957915e22910f5fc7c1edd6d66509e8e24) to overlayfs as ` +
		`"extract-537539137-x9rV sha256:7e08cb67e7cfae2519b21255fab291a8f565854050a63b9aada56cb17dfbc81e": ` +
		`failed to Lchown "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/134/fs/usr/bin/bao" ` +
		`for UID 0, GID 0: lchown /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/134/fs/usr/bin/bao: ` +
		`no such file or directory`)

	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "lost lease", err: errors.New("unable to lease content: lease does not exist: not found"), want: true},
		{name: "wrapped lost lease", err: fmt.Errorf("failed to pull images: %w", errors.New("Unable to lease content: Lease does not exist")), want: true},
		{name: "failed commit", err: errors.New("failed commit on ref \"layer-sha256:abc\": rename: no such file or directory"), want: true},
		{name: "wrapped failed commit", err: fmt.Errorf("failed to pull images: %w", errors.New("Failed commit on ref \"layer-sha256:abc\"")), want: true},
		{name: "missing file during layer extraction", err: errExtract, want: true},
		{name: "wrapped layer extraction", err: fmt.Errorf("failed to pull images: %w", errExtract), want: true},
		{name: "mixed-case layer extraction", err: errors.New("Failed to Extract Layer: lchown /snapshot/usr/bin/bao: No Such File or Directory"), want: true},
		{name: "layer extraction permission denied", err: errors.New("failed to extract layer: lchown /snapshot/usr/bin/bao: permission denied"), want: false},
		{name: "layer extraction disk full", err: errors.New("failed to extract layer: write /snapshot/usr/bin/bao: no space left on device"), want: false},
		{name: "layer extraction corrupt archive", err: errors.New("failed to extract layer: archive/tar: invalid tar header"), want: false},
		{name: "layer extraction unsupported ownership", err: errors.New("failed to extract layer: lchown /snapshot/usr/bin/bao: operation not permitted"), want: false},
		{name: "unrelated missing file", err: errors.New("open /tmp/compose.yml: no such file or directory"), want: false},
		{name: "unrelated missing snapshot file", err: errors.New("lchown /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/134/fs/usr/bin/bao: no such file or directory"), want: false},
		{name: "access denied", err: errors.New("pull access denied for nginx, repository does not exist"), want: false},
		{name: "network", err: errors.New("dial tcp: connection refused"), want: false},
		{name: "missing image", err: errors.New("No such image: nginx:latest"), want: false},
		{name: "canceled", err: context.Canceled, want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := IsTransientError(tc.err); got != tc.want {
				t.Errorf("IsTransientError() = %v, want %v", got, tc.want)
			}
		})
	}
}
