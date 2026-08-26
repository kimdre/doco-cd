package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	distreference "github.com/distribution/reference"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker/registryauth"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
)

var ErrNoSuchImage = errors.New("no such image") // Image does not exist

const (
	dockerHubDomain        = "docker.io"
	dockerHubIndexDomain   = "index.docker.io"
	dockerHubRegistryHost  = "registry-1.docker.io"
	dockerHubAuthConfigKey = "https://index.docker.io/v1/"
	dockerContentDigest    = "Docker-Content-Digest"
	wwwAuthenticateHeader  = "Www-Authenticate"
	registryAuthBearer     = "Bearer"
	// TestRemoteDockerImage is the Docker-in-Docker image version used in e2e tests.
	// Tracked by Renovate for automated updates .
	// Needs to be outside of test/ directory because Renovate ignores test/ by default.
	TestRemoteDockerImage = "docker:29-dind"
)

var (
	registryManifestAccepts = strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ",")
	registryDigestHTTPClient = http.DefaultClient
)

// registryAuthForImage returns a base64-encoded auth string for the registry
// that hosts the given image ref, sourced from the Docker config file.
// Returns an empty string on any error (unauthenticated access is attempted).
func registryAuthForImage(dockerCli command.Cli, imageRef string) string {
	encoded, err := command.RetrieveAuthTokenFromImage(dockerCli.ConfigFile(), imageRef)
	if err != nil {
		hint := registryauth.BuildFailureHint(dockerCli.ConfigFile(), []string{imageRef}, err)
		if hint != "" {
			slog.Warn("failed to resolve registry auth token", slog.String("ref", imageRef), slog.String("err", err.Error()), slog.String("hint", hint))
		} else {
			slog.Warn("failed to resolve registry auth token", slog.String("ref", imageRef), slog.String("err", err.Error()))
		}

		return ""
	}

	return encoded
}

// registryDigestForRef queries the registry for the current manifest digest of
// the given image reference without downloading any image layers.
func registryDigestForRef(ctx context.Context, dockerCli command.Cli, imageRef string) (string, error) {
	digest, err := registryDigestHeadLookup(ctx, dockerCli, imageRef)
	if err == nil {
		slog.Debug("registry HEAD digest lookup successful", slog.String("ref", imageRef), slog.String("digest", digest))
		return digest, nil
	}

	slog.Warn("registry HEAD digest lookup failed, falling back to distribution inspect", slog.String("ref", imageRef), slog.String("err", err.Error()))

	return registryDigestDistributionLookup(ctx, dockerCli, imageRef)
}

// registryDigestForRefViaDistributionInspect asks the Docker Engine API for
// the remote descriptor digest of the image reference.
func registryDigestForRefViaDistributionInspect(ctx context.Context, dockerCli command.Cli, imageRef string) (string, error) {
	info, err := dockerCli.Client().DistributionInspect(ctx, imageRef, client.DistributionInspectOptions{
		EncodedRegistryAuth: registryAuthForImage(dockerCli, imageRef),
	})
	if err != nil {
		return "", fmt.Errorf("registry inspect failed for %s: %w", imageRef, err)
	}

	return info.Descriptor.Digest.String(), nil
}

// registryDigestForRefViaHEAD queries the registry manifest endpoint directly
// using HEAD and returns Docker-Content-Digest when available.
func registryDigestForRefViaHEAD(ctx context.Context, dockerCli command.Cli, imageRef string) (string, error) {
	return registryDigestForRefViaHEADWithClient(ctx, dockerCli.ConfigFile(), imageRef, registryDigestHTTPClient)
}

func registryDigestForRefViaHEADWithClient(ctx context.Context, cfg *configfile.ConfigFile, imageRef string, httpClient *http.Client) (string, error) {
	manifestURL, authConfigKey, scope, err := registryManifestURL(imageRef)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", imageRef, err)
	}

	authConfig := registryAuthConfigForKey(cfg, authConfigKey)

	digest, err := registryManifestDigestHEAD(ctx, httpClient, manifestURL, scope, authConfig)
	if err != nil {
		return "", fmt.Errorf("registry HEAD inspect failed for %s: %w", imageRef, err)
	}

	return digest, nil
}

