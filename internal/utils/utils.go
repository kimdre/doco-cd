package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type jsonError struct {
	Code       int    `json:"code"`
	Error      string `json:"error"`
	Details    string `json:"details,omitempty"`
	Repository string `json:"repository,omitempty"`
	JobID      string `json:"job_id,omitempty"`
}

// JSONError writes an error response to the client in JSON format
func JSONError(w http.ResponseWriter, err interface{}, details, repo, jobId string, code int) {
	if _, ok := err.(error); ok {
		err = fmt.Sprintf("%v", err)
	}

	err = jsonError{
		Error:      err.(string),
		Code:       code,
		Details:    details,
		Repository: repo,
		JobID:      jobId,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	err = json.NewEncoder(w).Encode(err)
	if err != nil {
		return
	}
}
