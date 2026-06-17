package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.yaml.in/yaml/v3"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

var (
	ErrDigestMismatch        = errors.New("artifact digest does not match expected digest")
	ErrUnsupportedLayout     = errors.New("unsupported OCI layout")
	ErrInvalidArtifactLayout = errors.New("invalid OCI artifact layout")
)

type PullResult struct {
	Digest string
}

// ParseArtifact parses an OCI artifact reference and returns the repository name and tag.
// For "ghcr.io/org/repo:latest" it returns ("ghcr.io/org/repo", "latest").
// Tag defaults to "latest" when not specified (standard OCI behaviour).
// On parse failure the trimmed artifact string is returned as the repository name with an empty tag.
func ParseArtifact(artifact string) (repository, tag string) {
	trimmed := strings.TrimSpace(artifact)

	ref, err := name.ParseReference(trimmed, name.WeakValidation)
	if err != nil {
		return trimmed, ""
	}

	return ref.Context().Name(), ref.Identifier()
}

// RepositoryNameFromArtifact returns the repository portion of an OCI artifact reference.
func RepositoryNameFromArtifact(artifact string) string {
	repo, _ := ParseArtifact(artifact)
	return repo
}

// TagFromArtifact returns the tag (or digest identifier) of an OCI artifact reference.
func TagFromArtifact(artifact string) string {
	_, tag := ParseArtifact(artifact)
	return tag
}

func PullAndExtract(ctx context.Context, artifactRef, expectedDigest, layout, destination, customTarget string) (PullResult, error) {
	if strings.TrimSpace(layout) != config.OciArtifactLayoutV1 {
		return PullResult{}, fmt.Errorf("%w: %s", ErrUnsupportedLayout, layout)
	}

	ref, err := name.ParseReference(strings.TrimSpace(artifactRef), name.WeakValidation)
	if err != nil {
		return PullResult{}, fmt.Errorf("failed to parse OCI artifact reference: %w", err)
	}

	desc, err := remote.Get(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return PullResult{}, fmt.Errorf("failed to resolve OCI artifact: %w", err)
	}

	digest := desc.Digest.String()
	if strings.TrimSpace(expectedDigest) != "" && strings.TrimSpace(expectedDigest) != digest {
		return PullResult{}, fmt.Errorf("%w: expected %s got %s", ErrDigestMismatch, expectedDigest, digest)
	}

	img, err := desc.Image()
	if err != nil {
		return PullResult{}, fmt.Errorf("failed to read OCI artifact image manifest: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return PullResult{}, fmt.Errorf("failed to read OCI artifact layers: %w", err)
	}

	if len(layers) == 0 {
		return PullResult{}, fmt.Errorf("%w: no layers in artifact", ErrInvalidArtifactLayout)
	}

	if err := os.RemoveAll(destination); err != nil {
		return PullResult{}, fmt.Errorf("failed to reset artifact destination: %w", err)
	}

	if err := os.MkdirAll(destination, filesystem.PermDir); err != nil {
		return PullResult{}, fmt.Errorf("failed to create artifact destination: %w", err)
	}

	for _, layer := range layers {
		r, err := layer.Uncompressed()
		if err != nil {
			return PullResult{}, fmt.Errorf("failed to read artifact layer stream: %w", err)
		}

		err = extractTarStream(destination, r)
		_ = r.Close()

		if err != nil {
			return PullResult{}, err
		}
	}

	if err := validateDocoLayoutV1(destination, customTarget); err != nil {
		return PullResult{}, err
	}

	return PullResult{Digest: digest}, nil
}

func extractTarStream(destination string, reader io.Reader) error {
	tr := tar.NewReader(reader)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("failed to read tar stream: %w", err)
		}

		cleanName := filepath.Clean(h.Name)
		target := filepath.Join(destination, cleanName)

		if !filesystem.InBasePath(destination, target) {
			return fmt.Errorf("%w: %s", filesystem.ErrPathTraversal, h.Name)
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(uint32(h.Mode&0o777))); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), filesystem.PermDir); err != nil {
				return fmt.Errorf("failed to create parent directory for %s: %w", target, err)
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(uint32(h.Mode&0o777)))
			if err != nil {
				return fmt.Errorf("failed to create file %s: %w", target, err)
			}

			if _, err = io.CopyN(f, tr, h.Size); err != nil {
				if !errors.Is(err, io.EOF) {
					_ = f.Close()
					return fmt.Errorf("failed to extract file %s: %w", target, err)
				}

				_ = f.Close()
			}

			if err := f.Close(); err != nil {
				return fmt.Errorf("failed to close extracted file %s: %w", target, err)
			}
		}
	}
}

func validateDocoLayoutV1(destination, customTarget string) error {
	configFile, err := findArtifactConfigFile(destination, customTarget)
	if err != nil {
		if customTarget != "" {
			return fmt.Errorf("%w: expected .doco-cd.%s.y(a)ml or .doco-cd.y(a)ml at artifact root", ErrInvalidArtifactLayout, customTarget)
		}

		return fmt.Errorf("%w: expected .doco-cd.yml or .doco-cd.yaml at artifact root", ErrInvalidArtifactLayout)
	}

	layoutVersion, err := readLayoutVersionFromConfig(configFile)
	if err != nil {
		return fmt.Errorf("%w: failed to read layout from %s", ErrInvalidArtifactLayout, filepath.Base(configFile))
	}

	if layoutVersion != config.OciArtifactLayoutV1 {
		return fmt.Errorf("%w: expected layout=%s in %s", ErrInvalidArtifactLayout, config.OciArtifactLayoutV1, filepath.Base(configFile))
	}

	return nil
}

func findArtifactConfigFile(destination, customTarget string) (string, error) {
	var candidates []string

	if customTarget != "" {
		candidates = []string{
			fmt.Sprintf(".doco-cd.%s.yaml", customTarget),
			fmt.Sprintf(".doco-cd.%s.yml", customTarget),
		}
	} else {
		candidates = []string{".doco-cd.yaml", ".doco-cd.yml"}
	}

	for _, cfg := range candidates {
		cfgPath := filepath.Join(destination, cfg)
		if _, err := os.Stat(cfgPath); err == nil {
			return cfgPath, nil
		}
	}

	return "", os.ErrNotExist
}

func readLayoutVersionFromConfig(configFile string) (string, error) {
	b, err := os.ReadFile(configFile)
	if err != nil {
		return "", err
	}

	type layoutHeader struct {
		Version string `yaml:"version"`
	}

	dec := yaml.NewDecoder(bytes.NewReader(b))

	for {
		var header layoutHeader

		err = dec.Decode(&header)
		if errors.Is(err, io.EOF) {
			return config.OciArtifactLayoutV1, nil
		}

		if err != nil {
			return "", err
		}

		if strings.TrimSpace(header.Version) != "" {
			return strings.TrimSpace(header.Version), nil
		}
	}
}
