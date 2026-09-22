//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ociRegistryImage is a plain-HTTP OCI registry the e2e suite runs itself, so
// OCI scenarios don't depend on an externally published fixture image.
const ociRegistryImage = "registry:2"

// ociRegistryPort is the port registry:2 listens on inside its container,
// both for the exposed/mapped host port and for the per-network references
// PushOCIArtifact hands to containers on the scenario's own network.
const ociRegistryPort = "5000"

// ociTestRegistry is a single registry container shared by every scenario in
// the suite. A container can join more than one Docker network, so instead
// of building and starting a fresh registry per scenario, each harness
// connects this one to its own per-scenario network on first use and
// addresses it by its IP on that network.
type ociTestRegistry struct {
	container testcontainers.Container
	docker    *client.Client
	hostAddr  string // host:port reachable from the test process itself, used for pushes

	mu  sync.Mutex
	ips map[string]string // network name -> registry IP address on that network
}

var (
	ociRegistryOnce      sync.Once
	sharedOCIRegistry    *ociTestRegistry
	errSharedOCIRegistry error
)

// ensureOCIRegistry starts the shared registry on first use and returns it.
func ensureOCIRegistry(ctx context.Context, dockerCli *client.Client) (*ociTestRegistry, error) {
	ociRegistryOnce.Do(func() {
		sharedOCIRegistry, errSharedOCIRegistry = startOCIRegistry(ctx, dockerCli)
	})

	return sharedOCIRegistry, errSharedOCIRegistry
}

func startOCIRegistry(ctx context.Context, dockerCli *client.Client) (*ociTestRegistry, error) {
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        ociRegistryImage,
			Name:         fmt.Sprintf("doco-cd-e2e-registry-%d", os.Getpid()),
			ExposedPorts: []string{ociRegistryPort + "/tcp"},
			// ForHTTP dials through the published host port, unlike
			// ForListeningPort (which can report ready before Docker's port
			// publishing is actually reachable from the host), so it also
			// proves the address startOCIRegistry hands back for pushes works.
			WaitingFor: wait.ForHTTP("/v2/").WithPort(ociRegistryPort + "/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("start OCI registry: %w", err)
	}

	reg := &ociTestRegistry{
		container: c,
		docker:    dockerCli,
		ips:       map[string]string{},
	}

	if err := reg.refreshHostAddr(ctx); err != nil {
		return nil, err
	}

	return reg, nil
}

// pushArtifact attaches the registry to netName (connecting it if this is the
// first scenario to use that network) and pushes an artifact via push,
// returning a reference reachable from containers on netName. push is called
// with a reference resolved against the registry's current host-side
// address.
//
// Attach and push are done as one atomic operation under r.mu, not two:
// Docker can reallocate the registry's published host port whenever it gains
// an additional network connection (see refreshHostAddr), and scenarios run
// in parallel, each with their own network. Resolving the host address once
// and pushing afterwards - outside the lock - would leave a window for
// another scenario's NetworkConnect to invalidate the address before this
// push actually happens.
func (r *ociTestRegistry) pushArtifact(ctx context.Context, netName, repo, tag string, push func(ref name.Reference) error) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ip, ok := r.ips[netName]
	if !ok {
		id := r.container.GetContainerID()

		if _, err := r.docker.NetworkConnect(ctx, netName, client.NetworkConnectOptions{Container: id}); err != nil {
			return "", fmt.Errorf("connect OCI registry to network %s: %w", netName, err)
		}

		inspect, err := r.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return "", fmt.Errorf("inspect OCI registry: %w", err)
		}

		endpoint, ok := inspect.Container.NetworkSettings.Networks[netName]
		if !ok {
			return "", fmt.Errorf("OCI registry has no endpoint on network %s", netName)
		}

		ip = endpoint.IPAddress.String()
		r.ips[netName] = ip

		// See the refreshHostAddr doc comment: the host port can have just
		// changed as a side effect of the NetworkConnect above.
		if err := r.refreshHostAddr(ctx); err != nil {
			return "", fmt.Errorf("resolve OCI registry port after connecting network %s: %w", netName, err)
		}
	}

	pushRef, err := name.ParseReference(fmt.Sprintf("%s/%s:%s", r.hostAddr, repo, tag), name.WeakValidation)
	if err != nil {
		return "", fmt.Errorf("parse OCI push reference: %w", err)
	}

	if err := push(pushRef); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s:%s/%s:%s", ip, ociRegistryPort, repo, tag), nil
}