func registryManifestURL(imageRef string) (string, string, string, error) {
	namedRef, err := distreference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return "", "", "", err
	}

	domain := distreference.Domain(namedRef)
	registryHost := domain

	authConfigKey := domain
	if domain == dockerHubDomain || domain == dockerHubIndexDomain {
		registryHost = dockerHubRegistryHost
		authConfigKey = dockerHubAuthConfigKey
	}

	repositoryPath := distreference.Path(namedRef)

	manifestRef := ""
	if taggedRef, ok := distreference.TagNameOnly(namedRef).(distreference.NamedTagged); ok {
		manifestRef = taggedRef.Tag()
	}

	if canonicalRef, ok := namedRef.(distreference.Canonical); ok {
		manifestRef = canonicalRef.Digest().String()
	}

	if manifestRef == "" {
		return "", "", "", errors.New("manifest reference could not be resolved")
	}

	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registryHost, repositoryPath, url.PathEscape(manifestRef))
	scope := fmt.Sprintf("repository:%s:pull", repositoryPath)

	return manifestURL, authConfigKey, scope, nil
}

func registryAuthConfigForKey(cfg *configfile.ConfigFile, authConfigKey string) configtypes.AuthConfig {
	if cfg == nil {
		return configtypes.AuthConfig{}
	}

	authConfig, err := cfg.GetAuthConfig(authConfigKey)
	if err != nil {
		return configtypes.AuthConfig{}
	}

	return authConfig
}

func registryManifestDigestHEAD(ctx context.Context, httpClient *http.Client, manifestURL, scope string, authConfig configtypes.AuthConfig) (string, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := executeRegistryManifestHeadRequest(ctx, httpClient, manifestURL, authConfig, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		challenge, parseErr := parseBearerAuthChallenge(resp.Header.Get(wwwAuthenticateHeader))
		if parseErr != nil {
			return "", fmt.Errorf("registry unauthorized and challenge parse failed: %w", parseErr)
		}

		token, tokenErr := fetchRegistryBearerToken(ctx, httpClient, challenge, scope, authConfig)
		if tokenErr != nil {
			return "", tokenErr
		}

		retryResp, retryErr := executeRegistryManifestHeadRequest(ctx, httpClient, manifestURL, authConfig, token)
		if retryErr != nil {
			return "", retryErr
		}
		defer retryResp.Body.Close()

		return digestFromRegistryResponse(retryResp)
	}

	return digestFromRegistryResponse(resp)
}

func executeRegistryManifestHeadRequest(ctx context.Context, httpClient *http.Client, manifestURL string, authConfig configtypes.AuthConfig, bearerToken string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", registryManifestAccepts)

	switch {
	case bearerToken != "":
		req.Header.Set("Authorization", registryAuthBearer+" "+bearerToken)
	case authConfig.RegistryToken != "":
		req.Header.Set("Authorization", registryAuthBearer+" "+authConfig.RegistryToken)
	case authConfig.IdentityToken != "":
		req.Header.Set("Authorization", registryAuthBearer+" "+authConfig.IdentityToken)
	case authConfig.Username != "" || authConfig.Password != "":
		req.SetBasicAuth(authConfig.Username, authConfig.Password)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func digestFromRegistryResponse(resp *http.Response) (string, error) {
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("registry returned status %d", resp.StatusCode)
	}

	digest := strings.TrimSpace(resp.Header.Get(dockerContentDigest))
	if digest == "" {
		return "", fmt.Errorf("registry response missing %s header", dockerContentDigest)
	}

	return digest, nil
}

type bearerAuthChallenge struct {
	Realm   string
	Service string
	Scope   string
}

func parseBearerAuthChallenge(value string) (bearerAuthChallenge, error) {
	scheme, params, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, registryAuthBearer) {
		return bearerAuthChallenge{}, fmt.Errorf("unsupported challenge %q", value)
	}

	challenge := bearerAuthChallenge{}

	for pair := range strings.SplitSeq(params, ",") {
		key, rawVal, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found {
			continue
		}

		val := strings.Trim(strings.TrimSpace(rawVal), "\"")

		switch strings.ToLower(key) {
		case "realm":
			challenge.Realm = val
		case "service":
			challenge.Service = val
		case "scope":
			challenge.Scope = val
		}
	}

	if challenge.Realm == "" {
		return bearerAuthChallenge{}, errors.New("challenge missing realm")
	}

	return challenge, nil
}

