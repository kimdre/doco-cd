package docker

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/platforms"
	"github.com/moby/moby/api/types/image"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// composeNetworkHash returns the config hash that Compose stores in the
// com.docker.compose.config-hash label of a network.
// Compose keeps this function in an internal package, so doco-cd repeats it here.
// Compose cannot change the format: it would recreate the networks of all users.
func composeNetworkHash(o *types.NetworkConfig) (string, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}

	return digest.SHA256.FromBytes(raw).Encoded(), nil
}

// composeVolumeHash returns the config hash that Compose stores in the
// com.docker.compose.config-hash label of a volume.
// Compose uses the "local" driver when the volume sets no driver.
func composeVolumeHash(o types.VolumeConfig) (string, error) {
	if o.Driver == "" {
		o.Driver = "local"
	}

	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}

	return digest.SHA256.FromBytes(raw).Encoded(), nil
}

// serviceHashKeyOrder is the frozen top-level JSON key order of the service
// config hash. Compose hashes this layout, so a change recreates existing containers.
var serviceHashKeyOrder = []string{
	"profiles",
	"annotations",
	"attach",
	"build",
	"develop",
	"blkio_config",
	"cap_add",
	"cap_drop",
	"cgroup_parent",
	"cgroup",
	"cpu_count",
	"cpu_percent",
	"cpu_period",
	"cpu_quota",
	"cpu_rt_period",
	"cpu_rt_runtime",
	"cpus",
	"cpuset",
	"cpu_shares",
	"command",
	"configs",
	"container_name",
	"credential_spec",
	"depends_on",
	"deploy",
	"device_cgroup_rules",
	"devices",
	"dns",
	"dns_opt",
	"dns_search",
	"dockerfile",
	"domainname",
	"entrypoint",
	"provider",
	"environment",
	"env_file",
	"expose",
	"extends",
	"external_links",
	"extra_hosts",
	"group_add",
	"gpus",
	"hostname",
	"healthcheck",
	"image",
	"init",
	"ipc",
	"isolation",
	"labels",
	"label_file",
	"links",
	"logging",
	"log_driver",
	"log_opt",
	"mem_limit",
	"mem_reservation",
	"memswap_limit",
	"mem_swappiness",
	"mac_address",
	"models",
	"net",
	"network_mode",
	"networks",
	"oom_kill_disable",
	"oom_score_adj",
	"pid",
	"pids_limit",
	"platform",
	"ports",
	"privileged",
	"pull_policy",
	"read_only",
	"restart",
	"runtime",
	"scale",
	"secrets",
	"security_opt",
	"shm_size",
	"stdin_open",
	"stop_grace_period",
	"stop_signal",
	"storage_opt",
	"sysctls",
	"tmpfs",
	"tty",
	"ulimits",
	"use_api_socket",
	"user",
	"userns_mode",
	"uts",
	"volume_driver",
	"volumes",
	"volumes_from",
	"working_dir",
	"pre_start",
	"post_start",
	"pre_stop",
	"pull_refresh_after",
	"umask",
}

// trimServiceHashFields removes the attributes Compose excludes from the service config hash.
func trimServiceHashFields(o types.ServiceConfig) types.ServiceConfig {
	o.Build = nil
	o.PullPolicy = ""
	o.Scale = nil

	if o.Deploy != nil {
		deploy := *o.Deploy
		deploy.Replicas = nil
		o.Deploy = &deploy
	}

	o.DependsOn = nil
	o.Profiles = nil

	return o
}

// composeServiceHash returns the config hash Compose stores in the
// com.docker.compose.config-hash label of a container.
// Compose keeps ServiceHash in an internal package, so doco-cd repeats it here.
// The JSON layout is frozen because a change recreates existing containers.
func composeServiceHash(o types.ServiceConfig) (string, error) {
	raw, err := json.Marshal(trimServiceHashFields(o))
	if err != nil {
		return "", err
	}

	pinned, err := pinRootKeyOrder(raw, serviceHashKeyOrder)
	if err != nil {
		return "", err
	}

	return digest.SHA256.FromBytes(pinned).Encoded(), nil
}

// pinRootKeyOrder re-emits a JSON object with its top-level keys in order.
// Keys absent from order are appended in sorted order. Values stay byte-verbatim.
func pinRootKeyOrder(raw []byte, order []string) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	if err := buf.WriteByte('{'); err != nil {
		return nil, err
	}

	first := true

	write := func(key string, val json.RawMessage) error {
		if !first {
			if err := buf.WriteByte(','); err != nil {
				return err
			}
		}

		first = false

		name, err := json.Marshal(key)
		if err != nil {
			return err
		}

		if _, err = buf.Write(name); err != nil {
			return err
		}

		if err = buf.WriteByte(':'); err != nil {
			return err
		}

		if _, err = buf.Write(val); err != nil {
			return err
		}

		return nil
	}

	for _, key := range order {
		val, ok := root[key]
		if !ok {
			continue
		}

		if err := write(key, val); err != nil {
			return nil, err
		}

		delete(root, key)
	}

	for _, key := range slices.Sorted(maps.Keys(root)) {
		if err := write(key, root[key]); err != nil {
			return nil, err
		}
	}

	if err := buf.WriteByte('}'); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// composeImageContentDigest is the digest Compose records in com.docker.compose.image.
// mustRecreate compares that label separately from the config hash.
func composeImageContentDigest(inspect image.InspectResponse, platform string) (string, error) {
	var matcher platforms.Matcher = platforms.Default()

	if platform != "" {
		parsed, err := platforms.Parse(platform)
		if err != nil {
			return "", err
		}

		matcher = platforms.NewMatcher(parsed)
	}

	if len(inspect.Manifests) == 0 {
		return inspect.ID, nil
	}

	id, _ := matchComposeImageManifest(inspect, matcher)

	return id, nil
}

// matchComposeImageManifest selects the runnable manifest digest Compose uses
// for the com.docker.compose.image label.
func matchComposeImageManifest(inspect image.InspectResponse, platform platforms.Matcher) (string, bool) {
	var available []image.ManifestSummary

	for _, manifest := range inspect.Manifests {
		if manifest.Kind == image.ManifestKindImage && manifest.Available {
			available = append(available, manifest)
		}
	}

	for _, manifest := range available {
		if manifest.ImageData != nil && platform.Match(manifest.ImageData.Platform) {
			return manifest.ID, true
		}
	}

	if len(available) == 1 {
		manifest := available[0]
		if manifest.ImageData == nil {
			return manifest.ID, platform.Match(specs.Platform{
				Architecture: inspect.Architecture,
				OS:           inspect.Os,
				Variant:      inspect.Variant,
			})
		}

		return manifest.ID, false
	}

	return inspect.ID, false
}
