package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	openbao "github.com/openbao/openbao/api/v2"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "openbao"
)

const (
	PKIRefFormat     = `^pki:(?:[^:]+:)?[^:]+:[^:]+$`            // #nosec G101 pki:<namespace(optional)>:<secretEngine>:<commonName>
	PKIRoleRefFormat = `^pki-role:(?:[^:]+:)?[^:]+:[^:]+:[^:]+$` // #nosec G101 pki-role:<namespace(optional)>:<secretEngine>:<role>:<commonName>
	SecretRefFormat  = `^kv:(?:[^:]+:)?[^:]+:[^:]+:[^:]+$`       // #nosec G101 kv:<namespace(optional)>:<secretEngine>:<secretName>:<key>
)

// PKIRoleKeySuffix is appended to the env var name of a pki-role external secret reference to
// expose the matching private key issued alongside the certificate (e.g. CERT -> CERT_KEY).
const PKIRoleKeySuffix = secrettypes.PKIRoleKeySuffix

// pkiRoleRefRegexp is a precompiled matcher for PKIRoleRefFormat, used where matching happens in a
// loop (e.g. ResolveSecretReferences) to avoid recompiling the pattern on every call.
var pkiRoleRefRegexp = regexp.MustCompile(PKIRoleRefFormat)

var ErrInvalidSecretReference = errors.New("invalid secret reference")

type Provider struct {
	Client *openbao.Client
}

type deployedCertState struct {
	Ref    string `json:"ref"`
	Serial string `json:"serial"`
}

// Name returns the name of the secret provider.
func (p *Provider) Name() string {
	return Name
}

// NewProvider creates a new Provider instance for OpenBao and performs login using the provided address and access token.
func NewProvider(_ context.Context, address, token string) (*Provider, error) {
	config := openbao.DefaultConfig()

	config.Address = address

	client, err := openbao.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize OpenBao client: %w", err)
	}

	client.SetToken(token)

	provider := &Provider{Client: client}

	return provider, nil
}

// GetSecret retrieves a secret value from the Secrets Manager using the provided secret reference.
func (p *Provider) GetSecret(ctx context.Context, ref string) (string, error) {
	namespace, engineType, engineName, id, key, err := parseReference(ref)
	if err != nil {
		return "", err
	}

	c := p.Client.WithNamespace(namespace)

	var strValue string

	switch engineType {
	case "pki":
		serial, err := GetCertSerial(ctx, c, engineName, id)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve certificate serial for common name %s: %w", id, err)
		}

		strValue, err = GetCert(ctx, c, engineName, serial)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve certificate with serial %s: %w", id, err)
		}

	case "pki-role":
		issued, err := IssueCert(ctx, c, engineName, key, id)
		if err != nil {
			return "", fmt.Errorf("failed to issue certificate for common name %s using role %s: %w", id, key, err)
		}

		strValue = issued.Certificate

	case "kv":
		strValue, err = GetSecret(ctx, c, engineName, id, key)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve secret with id %s: %w", id, err)
		}
	default:
		return "", fmt.Errorf("%w: unknown secret engine type %s", ErrInvalidSecretReference, engineType)
	}

	return strValue, nil
}

// GetSecrets retrieves multiple secrets from Secrets Manager using the provided list of secret references.
func (p *Provider) GetSecrets(ctx context.Context, refs []string) (map[string]string, error) {
	resolvedSecrets := make(map[string]string)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)

	for _, ref := range refs {
		wg.Add(1)

		go func(secretName string) {
			defer wg.Done()

			v, err := p.GetSecret(ctx, secretName)
			if err != nil {
				select {
				case errCh <- fmt.Errorf("resolve secret reference %q: %w", secretName, err):
					cancel()
				default:
				}

				return
			}

			mu.Lock()

			resolvedSecrets[secretName] = v

			mu.Unlock()
		}(ref)
	}

	wg.Wait()
	close(errCh)

	if err, ok := <-errCh; ok {
		return nil, err
	}

	return resolvedSecrets, nil
}

// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
// by fetching the corresponding secret values from the secret provider.
//
// A pki-role reference (see PKIRoleRefFormat) issues a fresh certificate and its matching private
// key, and expands into two output entries: envVar holds the certificate PEM, and envVar + PKIRoleKeySuffix
// (e.g. CERT_KEY) holds the private key PEM.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	plainSecrets := make(map[string]string, len(secrets))
	pkiRoleSecrets := make(map[string]string, len(secrets))

	for envVar, ref := range secrets {
		if pkiRoleRefRegexp.MatchString(ref) {
			pkiRoleSecrets[envVar] = ref
			continue
		}

		plainSecrets[envVar] = ref
	}

	for envVar := range pkiRoleSecrets {
		if _, exists := secrets[envVar+PKIRoleKeySuffix]; exists {
			return nil, fmt.Errorf("external secret %q conflicts with the private key generated for pki-role secret %q", envVar+PKIRoleKeySuffix, envVar)
		}
	}

	out := make(map[string]string)

	if len(plainSecrets) > 0 {
		refs := make([]string, 0, len(plainSecrets))
		for _, ref := range plainSecrets {
			refs = append(refs, ref)
		}

		resolved, err := p.GetSecrets(ctx, refs)
		if err != nil {
			return nil, err
		}

		for envVar, ref := range plainSecrets {
			if val, ok := resolved[ref]; ok {
				out[envVar] = val
			} else {
				out[envVar] = ""
			}
		}
	}

	if len(pkiRoleSecrets) > 0 {
		issued, err := p.issuePKIRoleCerts(ctx, pkiRoleSecrets)
		if err != nil {
			return nil, err
		}

		for envVar, cert := range issued {
			out[envVar] = cert.Certificate
			out[envVar+PKIRoleKeySuffix] = cert.PrivateKey
		}
	}

	return out, nil
}

