package source

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// pushOCIFixture builds a minimal doco-cd v1 layout artifact from files and
// pushes it to ref on an in-process registry, returning its digest.
func pushOCIFixture(t *testing.T, ref name.Reference, files map[string]string) string {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for fileName, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: fileName, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("write tar header for %s: %v", fileName, err)
		}

		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar content for %s: %v", fileName, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	tarBytes := buf.Bytes()

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(tarBytes)), nil
	})
	if err != nil {
		t.Fatalf("build layer: %v", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}

	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push test artifact: %v", err)
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("compute digest: %v", err)
	}

	return digest.String()
}

// newTestOCIRegistryHost starts an in-process OCI registry and returns its
// host:port, so multiple refs (e.g. different tags of the same repo) can be
// parsed against the same registry instance.
func newTestOCIRegistryHost(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://")
}

// ociTestRef parses a repo:tag reference against host.
func ociTestRef(t *testing.T, host, tag string) name.Reference {
	t.Helper()

	ref, err := name.ParseReference(fmt.Sprintf("%s/repo:%s", host, tag), name.WeakValidation)
	if err != nil {
		t.Fatalf("parse test reference for tag %q: %v", tag, err)
	}

	return ref
}

// newTestOCIRegistryRef starts an in-process OCI registry and returns a
// parsed reference for repo:tag on it.
func newTestOCIRegistryRef(t *testing.T, tag string) name.Reference {
	t.Helper()

	return ociTestRef(t, newTestOCIRegistryHost(t), tag)
}

func TestPrepare_OCISuccess(t *testing.T) {
	t.Parallel()

	ref := newTestOCIRegistryRef(t, "latest")
	digest := pushOCIFixture(t, ref, map[string]string{
		".doco-cd.yaml":     testDeployConfigYAML,
		"test.compose.yaml": "services: {}\n",
	})

	p := newTestPreparer(t, &app.Config{})

	result, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     config.SourceTypeOCI,
		SourceRef:      ref.Name(),
		Payload:        webhook.ParsedPayload{},
		DataMountPoint: testMountPoint(t),
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if result.SourceType != config.SourceTypeOCI {
		t.Fatalf("SourceType = %q, want %q", result.SourceType, config.SourceTypeOCI)
	}

	if result.Revision != digest {
		t.Fatalf("Revision = %q, want %q", result.Revision, digest)
	}

	if !result.OCITrusted {
		t.Fatal("expected OCITrusted to be true when no trust policy is configured")
	}

	if result.Payload.Source != webhook.PayloadSourceOCI {
		t.Fatalf("Payload.Source = %q, want %q", result.Payload.Source, webhook.PayloadSourceOCI)
	}

	if result.Payload.Digest != digest {
		t.Fatalf("Payload.Digest = %q, want %q", result.Payload.Digest, digest)
	}

	// The artifact must be published as its own immutable, per-digest
	// artifact directory - not extracted directly into the store's base
	// directory - mirroring the Git store's layout.
	wantSuffix := filepath.Join(store.ArtifactsSubdir, store.ArtifactDirName(store.Revision(digest)))
	if !strings.HasSuffix(result.PathInternal, wantSuffix) {
		t.Fatalf("PathInternal = %q, want suffix %q", result.PathInternal, wantSuffix)
	}

	if len(result.DeployConfigs) != 1 {
		t.Fatalf("expected 1 deploy config, got %d", len(result.DeployConfigs))
	}

	if got := result.DeployConfigs[0].Internal.ConfigTarget; got != "" {
		t.Fatalf("expected no custom target, got %q", got)
	}
}

// TestPrepare_OCIConcurrentDifferentDigests_NonInterfering proves that two
// concurrent Prepare calls against the same OCI repository, resolving two
// distinct tags/digests, do not corrupt or interfere with each other and
// each produce their own correct, independent artifact - the OCI analogue of
// TestPrepare_ConcurrentDifferentRevisions_NonInterfering.
func TestPrepare_OCIConcurrentDifferentDigests_NonInterfering(t *testing.T) {
	t.Parallel()

	host := newTestOCIRegistryHost(t)

	refV1 := ociTestRef(t, host, "v1")
	refV2 := ociTestRef(t, host, "v2")

	digestV1 := pushOCIFixture(t, refV1, map[string]string{
		".doco-cd.yaml":     testDeployConfigYAML,
		"test.compose.yaml": "services: {}\n",
	})
	digestV2 := pushOCIFixture(t, refV2, map[string]string{
		".doco-cd.yaml":     testDeployConfigYAML,
		"test.compose.yaml": "services: {}\n# v2\n",
	})

	p := newTestPreparer(t, &app.Config{})
	mountPoint := testMountPoint(t)

	type outcome struct {
		result Result
		err    error
	}

	run := func(ref name.Reference) outcome {
		result, err := p.Prepare(t.Context(), Request{
			Logger:         logger.New(logger.LevelCritical).Logger,
			JobTrigger:     stages.JobTriggerWebhook,
			SourceType:     config.SourceTypeOCI,
			SourceRef:      ref.Name(),
			Payload:        webhook.ParsedPayload{},
			DataMountPoint: mountPoint,
		})

		return outcome{result: result, err: err}
	}

	var (
		wg                   sync.WaitGroup
		outcomeV1, outcomeV2 outcome
	)

	wg.Add(2)

	go func() {
		defer wg.Done()

		outcomeV1 = run(refV1)
	}()

	go func() {
		defer wg.Done()

		outcomeV2 = run(refV2)
	}()

	wg.Wait()

	if outcomeV1.err != nil {
		t.Fatalf("Prepare(v1) error = %v", outcomeV1.err)
	}

	if outcomeV2.err != nil {
		t.Fatalf("Prepare(v2) error = %v", outcomeV2.err)
	}

	if outcomeV1.result.Revision != digestV1 {
		t.Fatalf("Prepare(v1) revision = %q, want %q", outcomeV1.result.Revision, digestV1)
	}

	if outcomeV2.result.Revision != digestV2 {
		t.Fatalf("Prepare(v2) revision = %q, want %q", outcomeV2.result.Revision, digestV2)
	}

	if outcomeV1.result.PathInternal == outcomeV2.result.PathInternal {
		t.Fatalf("expected distinct artifact paths for distinct digests, both got %q", outcomeV1.result.PathInternal)
	}

	if _, err := os.Stat(outcomeV1.result.PathInternal); err != nil {
		t.Fatalf("v1 artifact path missing: %v", err)
	}

	if _, err := os.Stat(outcomeV2.result.PathInternal); err != nil {
		t.Fatalf("v2 artifact path missing: %v", err)
	}
}

func TestPrepare_OCIVerifyFailure(t *testing.T) {
	t.Parallel()

	ref := newTestOCIRegistryRef(t, "latest")
	pushOCIFixture(t, ref, map[string]string{".doco-cd.yaml": testDeployConfigYAML})

	p := newTestPreparer(t, &app.Config{
		OciTrustPolicy: config.OciTrustPolicy{
			Enabled: true,
			KeylessIdentities: []config.OciKeylessIdentity{
				{Issuer: "https://example.com", Subject: "nobody"},
			},
		},
	})

	_, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     config.SourceTypeOCI,
		SourceRef:      ref.Name(),
		Payload:        webhook.ParsedPayload{},
		DataMountPoint: testMountPoint(t),
	})
	if !errors.Is(err, ErrOCIVerify) {
		t.Fatalf("Prepare() error = %v, want wrapping ErrOCIVerify", err)
	}
}
