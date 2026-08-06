package httperror

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FormatErrorResponseBody formats the response body into a human-readable string.
// It returns two strings:
// the first is the raw body text, and the second is a structured representation of the error details if the body is in JSON format.
// If the body is empty or cannot be parsed, both strings will be empty.
func FormatErrorResponseBody(body []byte) (string, string) {
	bodyText := strings.Join(strings.Fields(strings.TrimSpace(string(body))), " ")
	if bodyText == "" {
		return "", ""
	}

	parsedDetails := parseErrorResponseJSON(body)
	if parsedDetails == "" {
		return bodyText, ""
	}

	return bodyText, parsedDetails
}

// parseErrorResponseJSON attempts to parse the response body as JSON and extract relevant error details.
func parseErrorResponseJSON(body []byte) string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err == nil {
		message := firstStringField(root, "message")
		typeName := firstStringField(root, "typeName")
		errorCode := firstField(root, "errorCode")

		if message == "" && typeName == "" && errorCode == nil {
			return ""
		}

		parts := make([]string, 0, 3)
		if message != "" {
			parts = append(parts, "message="+message)
		}

		if typeName != "" {
			parts = append(parts, "typeName="+typeName)
		}

		if errorCode != nil {
			parts = append(parts, "errorCode="+formatErrorCode(errorCode))
		}

		return strings.Join(parts, ", ")
	}

	var message string
	if err := json.Unmarshal(body, &message); err != nil {
		return ""
	}

	message = strings.Join(strings.Fields(strings.TrimSpace(message)), " ")
	if message == "" {
		return ""
	}

	return "message=" + message
}

// firstStringField retrieves the first occurrence of a string field from the root map or its nested "error" map.
func firstStringField(root map[string]any, field string) string {
	if value := firstField(root, field); value != nil {
		switch v := value.(type) {
		case string:
			return strings.Join(strings.Fields(strings.TrimSpace(v)), " ")
		default:
			return strings.Join(strings.Fields(strings.TrimSpace(fmt.Sprint(v))), " ")
		}
	}

	return ""
}

// firstField retrieves the first occurrence of a field from the root map or its nested "error" map.
func firstField(root map[string]any, field string) any {
	if root == nil {
		return nil
	}

	if value, ok := root[field]; ok {
		return value
	}

	errorData, ok := root["error"].(map[string]any)
	if !ok {
		return nil
	}

	if value, ok := errorData[field]; ok {
		return value
	}

	return nil
}

// formatErrorCode formats the error code into a string representation.
func formatErrorCode(code any) string {
	switch value := code.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}
