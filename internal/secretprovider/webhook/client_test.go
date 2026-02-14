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
)

type testCase struct {
	haveSecretID       string
	haveResultJMESPath string
	haveURLPath        string
	haveRequestBody    string
	haveBearerToken    string
	haveBasicUsername  string
	haveBasicPassword  string

	wantSecret string
	wantErr    bool
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

	if c.haveBearerToken != "" {
		t.Setenv("SECRET_PROVIDER_BEARER_TOKEN", c.haveBearerToken)
	}

	if c.haveBasicUsername != "" {
		t.Setenv("SECRET_PROVIDER_BASIC_USERNAME", c.haveBasicUsername)
	}

	if c.haveBasicPassword != "" {
		t.Setenv("SECRET_PROVIDER_BASIC_PASSWORD", c.haveBasicPassword)
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
			haveBasicPassword: "password",
			wantSecret:        "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
		},
		"bearer_token": {
			haveSecretID:    "authorization",
			haveBearerToken: "DEADBEEFCAFE",
			wantSecret:      "Bearer DEADBEEFCAFE",
		},
		"lookup_error": {
			haveSecretID: "error",
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
	}

	ts := httptest.NewServer(newMockHandler())
	defer ts.Close()

	for name, tc := range testCases {
		tr := func(t *testing.T) {
			tc.LoadEnv(ts.URL, t)

			cfg, err := GetConfig()
			if err != nil {
				t.Fatalf("Unable to get config: %v", err)
			}

			subject, err := NewValueProvider(t.Context(), cfg)
			if err != nil {
				t.Fatalf("Failed to create Webhook value provider: %v", err)
			}

			got, err := subject.GetSecret(t.Context(), tc.haveSecretID)

			if !tc.wantErr && err != nil {
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
	if err != nil {
		httpRespondError(w, http.StatusBadRequest, errNoLookup)
		return
	}

	httpRespondSecret(w, result)
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
	case "method":
		return r.Method, nil
	case "error":
		return "", errMockError
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
