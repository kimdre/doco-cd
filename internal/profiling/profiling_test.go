package profiling

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerServesHeapProfile(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, Path+"heap", nil)
	res := httptest.NewRecorder()

	newHandler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("heap profile status = %d, want %d", res.Code, http.StatusOK)
	}

	if res.Body.Len() == 0 {
		t.Fatal("heap profile response is empty")
	}
}