func fetchRegistryBearerToken(ctx context.Context, httpClient *http.Client, challenge bearerAuthChallenge, fallbackScope string, authConfig configtypes.AuthConfig) (string, error) {
	tokenURL, err := url.Parse(challenge.Realm)
	if err != nil {
		return "", fmt.Errorf("invalid bearer realm %q: %w", challenge.Realm, err)
	}

	query := tokenURL.Query()
	if challenge.Service != "" {
		query.Set("service", challenge.Service)
	}

	scope := challenge.Scope
	if scope == "" {
		scope = fallbackScope
	}

	if scope != "" {
		query.Set("scope", scope)
	}

	tokenURL.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", err
	}

	switch {
	case authConfig.RegistryToken != "":
		req.Header.Set("Authorization", registryAuthBearer+" "+authConfig.RegistryToken)
	case authConfig.Username != "" || authConfig.Password != "":
		req.SetBasicAuth(authConfig.Username, authConfig.Password)
	case authConfig.IdentityToken != "":
		req.Header.Set("Authorization", registryAuthBearer+" "+authConfig.IdentityToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("token service returned status %d", resp.StatusCode)
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}

	if payload.Token != "" {
		return payload.Token, nil
	}

	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}

	return "", errors.New("token response missing token")
}

// digestFromReference extracts the digest part from an image reference in
// "name@digest" form and returns an empty string when no digest is present.
func digestFromReference(ref string) string {
	_, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ""
	}

	return digest
}

// digestFromRepoDigests returns the first digest found in RepoDigests.
// It returns an empty string when no digest entry can be parsed.
func digestFromRepoDigests(repoDigests []string) string {
	for _, repoDigest := range repoDigests {
		digest := digestFromReference(repoDigest)
		if digest != "" {
			return digest
		}
	}

	return ""
}

// getDeployedServiceImageDigests collects deployed service digests keyed by
// service name for the given project in both Swarm and non-Swarm modes.
func getDeployedServiceImageDigests(ctx context.Context, dockerCli command.Cli, swarmMode bool, projectName string, logger *slog.Logger) (map[string]string, error) {
	deployed := make(map[string]string)

	if swarmMode {
		services, err := swarmInternal.GetStackServices(ctx, dockerCli.Client(), projectName)
		if err != nil {
			return nil, fmt.Errorf("failed to list swarm services for %s: %w", projectName, err)
		}

		ns := convert.NewNamespace(projectName)
		for _, svc := range services {
			svcName := ns.Descope(svc.Spec.Name)

			digest := digestFromReference(svc.Spec.TaskTemplate.ContainerSpec.Image)
			if digest == "" {
				logger.Warn("deployed swarm service image has no digest", slog.String("service", svcName), slog.String("image", svc.Spec.TaskTemplate.ContainerSpec.Image))
				continue
			}

			deployed[svcName] = digest
		}

		return deployed, nil
	}

	containers, err := GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to list project containers for %s: %w", projectName, err)
	}

	selected := make(map[string]api.ContainerSummary)

	for _, cont := range containers {
		svcName := cont.Labels[api.ServiceLabel]
		if svcName == "" {
			continue
		}

		existing, ok := selected[svcName]
		if !ok || (existing.State != container.StateRunning && cont.State == container.StateRunning) {
			selected[svcName] = cont
		}
	}

	for svcName, cont := range selected {
		contInspect, err := dockerCli.Client().ContainerInspect(ctx, cont.ID, client.ContainerInspectOptions{})
		if err != nil {
			logger.Warn("failed to inspect deployed container", slog.String("service", svcName), slog.String("container_id", cont.ID), slog.String("err", err.Error()))
			continue
		}

		imageID := contInspect.Container.Image

		img, err := dockerCli.Client().ImageInspect(ctx, imageID)
		if err != nil {
			logger.Warn("failed to inspect deployed image", slog.String("service", svcName), slog.String("image_id", imageID), slog.String("err", err.Error()))
			continue
		}

		digest := digestFromRepoDigests(img.RepoDigests)
		if digest == "" {
			logger.Warn("deployed image has no digest", slog.String("service", svcName), slog.String("image_id", imageID))
			continue
		}

		deployed[svcName] = digest
	}

	return deployed, nil
}

