package openbao

import (
	"context"
	"errors"
	"fmt"

	"github.com/openbao/openbao/api/v2"
)

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
