package webhook

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var (
	errMockError = errors.New("simulated error")
	errNoLookup  = errors.New("no secret lookup key provider")
	errNoJSON    = errors.New("unsupported media type")
)

type testCase struct {
	haveSecretID          string
	haveResultJMESPath    string
	haveURLPath           string
	haveRequestBody       string
	haveCustomHeaders     string
	haveBearerToken       string
	haveBasicUsername     string
	haveBasicPassword     string
	haveBearerTokenFile   string
	haveBasicUsernameFile string
	haveBasicPasswordFile string

	wantSecret    string
	wantConfigErr bool
	wantCtorErr   bool
	wantErr       bool
}

func (c *testCase) LoadEnv(baseURL string, t *testing.T) {
	urlPath := "/get/{{.remoteRef}}"
	jmesPath := "result"

	if c.haveResultJMESPath != "" {
		jmesPath = c.haveResultJMESPath
	}

	if c.haveURLPath != "" {
		urlPath = c.haveURLPath
	}

	t.Setenv("SECRET_PROVIDER_SITE_URL", baseURL+urlPath)
	t.Setenv("SECRET_PROVIDER_RESULT_JMES_PATH", jmesPath)

	if c.haveRequestBody != "" {
		t.Setenv("SECRET_PROVIDER_REQUEST_BODY", c.haveRequestBody)
	}

	if c.haveCustomHeaders != "" {
		t.Setenv("SECRET_PROVIDER_CUSTOM_HEADERS", c.haveCustomHeaders)
	}

	if c.haveBearerToken != "" {
		t.Setenv("SECRET_PROVIDER_BEARER_TOKEN", c.haveBearerToken)
	}

	if c.haveBasicUsername != "" {
		t.Setenv("SECRET_PROVIDER_BASIC_USERNAME", c.haveBasicUsername)
	}

	if c.haveBasicPassword != "" {
		t.Setenv("SECRET_PROVIDER_BASIC_PASSWORD", c.haveBasicPassword)
	}

	if c.haveBearerTokenFile != "" {
		t.Setenv("SECRET_PROVIDER_BEARER_TOKEN_FILE", c.haveBearerTokenFile)
	}

	if c.haveBasicUsernameFile != "" {
		t.Setenv("SECRET_PROVIDER_BASIC_USERNAME_FILE", c.haveBasicUsernameFile)
	}

	if c.haveBasicPasswordFile != "" {
		t.Setenv("SECRET_PROVIDER_BASIC_PASSWORD_FILE", c.haveBasicPasswordFile)
	}
}

