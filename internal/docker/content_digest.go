package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// contentDigestDomain separates the content digest from other SHA-256 values.
// A change of this value changes the digest of every service, which recreates the services.
const contentDigestDomain = "doco-cd/content-digest/v1"

const (
	contentDigestKindSecret = "secret"
	contentDigestKindConfig = "config"
)

// PKIRoleNormMap builds a map from resolved pki-role cert and private-key PEM
// values to stable placeholder strings (the ref string itself), for use with
// WithNormalizedEnvValues and ApplyContentDigestLabels.
//
// Certs issued by OpenBao pki-role refs are always freshly generated on every
// ResolveSecretReferences call, so their PEM bytes differ each time even when no
// configuration has changed. Using the ref string as a stable placeholder prevents
// the project hash from changing on every poll cycle.
func PKIRoleNormMap(externalSecrets map[string]secrettypes.ExternalSecretRef, env map[string]string) map[string]string {
	return pkiRoleValueReferences(pkiRoleEnvReferences(externalSecrets), env)
}

func pkiRoleEnvReferences(externalSecrets map[string]secrettypes.ExternalSecretRef) map[string]string {
	refs := make(map[string]string)

	for envVar, ref := range externalSecrets {
		if !strings.HasPrefix(ref.LegacyRef, secrettypes.PKIRoleRefPrefix) {
			continue
		}

		refs[envVar] = ref.LegacyRef
		refs[envVar+secrettypes.PKIRoleKeySuffix] = ref.LegacyRef + secrettypes.PKIRoleKeySuffix
		refs[envVar+secrettypes.PKIFullChainSuffix] = ref.LegacyRef + secrettypes.PKIFullChainSuffix
	}

	return refs
}

func pkiRoleValueReferences(refs, env map[string]string) map[string]string {
	norm := make(map[string]string)

	// When values match, always select the same reference.
	for _, name := range slices.Sorted(maps.Keys(refs)) {
		if value := env[name]; value != "" {
			norm[value] = refs[name]
		}
	}

	return norm
}

// ApplyContentDigestLabels sets the content digest label on each Compose service that
// consumes a top-level secret or config with an `environment` or `content` source.
//
// Compose copies these values only when it creates the container. The label
// lets doco-cd and Compose detect changes to the values in their hashes.
//
// File-based and external objects are not included. The change detection for files
// handles file-based objects. Objects that recreate.ignore matches are not included,
// so their value changes do not recreate the service.
// Resolved pki-role values use a stable reference in the digest.
//
// Services without such objects lose the label.
func ApplyContentDigestLabels(project *types.Project, externalSecrets map[string]secrettypes.ExternalSecretRef) error {
	if project == nil {
		return nil
	}

	// Read the recreate.ignore config only when it can change the result.
	// Projects without such objects then do not fail on an invalid ignore label here.
	var ignore projectIgnoreCfg

	if projectHasContentObjects(project) {
		var err error

		ignore, err = getIgnoreRecreateCfgFromProject(project)
		if err != nil {
			return fmt.Errorf("failed to read recreate ignore config: %w", err)
		}
	}

	label := DocoCDLabels.Deployment.ContentDigest
	pkiEnvRefs := pkiRoleEnvReferences(externalSecrets)
	pkiValueRefs := pkiRoleValueReferences(pkiEnvRefs, project.Environment)

	for name, svc := range project.Services {
		entries := make([]string, 0, len(svc.Secrets)+len(svc.Configs))

		for _, ref := range svc.Secrets {
			obj, ok := project.Secrets[ref.Source]
			if !ok || checkIsIgnoreByCfg(ignore, name, changeScopeSecrets, ref.Source) {
				continue
			}

			if value, ok := contentObjectValue(project, types.FileObjectConfig(obj), pkiEnvRefs, pkiValueRefs); ok {
				entries = append(entries, contentDigestEntry(contentDigestKindSecret, ref.Source, value))
			}
		}

		for _, ref := range svc.Configs {
			obj, ok := project.Configs[ref.Source]
			if !ok || checkIsIgnoreByCfg(ignore, name, changeScopeConfigs, ref.Source) {
				continue
			}

			if value, ok := contentObjectValue(project, types.FileObjectConfig(obj), pkiEnvRefs, pkiValueRefs); ok {
				entries = append(entries, contentDigestEntry(contentDigestKindConfig, ref.Source, value))
			}
		}

		current, hasLabel := svc.Labels[label]

		if len(entries) == 0 {
			if !hasLabel {
				continue
			}

			svc.Labels = maps.Clone(svc.Labels)
			delete(svc.Labels, label)
			project.Services[name] = svc

			continue
		}

		digest := contentDigest(entries)
		if hasLabel && current == digest {
			continue
		}

		svc.Labels = maps.Clone(svc.Labels)
		if svc.Labels == nil {
			svc.Labels = types.Labels{}
		}

		svc.Labels[label] = digest
		project.Services[name] = svc
	}

	return nil
}

// projectHasContentObjects reports whether the project defines a top-level secret or
// config with an `environment` or `content` source.
func projectHasContentObjects(project *types.Project) bool {
	hasContent := func(obj types.FileObjectConfig) bool {
		return !bool(obj.External) && (obj.Content != "" || obj.Environment != "")
	}

	for _, obj := range project.Secrets {
		if hasContent(types.FileObjectConfig(obj)) {
			return true
		}
	}

	for _, obj := range project.Configs {
		if hasContent(types.FileObjectConfig(obj)) {
			return true
		}
	}

	return false
}

// contentObjectValue returns the value that Compose copies, with PKI values replaced by references.
func contentObjectValue(project *types.Project, obj types.FileObjectConfig, pkiEnvRefs, pkiValueRefs map[string]string) (string, bool) {
	if obj.External || (obj.Content == "" && obj.Environment == "") {
		return "", false
	}

	// Compose prefers Content. An included file can resolve it from its own environment.
	value := obj.Content
	if obj.Environment != "" {
		envValue, found := project.Environment[obj.Environment]
		if value == "" {
			if !found {
				// An unset variable must not hash like an empty file. Compose reports
				// the missing variable if it creates the container.
				return "", false
			}

			value = envValue
		}

		// The leaf and full chain can have the same bytes. Use the source name
		// to keep the digest stable before and after certificate issuance.
		if ref, ok := pkiEnvRefs[obj.Environment]; ok && value != "" && value == envValue {
			return ref, true
		}
	}

	if ref, ok := pkiValueRefs[value]; ok {
		return ref, true
	}

	return value, true
}

// contentDigestEntry identifies the object and hashes its value.
func contentDigestEntry(kind, name, value string) string {
	sum := sha256.Sum256([]byte(value))

	return kind + "\x00" + name + "\x00" + hex.EncodeToString(sum[:])
}

// contentDigest returns the SHA-256 digest of the entries in a stable order.
func contentDigest(entries []string) string {
	slices.Sort(entries)
	entries = slices.Compact(entries)

	h := sha256.New()
	h.Write([]byte(contentDigestDomain))

	for _, entry := range entries {
		h.Write([]byte{'\n'})
		h.Write([]byte(entry))
	}

	return hex.EncodeToString(h.Sum(nil))
}
