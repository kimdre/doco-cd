package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestJSONResponse(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()

	jobId := uuid.Must(uuid.NewV7()).String()

	JSONResponse(rr, "this is a test", jobId, http.StatusOK)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			rr.Code, http.StatusOK)
	}

	expectedReturnMessage := fmt.Sprintf(`{"content":"this is a test","job_id":"%s"}%s`, jobId, "\n")
	if rr.Body.String() != expectedReturnMessage {
		t.Errorf("handler returned unexpected body: got '%v' want '%v'",
			rr.Body.String(), expectedReturnMessage)
	}
}

func TestJSONError(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()

	jobId := uuid.Must(uuid.NewV7()).String()

	JSONError(rr, "this is a error", "this is a detail", jobId, http.StatusInternalServerError)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("handler returned wrong status code: got %v want %v",
			rr.Code, http.StatusInternalServerError)
	}

	expectedReturnMessage := fmt.Sprintf(`{"error":"this is a error","content":"this is a detail","job_id":"%s"}%s`, jobId, "\n")
	if rr.Body.String() != expectedReturnMessage {
		t.Errorf("handler returned unexpected body: got '%v' want '%v'",
			rr.Body.String(), expectedReturnMessage)
	}
}