// pruneImages tries to remove the specified image IDs from the Docker host and
// returns a list of pruned image IDs. Images still in use are silently skipped.
func pruneImages(ctx context.Context, dockerCli command.Cli, images []string) ([]string, error) {
	var prunedImages []string

	for _, img := range images {
		result, err := dockerCli.Client().ImageRemove(ctx, img, client.ImageRemoveOptions{
			Force:         true,
			PruneChildren: true,
		})
		if err != nil {
			switch {
			case strings.Contains(err.Error(), "image is being used by running container"):
				continue
			case strings.Contains(strings.ToLower(err.Error()), ErrNoSuchImage.Error()),
				strings.Contains(strings.ToLower(err.Error()), "not found"):
				continue
			default:
				return nil, fmt.Errorf("failed to remove image %s: %w", img, err)
			}
		}

		for _, r := range result.Items {
			switch {
			case r.Deleted != "":
				prunedImages = append(prunedImages, r.Deleted)
			case r.Untagged != "":
				prunedImages = append(prunedImages, r.Untagged)
			}
		}
	}

	return prunedImages, nil
}

// PullImages pulls all images defined in the named compose project.
func PullImages(ctx context.Context, dockerCli command.Cli, projectName string) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	containers, err := GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return fmt.Errorf("failed to get project containers: %w", err)
	}

	containerNames := make([]string, 0, len(containers))
	for _, c := range containers {
		containerNames = append(containerNames, c.Name)
	}

	project, err := service.Generate(ctx, api.GenerateOptions{ProjectName: projectName, Containers: containerNames})
	if err != nil {
		return fmt.Errorf("failed to generate project: %w", err)
	}

	err = service.Pull(ctx, project, api.PullOptions{Quiet: true})
	if err != nil {
		imageRefs := make([]string, 0, len(project.Services))
		for _, svc := range project.Services {
			if svc.Image == "" {
				continue
			}

			imageRefs = append(imageRefs, svc.Image)
		}

		hint := registryauth.BuildFailureHint(dockerCli.ConfigFile(), imageRefs, err)
		if hint != "" {
			return fmt.Errorf("failed to pull images: %w; %s", err, hint)
		}

		return fmt.Errorf("failed to pull images: %w", err)
	}

	return nil
}

// vars used to allow overriding in tests without needing to mock the entire function.
var (
	registryDigestLookup             = registryDigestForRef           // registryDigestLookup fetches registry digests for image refs, can be overridden in tests
	deployedServiceDigestLookup      = getDeployedServiceImageDigests // deployedServiceDigestLookup fetches deployed service image digests, can be overridden in tests
	deployedServiceRefLookup         = getDeployedServiceImageRefs    // deployedServiceRefLookup fetches deployed service image references, can be overridden in tests
	registryDigestHeadLookup         = registryDigestForRefViaHEAD
	registryDigestDistributionLookup = registryDigestForRefViaDistributionInspect
)