// DeploymentHasRevokedCertificate reports whether any currently deployed pki-role certificate
// described by certState has already been revoked in OpenBao.
func (p *Provider) DeploymentHasRevokedCertificate(ctx context.Context, certState string) (bool, error) {
	if strings.TrimSpace(certState) == "" {
		return false, nil
	}

	var deployed []deployedCertState
	if err := json.Unmarshal([]byte(certState), &deployed); err != nil {
		return false, fmt.Errorf("parse deployed certificate state: %w", err)
	}

	type mountKey struct {
		namespace string
		engine    string
	}

	revokedByMount := make(map[mountKey]map[string]struct{}, len(deployed))

	for _, cert := range deployed {
		namespace, engineType, engineName, _, _, err := parseReference(cert.Ref)
		if err != nil {
			return false, fmt.Errorf("parse deployed certificate ref %q: %w", cert.Ref, err)
		}

		if engineType != "pki-role" {
			continue
		}

		key := mountKey{namespace: namespace, engine: engineName}

		if _, ok := revokedByMount[key]; !ok {
			serials, err := ListRevokedCertSerials(ctx, p.Client.WithNamespace(namespace), engineName)
			if err != nil {
				return false, fmt.Errorf("list revoked certificates for %s/%s: %w", namespace, engineName, err)
			}

			revoked := make(map[string]struct{}, len(serials))
			for _, serial := range serials {
				revoked[normalizeCertSerial(serial)] = struct{}{}
			}

			revokedByMount[key] = revoked
		}

		if _, revoked := revokedByMount[key][normalizeCertSerial(cert.Serial)]; revoked {
			return true, nil
		}
	}

	return false, nil
}

// issuePKIRoleCerts issues a fresh certificate/key pair for each pki-role reference in refs,
// keyed by the same env var name used in the input map.
func (p *Provider) issuePKIRoleCerts(ctx context.Context, refs map[string]string) (map[string]IssuedCertificate, error) {
	issued := make(map[string]IssuedCertificate, len(refs))

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)

	for envVar, ref := range refs {
		wg.Add(1)

		go func(envVar, ref string) {
			defer wg.Done()

			namespace, _, engineName, commonName, roleName, err := parseReference(ref)
			if err != nil {
				select {
				case errCh <- err:
					cancel()
				default:
				}

				return
			}

			c := p.Client.WithNamespace(namespace)

			cert, err := IssueCert(ctx, c, engineName, roleName, commonName)
			if err != nil {
				select {
				case errCh <- fmt.Errorf("failed to issue certificate for common name %s: %w", commonName, err):
					cancel()
				default:
				}

				return
			}

			mu.Lock()
			issued[envVar] = cert
			mu.Unlock()
		}(envVar, ref)
	}

	wg.Wait()
	close(errCh)

	if err, ok := <-errCh; ok {
		return nil, err
	}

	return issued, nil
}

// Close cleans up resources used by the Provider.
func (p *Provider) Close() {}

// parseReference parses the reference string into its components: engineType, engineName, id, and key.
func parseReference(ref string) (namespace, engineType, engineName, id, key string, err error) {
	const defaultNamespace = "root"

	matchedPKI, _ := regexp.MatchString(PKIRefFormat, ref)
	matchedPKIRole, _ := regexp.MatchString(PKIRoleRefFormat, ref)
	matchedSecret, _ := regexp.MatchString(SecretRefFormat, ref)

	// Check if reference is in the correct format
	if !matchedPKI && !matchedPKIRole && !matchedSecret {
		return "", "", "", "", "", fmt.Errorf("%w: unexpected ref format %q", ErrInvalidSecretReference, ref)
	}

	// Handle PKI reference
	if matchedPKI {
		parts := strings.Split(ref, ":")
		if len(parts) == 3 {
			// pki:<engineType>:<commonName>
			return defaultNamespace, parts[0], parts[1], parts[2], "", nil
		} else if len(parts) == 4 {
			// pki:<namespace>:<engineType>:<commonName>
			return parts[1], parts[0], parts[2], parts[3], "", nil
		}

		return "", "", "", "", "", fmt.Errorf("%w: expected format 'pki:<namespace(optional)>:<secretEngine>:<commonName>', got %q", ErrInvalidSecretReference, ref)
	}

	// Handle PKI role (issuance) reference
	if matchedPKIRole {
		parts := strings.Split(ref, ":")
		if len(parts) == 4 {
			// pki-role:<engineType>:<role>:<commonName>
			return defaultNamespace, parts[0], parts[1], parts[3], parts[2], nil
		} else if len(parts) == 5 {
			// pki-role:<namespace>:<engineType>:<role>:<commonName>
			return parts[1], parts[0], parts[2], parts[4], parts[3], nil
		}

		return "", "", "", "", "", fmt.Errorf("%w: expected format 'pki-role:<namespace(optional)>:<secretEngine>:<role>:<commonName>', got %q", ErrInvalidSecretReference, ref)
	}

	// Handle Secret reference
	parts := strings.Split(ref, ":")
	if len(parts) == 4 {
		// kv:<engineType>:<secretName>:<key>
		return defaultNamespace, parts[0], parts[1], parts[2], parts[3], nil
	} else if len(parts) == 5 {
		// kv:<namespace>:<engineType>:<secretName>:<key>
		return parts[1], parts[0], parts[2], parts[3], parts[4], nil
	}

	return "", "", "", "", "", fmt.Errorf("%w: expected format 'kv:<namespace(optional)>:<secretEngine>:<secretName>:<key>', got %q", ErrInvalidSecretReference, ref)
}
