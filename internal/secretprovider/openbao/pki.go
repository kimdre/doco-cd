package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openbao/openbao/api/v2"
)

// IssuedCertificate represents a certificate freshly issued by an OpenBao PKI role,
// including its matching private key, its issuing CA chain and expiration time.
type IssuedCertificate struct {
	Certificate string
	CAChain     []string
	PrivateKey  string
	Expiration  time.Time
}

// FullChain returns the leaf certificate followed by its issuing CA chain as a single PEM bundle.
// When no CA chain was returned by OpenBao, it is equivalent to Certificate.
func (c IssuedCertificate) FullChain() string {
	return joinPEMChain(c.Certificate, c.CAChain)
}

// joinPEMChain concatenates a leaf certificate with its issuing CA chain into a single PEM bundle
// (commonly called a fullchain), skipping empty and duplicate entries.
func joinPEMChain(leaf string, chain []string) string {
	parts := make([]string, 0, len(chain)+1)
	seen := make(map[string]struct{}, len(chain)+1)

	for _, cert := range append([]string{leaf}, chain...) {
		cert = strings.TrimSpace(cert)
		if cert == "" {
			continue
		}

		if _, exists := seen[cert]; exists {
			continue
		}

		seen[cert] = struct{}{}

		parts = append(parts, cert)
	}

	return strings.Join(parts, "\n")
}

// parseCAChain extracts the issuing CA chain from a PKI response payload. OpenBao returns the
// chain as ca_chain; older mounts and some endpoints only expose the single issuing CA, which is
// then used as a one-element chain.
func parseCAChain(data map[string]any) []string {
	switch chain := data["ca_chain"].(type) {
	case []string:
		if len(chain) > 0 {
			return chain
		}
	case []any:
		certs := make([]string, 0, len(chain))

		for _, entry := range chain {
			cert, ok := entry.(string)
			if !ok {
				continue
			}

			certs = append(certs, cert)
		}

		if len(certs) > 0 {
			return certs
		}
	}

	if issuingCA, ok := data["issuing_ca"].(string); ok && strings.TrimSpace(issuingCA) != "" {
		return []string{issuingCA}
	}

	return nil
}

// GetCertSerial retrieves the serial number of a certificate from the PKI engine in OpenBao using the provided engine name and common name.
func GetCertSerial(ctx context.Context, client *api.Client, engineName, commonName string) (string, error) {
	pathToList := engineName + "/certs/detailed"

	response, err := client.Logical().ListWithContext(ctx, pathToList)
	if err != nil {
		return "", fmt.Errorf("unable to list certificates from OpenBao: %w", err)
	}

	if response == nil || response.Data == nil {
		return "", errors.New("no data found when listing certificates")
	}

	for serial, certInfoRaw := range response.Data["key_info"].(map[string]any) {
		certInfo, ok := certInfoRaw.(map[string]any)
		if !ok {
			continue
		}

		if certInfo["common_name"] == commonName {
			return serial, nil
		}
	}

	return "", fmt.Errorf("certificate with common name %s not found", commonName)
}

// GetCert retrieves a certificate from the PKI engine in OpenBao using the provided engine name and serial number.
func GetCert(ctx context.Context, client *api.Client, engineName, serial string) (string, error) {
	cert, _, err := readCert(ctx, client, engineName, serial)

	return cert, err
}

// GetCertWithFullChain retrieves a certificate from the PKI engine in OpenBao and returns both the
// leaf certificate on its own and the PEM bundle of the leaf followed by its issuing CA chain.
// When the read response carries no chain, the mount's CA chain is fetched separately.
func GetCertWithFullChain(ctx context.Context, client *api.Client, engineName, serial string) (cert, fullChain string, err error) {
	cert, chain, err := readCert(ctx, client, engineName, serial)
	if err != nil {
		return "", "", err
	}

	if len(chain) == 0 {
		chain, err = GetCAChain(ctx, client, engineName)
		if err != nil {
			return "", "", err
		}
	}

	return cert, joinPEMChain(cert, chain), nil
}