// refreshHostAddr re-resolves the registry's published host port and waits
// for it to accept connections, updating r.hostAddr. Callers must hold r.mu.
func (r *ociTestRegistry) refreshHostAddr(ctx context.Context) error {
	port, err := r.container.MappedPort(ctx, ociRegistryPort+"/tcp")
	if err != nil {
		return fmt.Errorf("resolve OCI registry port: %w", err)
	}

	// Force IPv4 loopback rather than r.container.Host(ctx) ("localhost"):
	// the published port only binds an IPv4 listener, and some environments
	// resolve "localhost" to the IPv6 loopback first, which then just
	// refuses the connection.
	addr := net.JoinHostPort("127.0.0.1", port.Port())

	if err := waitForHostPort(ctx, addr); err != nil {
		return err
	}

	r.hostAddr = addr

	return nil
}

// waitForHostPort polls addr until a TCP connection succeeds or ctx's
// deadline (capped at 10s) runs out.
func waitForHostPort(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var lastErr error

	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()

			return nil
		}

		lastErr = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s to accept connections: %w", addr, lastErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// detachFromNetwork disconnects the registry from netName, so the network
// can be removed during teardown. It's a no-op if the registry was never
// attached to netName.
func (r *ociTestRegistry) detachFromNetwork(ctx context.Context, netName string) {
	r.mu.Lock()
	_, attached := r.ips[netName]
	delete(r.ips, netName)
	r.mu.Unlock()

	if !attached {
		return
	}

	_, _ = r.docker.NetworkDisconnect(ctx, netName, client.NetworkDisconnectOptions{
		Container: r.container.GetContainerID(),
		Force:     true,
	})
}

// terminateSharedOCIRegistry tears down the suite-wide registry container, if
// one was ever started. Call from TestMain after all tests have run.
func terminateSharedOCIRegistry() {
	if sharedOCIRegistry == nil {
		return
	}

	_ = sharedOCIRegistry.container.Terminate(context.Background(), testcontainers.StopTimeout(e2eStopTimeout))
}