// DeployedServicesWithChangedImageDigests returns the sorted names of deployed
// services whose image digest differs from the registry digest of the configured image ref.
//
// This compares:
//  1. deployed service image digest (currently running/deployed)
//  2. registry digest of configured service image reference (DistributionInspect)
//
// A service without a deployed digest is treated as changed.
func DeployedServicesWithChangedImageDigests(ctx context.Context, dockerCli command.Cli, swarmMode bool, project *types.Project, logger *slog.Logger) ([]string, error) {
	// service name -> configured image ref
	configuredRefs := make(map[string]string)
	uniqueRefs := set.New[string]()

	for _, svc := range project.Services {
		// Scale-0 services never produce a container, so they have no deployed digest to compare.
		if svc.Image == "" || isScaledToZero(svc) {
			continue
		}

		configuredRefs[svc.Name] = svc.Image
		uniqueRefs.Add(svc.Image)
	}

	if len(configuredRefs) == 0 {
		return nil, nil
	}

	registryDigests := make(map[string]string, uniqueRefs.Len())
	for _, ref := range uniqueRefs.ToSlice() {
		digest, err := registryDigestLookup(ctx, dockerCli, ref)
		if err != nil {
			logger.Warn("could not fetch registry digest", slog.String("ref", ref), slog.String("err", err.Error()))
			continue
		}

		registryDigests[ref] = digest
	}

	deployedDigests, err := deployedServiceDigestLookup(ctx, dockerCli, swarmMode, project.Name, logger)
	if err != nil {
		return nil, err
	}

	var changedServices []string

	for serviceName, configuredRef := range configuredRefs {
		registryDigest, ok := registryDigests[configuredRef]
		if !ok {
			logger.Warn("registry digest unavailable, skipping service", slog.String("service", serviceName), slog.String("ref", configuredRef))
			continue
		}

		deployedDigest, ok := deployedDigests[serviceName]
		if !ok {
			logger.Debug("deployed service image digest unavailable, treating as changed", slog.String("service", serviceName), slog.String("ref", configuredRef))

			changedServices = append(changedServices, serviceName)

			continue
		}

		if deployedDigest != registryDigest {
			logger.Info("service image digest changed",
				slog.String("service", serviceName),
				slog.Group("image",
					slog.String("ref", configuredRef),
					slog.Group("digest",
						slog.String("deployed", deployedDigest),
						slog.String("registry", registryDigest),
					),
				),
			)

			changedServices = append(changedServices, serviceName)
		}
	}

	slices.Sort(changedServices)

	return changedServices, nil
}

// normalizeImageRef returns a comparable form of an image reference.
//
// Docker reports a deployed image in whatever form it was pulled while compose reports
// what the project file says, so `nginx` and `docker.io/library/nginx:latest` have to
// compare equal. A digest-pinned reference is already exact and is left alone; only a
// tag-less reference needs `:latest` filled in.
//
// An unparsable reference is returned untouched: comparing two raw strings is still
// better than dropping the comparison entirely.
func normalizeImageRef(ref string) string {
	if ref == "" {
		return ""
	}

	parsed, err := distreference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}

	if _, ok := parsed.(distreference.Canonical); ok {
		return parsed.String()
	}

	return distreference.TagNameOnly(parsed).String()
}

// refTag returns the tag of a reference, defaulting to `latest` when it carries none.
func refTag(named distreference.Named) string {
	if tagged, ok := named.(distreference.Tagged); ok {
		return tagged.Tag()
	}

	return "latest"
}

// imageRefsEqual reports whether a deployed image reference is the same image as the
// configured one.
//
// A literal comparison does not work. Swarm records the digest it resolved onto the
// reference it deployed (`nginx:1.27@sha256:...`) while a compose project normally
// configures a plain tag (`nginx:1.27`), so every tagged swarm service would be flagged
// on every deployment. The comparison therefore follows what the CONFIGURATION asks for:
//
//   - configured by tag: compare repository and tag, ignore any deployed digest
//   - configured by digest: compare repository and digest, ignore any tag
func imageRefsEqual(configured, deployed string) bool {
	confNamed, confErr := distreference.ParseNormalizedNamed(configured)
	deplNamed, deplErr := distreference.ParseNormalizedNamed(deployed)

	if confErr != nil || deplErr != nil {
		// One side is not a reference we can reason about. Comparing the raw strings is
		// still better than declaring them equal.
		return configured == deployed
	}

	if distreference.TrimNamed(confNamed).String() != distreference.TrimNamed(deplNamed).String() {
		return false
	}

	if confCanonical, pinned := confNamed.(distreference.Canonical); pinned {
		deplCanonical, ok := deplNamed.(distreference.Canonical)

		return ok && confCanonical.Digest() == deplCanonical.Digest()
	}

	return refTag(confNamed) == refTag(deplNamed)
}

