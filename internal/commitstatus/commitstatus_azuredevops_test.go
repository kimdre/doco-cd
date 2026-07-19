package commitstatus_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/kimdre/doco-cd/internal/commitstatus"
)

func TestPost_AzureDevOpsAPI(t *testing.T) {
	t.Parallel()

	received := map[string]any{}
	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":token"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/org/project/_apis/git/repositories/repo/commits/deadbeef/statuses")
		assert.Equal(t, r.URL.Query().Get("api-version"), "7.1")
		assert.Equal(t, r.Header.Get("Authorization"), expectedAuth)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAzureDevOps,
		srv.URL+"/org/project/_git/repo",
		"org/project/_git/repo",
		"deadbeef",
		"token",
		commitstatus.Status{
			State:       commitstatus.StateSuccess,
			Description: "Successful in 47s",
			Context:     "doco-cd/deploy",
			TargetURL:   "https://example.com/logs/1",
		},
	)
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "succeeded")
	assert.Equal(t, received["description"], "Successful in 47s")
	contextData, ok := received["context"].(map[string]any)
	assert.Assert(t, ok)
	assert.Equal(t, contextData["name"], "doco-cd/deploy")
	assert.Equal(t, contextData["genre"], "doco-cd")
	assert.Equal(t, received["targetUrl"], "https://example.com/logs/1")
}

func TestGet_AzureDevOpsAPI(t *testing.T) {
	t.Parallel()

	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":token"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Equal(t, r.URL.Path, "/org/project/_apis/git/repositories/repo/commits/deadbeef/statuses")
		assert.Equal(t, r.URL.Query().Get("api-version"), "7.1")
		assert.Equal(t, r.Header.Get("Authorization"), expectedAuth)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{
					"state":       "succeeded",
					"description": "Successful in 47s",
					"targetUrl":   "https://example.com/logs/1",
					"context": map[string]string{
						"name":  commitstatus.BaseContext,
						"genre": "doco-cd",
					},
				},
			},
		})
	}))
	defer srv.Close()

	status, found, err := commitstatus.Get(context.Background(),
		commitstatus.ProviderAzureDevOps,
		srv.URL+"/org/project/_git/repo",
		"org/project/_git/repo",
		"deadbeef",
		"token",
		commitstatus.BaseContext)
	assert.NilError(t, err)
	assert.Assert(t, found)
	assert.Equal(t, status.State, commitstatus.StateSuccess)
	assert.Equal(t, status.Description, "Successful in 47s")
	assert.Equal(t, status.TargetURL, "https://example.com/logs/1")
}
