package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type jsonResponse struct {
	Details string `json:"details,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

// jsonError inherits from jsonResponse and adds an error message
type jsonError struct {
	Error string `json:"error"`
	jsonResponse
}

// JSONError writes an error response to the client in JSON format
func JSONError(w http.ResponseWriter, err interface{}, details, jobId string, code int) {
	if _, ok := err.(error); ok {
		err = fmt.Sprintf("%v", err)
	}

	resp := jsonError{
		Error: err.(string),
		jsonResponse: jsonResponse{
			Details: details,
			JobID:   jobId,
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		return
	}
}

func JSONResponse(w http.ResponseWriter, details, jobId string, code int) {
	resp := jsonResponse{
		Details: details,
		JobID:   jobId,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	err := json.NewEncoder(w).Encode(resp)
	if err != nil {
		return
	}
}