// pushOCIArtifact builds a minimal doco-cd v1 layout artifact from files and
// pushes it to ref, returning its digest. It mirrors
// internal/source/prepare_oci_test.go's pushOCIFixture for e2e use.
func pushOCIArtifact(ref name.Reference, files map[string]string) (string, error) {
	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for fileName, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: fileName, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return "", fmt.Errorf("write tar header for %s: %w", fileName, err)
		}

		if _, err := tw.Write([]byte(content)); err != nil {
			return "", fmt.Errorf("write tar content for %s: %w", fileName, err)
		}
	}

	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("close tar writer: %w", err)
	}

	tarBytes := buf.Bytes()

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(tarBytes)), nil
	})
	if err != nil {
		return "", fmt.Errorf("build layer: %w", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return "", fmt.Errorf("append layer: %w", err)
	}

	if err := remote.Write(ref, img); err != nil {
		return "", fmt.Errorf("push test artifact: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("compute digest: %w", err)
	}

	return digest.String(), nil
}

// composeProjectArtifactType, composeYAMLMediaType and composeEnvFileMediaType
// mirror the unexported constants of the same name in
// github.com/docker/compose/v5/internal/oci, which can't be imported directly
// (it's an internal package). The compose "include: oci://..." loader checks
// the manifest's artifactType and each layer's mediaType against these exact
// strings to recognize a compose project artifact.
const (
	composeProjectArtifactType = "application/vnd.docker.compose.project"
	composeYAMLMediaType       = "application/vnd.docker.compose.file+yaml"
	composeEnvFileMediaType    = "application/vnd.docker.compose.envfile"
)

// ociEmptyJSONMediaType and ociEmptyJSONContent are OCI's well-known "no
// config" placeholder (github.com/opencontainers/image-spec's
// DescriptorEmptyJSON), used as the compose project artifact's config blob.
const (
	ociEmptyJSONMediaType = "application/vnd.oci.empty.v1+json"
	ociEmptyJSONContent   = "{}"
)

// pushComposeOCIArtifact pushes files as a compose project OCI artifact -
// the shape docker/compose's own "include: oci://..." loader expects, which
// is unrelated to doco-cd's own OCI artifact layout pushOCIArtifact builds.
// Each file becomes its own layer (raw content, not a tar archive), typed by
// extension as either a compose file or an env file.
func pushComposeOCIArtifact(ref name.Reference, files map[string]string) (string, error) {
	repo := ref.Context()

	config := static.NewLayer([]byte(ociEmptyJSONContent), ociEmptyJSONMediaType)
	if err := remote.WriteLayer(repo, config); err != nil {
		return "", fmt.Errorf("push config blob: %w", err)
	}

	configDigest, err := config.Digest()
	if err != nil {
		return "", fmt.Errorf("compute config digest: %w", err)
	}

	configSize, err := config.Size()
	if err != nil {
		return "", fmt.Errorf("compute config size: %w", err)
	}

	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  composeProjectArtifactType,
		Config: v1.Descriptor{
			MediaType: ociEmptyJSONMediaType,
			Digest:    configDigest,
			Size:      configSize,
		},
	}

	// Sorted for a deterministic layer order: compose's loader treats the
	// first compose-file layer specially (always writes it to
	// "compose.yaml"), so iterating files in map order would make that
	// choice depend on random map iteration.
	fileNames := make([]string, 0, len(files))
	for fileName := range files {
		fileNames = append(fileNames, fileName)
	}

	sort.Strings(fileNames)

	for _, fileName := range fileNames {
		content := files[fileName]

		mediaType := composeYAMLMediaType
		if ext := filepath.Ext(fileName); ext == ".env" {
			mediaType = composeEnvFileMediaType
		}

		layer := static.NewLayer([]byte(content), types.MediaType(mediaType))
		if err := remote.WriteLayer(repo, layer); err != nil {
			return "", fmt.Errorf("push layer for %s: %w", fileName, err)
		}

		digest, err := layer.Digest()
		if err != nil {
			return "", fmt.Errorf("compute digest for %s: %w", fileName, err)
		}

		size, err := layer.Size()
		if err != nil {
			return "", fmt.Errorf("compute size for %s: %w", fileName, err)
		}

		manifest.Layers = append(manifest.Layers, v1.Descriptor{
			MediaType:   types.MediaType(mediaType),
			Digest:      digest,
			Size:        size,
			Annotations: map[string]string{"com.docker.compose.file": fileName},
		})
	}

	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal compose project manifest: %w", err)
	}

	if err := remote.Put(ref, rawManifest{raw: raw, mediaType: types.OCIManifestSchema1}); err != nil {
		return "", fmt.Errorf("push compose project manifest: %w", err)
	}

	digest, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("compute manifest digest: %w", err)
	}

	return digest.String(), nil
}

// rawManifest lets pushComposeOCIArtifact hand remote.Put an already built
// manifest (with fields, like artifactType, that go-containerregistry's
// higher-level v1.Image/mutate APIs don't expose a way to set) instead of
// building it up through an image and its mutators.
type rawManifest struct {
	raw       []byte
	mediaType types.MediaType
}

func (m rawManifest) RawManifest() ([]byte, error) { return m.raw, nil }

func (m rawManifest) MediaType() (types.MediaType, error) { return m.mediaType, nil }