// GetCAChain returns the CA chain of the given PKI mount, from the issuing CA up to the root.
func GetCAChain(ctx context.Context, client *api.Client, engineName string) ([]string, error) {
	pathToRead := engineName + "/cert/ca_chain"

	response, err := client.Logical().ReadWithContext(ctx, pathToRead)
	if err != nil {
		return nil, fmt.Errorf("unable to read CA chain from OpenBao: %w", err)
	}

	if response == nil || response.Data == nil {
		return nil, nil
	}

	if chain := parseCAChain(response.Data); len(chain) > 0 {
		return chain, nil
	}

	// The ca_chain pseudo-serial returns the concatenated chain in the certificate field.
	if bundle, ok := response.Data["certificate"].(string); ok && strings.TrimSpace(bundle) != "" {
		return []string{bundle}, nil
	}

	return nil, nil
}

// readCert reads a certificate and, when available, its issuing CA chain from the PKI engine.
func readCert(ctx context.Context, client *api.Client, engineName, serial string) (string, []string, error) {
	pathToRead := fmt.Sprintf("%s/cert/%s", engineName, serial)

	response, err := client.Logical().ReadWithContext(ctx, pathToRead)
	if err != nil {
		return "", nil, fmt.Errorf("unable to read certificate from OpenBao: %w", err)
	}

	if response == nil {
		return "", nil, errors.New("no data found for the given certificate serial: " + serial)
	}

	if response.Data == nil {
		return "", nil, errors.New("no data found in the response")
	}

	certValue, ok := response.Data["certificate"].(string)
	if !ok {
		return "", nil, errors.New("certificate not found in the response data")
	}

	return certValue, parseCAChain(response.Data), nil
}

// ListRevokedCertSerials returns the serial numbers of certificates revoked from the given PKI
// mount. When the mount has no revoked certificates yet, OpenBao may return no list data.
func ListRevokedCertSerials(ctx context.Context, client *api.Client, engineName string) ([]string, error) {
	pathToList := engineName + "/certs/revoked"

	response, err := client.Logical().ListWithContext(ctx, pathToList)
	if err != nil {
		return nil, fmt.Errorf("unable to list revoked certificates from OpenBao: %w", err)
	}

	if response == nil || response.Data == nil {
		return nil, nil
	}

	keysRaw, ok := response.Data["keys"]
	if !ok {
		return nil, nil
	}

	switch keys := keysRaw.(type) {
	case []string:
		return keys, nil
	case []any:
		serials := make([]string, 0, len(keys))
		for _, key := range keys {
			serial, ok := key.(string)
			if !ok {
				continue
			}

			serials = append(serials, serial)
		}

		return serials, nil
	default:
		return nil, errors.New("unexpected revoked certificate list response")
	}
}

func normalizeCertSerial(serial string) string {
	replacer := strings.NewReplacer(":", "", "-", "")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(serial)))
}

// IssueCert issues a new certificate (with a matching private key) from the PKI engine in OpenBao
// using the provided engine name, role name, and common name. Unlike GetCert, which reads a
// previously-issued certificate by serial, IssueCert always generates a brand-new certificate/key
// pair, making it suitable for automatic certificate rotation.
func IssueCert(ctx context.Context, client *api.Client, engineName, roleName, commonName string) (IssuedCertificate, error) {
	pathToIssue := fmt.Sprintf("%s/issue/%s", engineName, roleName)

	response, err := client.Logical().WriteWithContext(ctx, pathToIssue, map[string]any{
		"common_name": commonName,
	})
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("unable to issue certificate from OpenBao: %w", err)
	}

	if response == nil || response.Data == nil {
		return IssuedCertificate{}, errors.New("no data found when issuing certificate")
	}

	certValue, ok := response.Data["certificate"].(string)
	if !ok {
		return IssuedCertificate{}, errors.New("certificate not found in the issue response data")
	}

	keyValue, ok := response.Data["private_key"].(string)
	if !ok {
		return IssuedCertificate{}, errors.New("private key not found in the issue response data")
	}

	var expiration time.Time

	switch exp := response.Data["expiration"].(type) {
	case json.Number:
		seconds, convErr := exp.Int64()
		if convErr != nil {
			return IssuedCertificate{}, fmt.Errorf("unable to parse certificate expiration: %w", convErr)
		}

		expiration = time.Unix(seconds, 0).UTC()
	case float64:
		expiration = time.Unix(int64(exp), 0).UTC()
	default:
		return IssuedCertificate{}, errors.New("expiration not found in the issue response data")
	}

	return IssuedCertificate{
		Certificate: certValue,
		CAChain:     parseCAChain(response.Data),
		PrivateKey:  keyValue,
		Expiration:  expiration,
	}, nil
}