func TestValueProvider_GetSecret_Webhook(t *testing.T) {
	testCases := map[string]testCase{
		"get": {
			haveSecretID: "bacgaff",
			wantSecret:   "BACGAFF",
		},
		"post": {
			haveSecretID:    "bacgaff",
			haveRequestBody: `{"secret":"{{.remoteRef}}"}`,
			wantSecret:      "BACGAFF",
		},
		"accept": {
			haveSecretID: "accept",
			wantSecret:   ContentTypeJSON,
		},
		"content_type_GET": {
			haveSecretID: "content-type",
		},
		"content_type_POST": {
			haveSecretID:    "content-type",
			haveRequestBody: `{"secret":"{{.remoteRef}}"}`,
			wantSecret:      ContentTypeJSON + "; charset=utf-8",
		},
		"unauthenticated_GET": {
			haveSecretID: "method",
			wantSecret:   "GET",
		},
		"unauthenticated_POST": {
			haveSecretID:    "method",
			haveRequestBody: `{"secret":"{{.remoteRef}}"}`,
			wantSecret:      "POST",
		},
		"basic_auth": {
			haveSecretID:      "authorization",
			haveBasicUsername: "username",
			haveBasicPassword: "password", // #nosec G101
			wantSecret:        "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
		},
		"basic_auth_without_password": {
			haveSecretID:      "authorization",
			haveBasicUsername: "username",
			wantSecret:        "Basic dXNlcm5hbWU6dXNlcm5hbWU=", // #nosec G101
		},
		"bearer_token": {
			haveSecretID:    "authorization",
			haveBearerToken: "DEADBEEFCAFE",
			wantSecret:      "Bearer DEADBEEFCAFE", // #nosec G101
		},
		"lookup_error": {
			haveSecretID: "error",
			wantErr:      true,
		},
		"decoding_error": {
			haveSecretID: "no-json",
			wantErr:      true,
		},
		"not_found": {
			haveSecretID: "404",
			haveURLPath:  "/not-found",
			wantErr:      true,
		},
		"jmespath_query_failed": {
			haveSecretID:       "test",
			haveResultJMESPath: "result.secret",
			wantErr:            true,
		},
		"jmespath_invalid_datatype": {
			haveSecretID:       "test",
			haveResultJMESPath: "length",
			wantErr:            true,
		},
		"broken_jmespath_pattern": {
			haveSecretID:       "broken",
			haveResultJMESPath: `test["]`,
			wantCtorErr:        true,
		},
		"broken_endpoint_template": {
			haveSecretID: "broken",
			haveURLPath:  "{{ //",
			wantCtorErr:  true,
		},
		"broken_endpoint_render": {
			haveSecretID: "broken",
			haveURLPath:  `{{ if "test" lt "case" }}{{ end }}`,
			wantErr:      true,
		},
		"broken_endpoint_url": {
			haveSecretID: "broken",
			haveURLPath:  ":1:2???3",
			wantErr:      true,
		},
		"broken_payload_template": {
			haveSecretID:    "broken",
			haveRequestBody: "{{ //",
			wantCtorErr:     true,
		},
		"broken_payload_render": {
			haveSecretID:    "broken",
			haveRequestBody: `{{ if "test" lt "case" }}{{ end }}`,
			wantErr:         true,
		},
		"broken_authentication_values": {
			haveSecretID:      "broken",
			haveBearerToken:   "DEADBEEFCAFE",
			haveBasicUsername: "username",
			wantCtorErr:       true,
		},
		"broken_authentication_setup": {
			haveSecretID:        "broken",
			haveBearerToken:     "DEADBEEFCAFE",
			haveBearerTokenFile: "testdata/does/not/exist.txt",
			wantConfigErr:       true,
		},
		"custom_headers": {
			haveSecretID:      "x-custom-header",
			haveCustomHeaders: `{"X-Custom-Header":"custom-value"}`,
			wantSecret:        "custom-value",
		},
		"custom_headers_override_accept": {
			haveSecretID:      "accept",
			haveCustomHeaders: `{"Accept":"text/plain"}`,
			wantSecret:        "text/plain",
		},
		"custom_headers_multiple": {
			haveSecretID:      "x-custom-header",
			haveCustomHeaders: `{"X-Custom-Header":"multi-value","X-Other":"other"}`,
			wantSecret:        "multi-value",
		},
		"broken_custom_headers_json": {
			haveSecretID:      "broken",
			haveCustomHeaders: `{not valid json}`,
			wantConfigErr:     true,
		},
	}

	ts := httptest.NewServer(newMockHandler())
	defer ts.Close()

	for name, tc := range testCases {
		tr := func(t *testing.T) {
			tc.LoadEnv(ts.URL, t)

			cfg, err := GetConfig()
			if tc.wantConfigErr {
				if err == nil {
					t.Fatalf("Expected configuration to yield an error")
				} else {
					return // no need to continue with invalid data
				}
			} else if err != nil {
				t.Fatalf("Unwanted config error: %v", err)
			}

			subject, err := NewValueProvider(t.Context(), cfg)
			if tc.wantCtorErr {
				if err == nil {
					t.Fatalf("Expected construction to yield an error")
				} else {
					return // no need to continue with invalid data
				}
			} else if err != nil {
				t.Fatalf("Failed to create Webhook value provider: %v", err)
			}

			got, err := subject.GetSecret(t.Context(), tc.haveSecretID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Expected GetSecret() to yield an error, got %q", got)
				} else {
					return // no need to continue with invalid data
				}
			} else if err != nil {
				t.Errorf("Unwanted error: %v", err)
			}

			if got != tc.wantSecret {
				t.Errorf("got %q, want %q", got, tc.wantSecret)
			}
		}

		t.Run(name, tr)
	}
}

func newMockHandler() http.Handler {
	handler := http.NewServeMux()

	handler.HandleFunc("/post", postSecret)
	handler.HandleFunc("/get/{secret}", getSecret)

	return handler
}

func getSecret(w http.ResponseWriter, r *http.Request) {
	lookup := r.PathValue("secret")

	result, err := lookupSecret(lookup, r)
	if err != nil {
		httpRespondError(w, http.StatusBadRequest, errNoLookup)
		return
	}

	httpRespondSecret(w, result)
}

func postSecret(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(HeaderContentType, ContentTypeJSON+"; charset=utf-8")

	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpRespondError(w, http.StatusBadRequest, err)
		return
	}

	lookup, ok := body["secret"]
	if !ok {
		httpRespondError(w, http.StatusBadRequest, errNoLookup)
		return
	}

	result, err := lookupSecret(lookup, r)
	switch {
	case err == nil:
		httpRespondSecret(w, result)
	case errors.Is(err, errNoJSON):
		http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
	default:
		httpRespondError(w, http.StatusBadRequest, errNoLookup)
	}
}

func lookupSecret(key string, r *http.Request) (string, error) {
	switch key {
	case "content-type":
		return r.Header.Get(HeaderContentType), nil
	case "accept":
		return r.Header.Get(HeaderAccept), nil
	case "user-agent":
		return r.Header.Get("User-Agent"), nil
	case "authorization":
		return r.Header.Get("Authorization"), nil
	case "x-custom-header":
		return r.Header.Get("X-Custom-Header"), nil
	case "method":
		return r.Method, nil
	case "error":
		return "", errMockError
	case "no-json":
		return "", errNoJSON
	case "":
		return "", errNoLookup
	default:
		return strings.ToUpper(key), nil
	}
}

func httpRespondSecret(w http.ResponseWriter, secret string) {
	body := map[string]interface{}{
		"result": secret,
		"length": len(secret), // additional non-string data for datatype test case
	}
	h := w.Header()

	h.Set(HeaderContentType, ContentTypeJSON+"; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func httpRespondError(w http.ResponseWriter, code int, err error) {
	body := map[string]interface{}{
		"error": err.Error(),
	}
	h := w.Header()

	h.Del("Content-Length")
	h.Set(HeaderContentType, ContentTypeJSON+"; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
