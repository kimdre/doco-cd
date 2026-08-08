package errdecode

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

// DecodeEmbeddedJSON scans text for an embedded JSON object (e.g. an error response body
// copied verbatim into an error message by an upstream library) or a bare JSON-encoded
// string containing Unicode escape sequences, and replaces it with a decoded,
// human-readable representation.
//
// This is useful for error messages produced by libraries (such as go-git) that include
// a remote server's raw response body as-is, which can contain visible "\uXXXX" escape
// sequences instead of readable UTF-8 text when the upstream server is localized
// (e.g. a non-English Azure DevOps Server).
//
// If no embedded JSON is found, or it cannot be parsed into a meaningful representation,
// the original text is returned unchanged.
func DecodeEmbeddedJSON(text string) string {
	if replaced, ok := replaceJSONObject(text); ok {
		return replaced
	}

	if replaced, ok := replaceEscapedJSONString(text); ok {
		return replaced
	}

	return text
}

// replaceJSONObject finds the outermost {...} span in text and, if it parses into
// meaningful structured error details, replaces it with those details.
func replaceJSONObject(text string) (string, bool) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return "", false
	}

	end := strings.LastIndexByte(text, '}')
	if end <= start {
		return "", false
	}

	_, structured := FormatErrorResponseBody([]byte(text[start : end+1]))
	if structured == "" {
		return "", false
	}

	return text[:start] + structured + text[end+1:], true
}

// replaceEscapedJSONString finds a quoted JSON string span in text that contains a
// Unicode escape sequence and, if it decodes successfully, replaces it with the decoded text.
// It only considers spans containing "\u" to avoid rewriting unrelated quoted text.
func replaceEscapedJSONString(text string) (string, bool) {
	if !strings.Contains(text, `\u`) {
		return "", false
	}

	start := strings.IndexByte(text, '"')
	if start < 0 {
		return "", false
	}

	end := strings.LastIndexByte(text, '"')
	if end <= start {
		return "", false
	}

	candidate := text[start : end+1]

	var decoded string
	if err := json.Unmarshal([]byte(candidate), &decoded); err != nil {
		return "", false
	}

	decoded = strings.Join(strings.Fields(strings.TrimSpace(decoded)), " ")
	if decoded == "" {
		return "", false
	}

	return text[:start] + decoded + text[end+1:], true
}
