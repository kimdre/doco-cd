package webhook

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"text/template"
)

// BuildTemplateFuncMap returns a map of template functions available for webhook store templates.
// These functions can be used to transform values in url, headers, body, and json_path fields.
func BuildTemplateFuncMap() template.FuncMap {
	return template.FuncMap{
		"b64enc": func(input string) string {
			return base64.StdEncoding.EncodeToString([]byte(input))
		},
		"b64dec": func(input string) (string, error) {
			decoded, err := base64.StdEncoding.DecodeString(input)
			if err != nil {
				return "", fmt.Errorf("failed to decode base64: %w", err)
			}

			return string(decoded), nil
		},
		"urlencode": url.QueryEscape,
		"urldecode": func(input string) (string, error) {
			decoded, err := url.QueryUnescape(input)
			if err != nil {
				return "", fmt.Errorf("failed to decode URL: %w", err)
			}

			return decoded, nil
		},
		"json": func(input any) (string, error) {
			data, err := json.Marshal(input)
			if err != nil {
				return "", fmt.Errorf("failed to marshal to JSON: %w", err)
			}

			return string(data), nil
		},
		"toUpper": strings.ToUpper,
		"toLower": strings.ToLower,
		"trim":    strings.TrimSpace,
	}
}
