package restapi

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Response is the common JSON envelope for successful REST responses.
type Response struct {
	Content any    `json:"content,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

// ErrorResponse inherits from Response and adds an error message.
type ErrorResponse struct {
	Error string `json:"error"`
	Response
}

// JSONError writes an error response to the client in JSON format.
func JSONError(w http.ResponseWriter, err, details any, jobId string, code int) {
	if _, ok := err.(error); ok {
		err = fmt.Sprintf("%v", err)
	}

	resp := ErrorResponse{
		Error:   err.(string),
		Content: details,
		JobID:   jobId,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		return
	}
}

// JSONResponse writes a successful response in the common JSON envelope.
func JSONResponse(w http.ResponseWriter, content any, jobId string, code int) {
	resp := Response{
		Content: content,
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
