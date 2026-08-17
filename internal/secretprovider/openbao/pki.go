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
// including its matching private key and expiration time.
type IssuedCertificate struct {
	Certificate string
	PrivateKey  string
	Expiration  time.Time
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
	pathToRead := fmt.Sprintf("%s/cert/%s", engineName, serial)

	response, err := client.Logical().ReadWithContext(ctx, pathToRead)
	if err != nil {
		return "", fmt.Errorf("unable to read certificate from OpenBao: %w", err)
	}

	if response == nil {
		return "", errors.New("no data found for the given certificate serial: " + serial)
	}

	if response.Data == nil {
		return "", errors.New("no data found in the response")
	}

	certValue, ok := response.Data["certificate"].(string)
	if !ok {
		return "", errors.New("certificate not found in the response data")
	}

	return certValue, nil
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
		PrivateKey:  keyValue,
		Expiration:  expiration,
	}, nil
}
