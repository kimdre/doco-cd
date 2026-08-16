package docker

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

// certRefPrefixes identifies external secret references that resolve to a certificate. Only
// openbao's "pki-role:" prefix supports automatic reissuance/rotation; the read-only "pki:"
// prefix is recorded for visibility but is never auto-rotated.
const (
	pkiReadOnlyRefPrefix = "pki:"
	pkiRoleRefPrefix     = "pki-role:"
)

// pkiRoleKeySuffix is appended to a pki-role external secret's env var name for its private key
// companion entry. This mirrors openbao.PKIRoleKeySuffix; it's duplicated here (instead of
// importing the openbao package) to keep internal/docker provider-agnostic.
const pkiRoleKeySuffix = "_KEY"

// rotatableCertValues returns the resolved values (certificate PEM and, where present, its
// private key PEM) of every pki-role-backed external secret in deployConfig, keyed by their env
// var name. These are exactly the values that change whenever RotateProjectCertificates reissues
// certificates through the secret provider.
func rotatableCertValues(deployConfig *deploy.Config) map[string]string {
	if deployConfig == nil || len(deployConfig.ExternalSecrets) == 0 {
		return nil
	}

	values := make(map[string]string)

	for envVar, ref := range deployConfig.ExternalSecrets {
		if !strings.HasPrefix(ref.LegacyRef, pkiRoleRefPrefix) {
			continue
		}

		for _, name := range [2]string{envVar, envVar + pkiRoleKeySuffix} {
			if v, ok := deployConfig.Internal.Environment[name]; ok && v != "" {
				values[name] = v
			}
		}
	}

	return values
}

// certificateValues returns all resolved certificate values, including private keys issued by
// pki-role references. It is used to identify the services which should carry cert-rotation
// labels, including deployments containing a read-only pki reference.
func certificateValues(deployConfig *deploy.Config) map[string]string {
	if deployConfig == nil || len(deployConfig.ExternalSecrets) == 0 {
		return nil
	}

	values := make(map[string]string)

	for envVar, ref := range deployConfig.ExternalSecrets {
		isPKI := strings.HasPrefix(ref.LegacyRef, pkiReadOnlyRefPrefix)

		isPKIRole := strings.HasPrefix(ref.LegacyRef, pkiRoleRefPrefix)
		if !isPKI && !isPKIRole {
			continue
		}

		if v, ok := deployConfig.Internal.Environment[envVar]; ok && v != "" {
			values[envVar] = v
		}

		if isPKIRole {
			if v, ok := deployConfig.Internal.Environment[envVar+pkiRoleKeySuffix]; ok && v != "" {
				values[envVar+pkiRoleKeySuffix] = v
			}
		}
	}

	return values
}

// certRotationLabelValues computes the cd.doco.deployment.cert.expiry and
// cd.doco.deployment.cert.rotatable label values for a deployment, based on its external secret
// references and their resolved values.
//
// It returns ok=false when the deployment has no cert-bearing external secrets, in which case no
// cert labels should be added.
func certRotationLabelValues(deployConfig *deploy.Config) (expiry, rotatable string, ok bool) {
	if deployConfig == nil || len(deployConfig.ExternalSecrets) == 0 {
		return "", "", false
	}

	var (
		earliest       time.Time
		hasCert        bool
		allRotatable   = true
		resolvedValues = deployConfig.Internal.Environment
	)

	for envVar, ref := range deployConfig.ExternalSecrets {
		legacyRef := ref.LegacyRef

		isPKI := strings.HasPrefix(legacyRef, pkiReadOnlyRefPrefix)
		isPKIRole := strings.HasPrefix(legacyRef, pkiRoleRefPrefix)

		if !isPKI && !isPKIRole {
			continue
		}

		value, exists := resolvedValues[envVar]
		if !exists || value == "" {
			continue
		}

		notAfter, parseErr := certificateNotAfter(value)
		if parseErr != nil {
			continue
		}

		hasCert = true

		if !isPKIRole {
			allRotatable = false
		}

		if earliest.IsZero() || notAfter.Before(earliest) {
			earliest = notAfter
		}
	}

	if !hasCert {
		return "", "", false
	}

	return earliest.UTC().Format(time.RFC3339), strconv.FormatBool(allRotatable), true
}

// servicesUsingRotatableCerts returns the names of services in project that reference any
// pki-role-backed certificate or private key value from deployConfig, either directly (the value
// appears in the service's own environment) or indirectly via a top-level config or secret whose
// resolved content matches one of those values.
//
// It's used to scope RotateProjectCertificates' redeploy to only the services actually affected
// by the rotation, instead of recreating every service in the project. Returns nil when
// deployConfig has no pki-role-backed secrets.
func servicesUsingRotatableCerts(project *types.Project, deployConfig *deploy.Config) []string {
	values := rotatableCertValues(deployConfig)
	if len(values) == 0 || project == nil {
		return nil
	}

	valueSet := make(map[string]struct{}, len(values))
	for _, v := range values {
		valueSet[v] = struct{}{}
	}

	var names []string

	for name, s := range project.Services {
		if serviceUsesAnyValue(s, project, valueSet) {
			names = append(names, name)
		}
	}

	return names
}

// serviceUsesAnyValue reports whether service s consumes any value in valueSet, either directly
// through its own environment, or indirectly through a top-level config or secret (by name,
// resolved against project) that it references.
func serviceUsesAnyValue(s types.ServiceConfig, project *types.Project, valueSet map[string]struct{}) bool {
	for _, v := range s.Environment {
		if v != nil {
			if _, used := valueSet[*v]; used {
				return true
			}
		}
	}

	for _, ref := range s.Configs {
		if cfg, ok := project.Configs[ref.Source]; ok {
			if _, used := valueSet[cfg.Content]; used {
				return true
			}
		}
	}

	for _, ref := range s.Secrets {
		if secret, ok := project.Secrets[ref.Source]; ok {
			if _, used := valueSet[secret.Content]; used {
				return true
			}
		}
	}

	return false
}

// certificateNotAfter parses value as a PEM-encoded X.509 certificate and returns its NotAfter
// (expiry) time.
func certificateNotAfter(value string) (time.Time, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" {
		return time.Time{}, errNotACertificate
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, err
	}

	return cert.NotAfter, nil
}

// applyCertRotationLabels adds cert expiry/rotatable labels to the provided label map when the
// deployment has any cert-bearing external secrets.
func applyCertRotationLabels(labels map[string]string, deployConfig *deploy.Config) {
	expiry, rotatable, ok := certRotationLabelValues(deployConfig)
	if !ok {
		return
	}

	labels[DocoCDLabels.Deployment.CertExpiry] = expiry
	labels[DocoCDLabels.Deployment.CertRotatable] = rotatable
}

// applyCertRotationLabelsToService adds cert rotation labels only when service consumes a
// certificate-related external secret. Labeling unrelated services would leave stale labels behind
// when a rotation recreates only the affected services.
func applyCertRotationLabelsToService(labels map[string]string, service types.ServiceConfig, project *types.Project, deployConfig *deploy.Config) {
	values := certificateValues(deployConfig)
	if len(values) == 0 {
		return
	}

	valueSet := make(map[string]struct{}, len(values))
	for _, value := range values {
		valueSet[value] = struct{}{}
	}

	if serviceUsesAnyValue(service, project, valueSet) {
		applyCertRotationLabels(labels, deployConfig)
	}
}

// errNotACertificate is returned by certificateNotAfter when value does not decode to a PEM
// CERTIFICATE block.
var errNotACertificate = errors.New("value is not a PEM encoded certificate")