// getDeployedServiceImageRefs returns a map of service name to the image reference of
// the container or swarm service currently deployed for it.
//
// Unlike getDeployedServiceImageDigests this needs no registry round trip and no image
// inspect, which is what makes it cheap enough to run on every deployment instead of
// only under force_image_pull.
func getDeployedServiceImageRefs(ctx context.Context, dockerCli command.Cli, swarmMode bool, projectName string, logger *slog.Logger) (map[string]string, error) {
	deployed := make(map[string]string)

	if swarmMode {
		services, err := swarmInternal.GetStackServices(ctx, dockerCli.Client(), projectName)
		if err != nil {
			return nil, fmt.Errorf("failed to list swarm services for %s: %w", projectName, err)
		}

		ns := convert.NewNamespace(projectName)
		for _, svc := range services {
			deployed[ns.Descope(svc.Spec.Name)] = svc.Spec.TaskTemplate.ContainerSpec.Image
		}

		logger.Debug("resolved deployed service image references", slog.String("project", projectName), slog.Int("services", len(deployed)))

		return deployed, nil
	}

	containers, err := GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to list project containers for %s: %w", projectName, err)
	}

	selected := make(map[string]api.ContainerSummary)

	for _, cont := range containers {
		svcName := cont.Labels[api.ServiceLabel]
		if svcName == "" {
			continue
		}

		existing, ok := selected[svcName]
		if !ok || (existing.State != container.StateRunning && cont.State == container.StateRunning) {
			selected[svcName] = cont
		}
	}

	for svcName, cont := range selected {
		deployed[svcName] = cont.Image
	}

	logger.Debug("resolved deployed service image references", slog.String("project", projectName), slog.Int("services", len(deployed)))

	return deployed, nil
}

// DeployedServicesWithChangedImageRefs returns the sorted names of services whose
// configured image reference differs from the one currently deployed.
//
// This is the tag-level counterpart of DeployedServicesWithChangedImageDigests. It
// answers "which services does this deployment move" for the ordinary case of a tag
// bump in the deploy config, where the digest comparison never runs because
// force_image_pull is off. It is local only, so it costs no registry lookups.
//
// It deliberately under-reports rather than guess, because it feeds a notification and
// a wrong service name is worse than a missing one:
//   - nothing deployed yet, i.e. the first deployment of this stack, yields no services;
//     the stack level notification already says the whole stack came up
//   - a service whose deployed reference cannot be determined is skipped
//
// A service that is configured but has no deployed counterpart while other services do
// IS reported: it is genuinely arriving with this deployment.
func DeployedServicesWithChangedImageRefs(ctx context.Context, dockerCli command.Cli, swarmMode bool, project *types.Project, logger *slog.Logger) ([]string, error) {
	if project == nil {
		return nil, nil
	}

	deployedRefs, err := deployedServiceRefLookup(ctx, dockerCli, swarmMode, project.Name, logger)
	if err != nil {
		return nil, err
	}

	if len(deployedRefs) == 0 {
		return nil, nil
	}

	var changedServices []string

	for _, svc := range project.Services {
		// Scale-0 services never produce a container, so they have nothing to compare.
		if svc.Image == "" || isScaledToZero(svc) {
			continue
		}

		configuredRef := normalizeImageRef(svc.Image)

		deployedRaw, ok := deployedRefs[svc.Name]
		if !ok {
			logger.Debug("service is not deployed yet, treating as changed", slog.String("service", svc.Name), slog.String("ref", svc.Image))

			changedServices = append(changedServices, svc.Name)

			continue
		}

		if deployedRaw == "" {
			logger.Debug("deployed image reference unavailable, skipping service", slog.String("service", svc.Name), slog.String("ref", svc.Image))

			continue
		}

		if !imageRefsEqual(svc.Image, deployedRaw) {
			logger.Info("service image reference changed",
				slog.String("service", svc.Name),
				slog.Group("image",
					slog.String("deployed", normalizeImageRef(deployedRaw)),
					slog.String("configured", configuredRef),
				),
			)

			changedServices = append(changedServices, svc.Name)
		}
	}

	slices.Sort(changedServices)

	return changedServices, nil
}
