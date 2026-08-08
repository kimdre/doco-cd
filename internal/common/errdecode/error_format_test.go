package errdecode_test

import (
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/common/errdecode"
)

func TestFormatErrorResponseBody_JSONObject(t *testing.T) {
	t.Parallel()

	body := []byte(`{"message":"\u041d\u0435\u0434\u043e\u043f\u0443\u0441\u0442\u0438\u043c\u0430\u044f \u0432\u0435\u0442\u043a\u0430","typeName":"Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException","errorCode":12345}`)

	bodyText, structured := errdecode.FormatErrorResponseBody(body)

	if bodyText == "" {
		t.Fatal("expected non-empty raw body text")
	}

	if !strings.Contains(structured, "message=Недопустимая ветка") {
		t.Errorf("expected decoded message in structured output, got: %s", structured)
	}

	if !strings.Contains(structured, "typeName=Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException") {
		t.Errorf("expected typeName in structured output, got: %s", structured)
	}

	if !strings.Contains(structured, "errorCode=12345") {
		t.Errorf("expected errorCode in structured output, got: %s", structured)
	}

	if strings.Contains(structured, `\u041d`) {
		t.Errorf("structured output should not contain escaped unicode, got: %s", structured)
	}
}

func TestFormatErrorResponseBody_NestedErrorObject(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"message":"nested failure","typeName":"SomeException","errorCode":42}}`)

	_, structured := errdecode.FormatErrorResponseBody(body)

	if !strings.Contains(structured, "message=nested failure") {
		t.Errorf("expected message from nested error object, got: %s", structured)
	}

	if !strings.Contains(structured, "errorCode=42") {
		t.Errorf("expected errorCode from nested error object, got: %s", structured)
	}
}

func TestFormatErrorResponseBody_JSONString(t *testing.T) {
	t.Parallel()

	body := []byte(`"\u041d\u0435\u0434\u043e\u0441\u0442\u0443\u043f\u043d\u043e"`)

	_, structured := errdecode.FormatErrorResponseBody(body)

	if !strings.Contains(structured, "message=Недоступно") {
		t.Errorf("expected decoded JSON string message, got: %s", structured)
	}
}

func TestFormatErrorResponseBody_PlainText(t *testing.T) {
	t.Parallel()

	body := []byte("not-json body content")

	bodyText, structured := errdecode.FormatErrorResponseBody(body)

	if bodyText != "not-json body content" {
		t.Errorf("expected raw body text to be preserved, got: %s", bodyText)
	}

	if structured != "" {
		t.Errorf("expected no structured details for non-JSON body, got: %s", structured)
	}
}

func TestFormatErrorResponseBody_Empty(t *testing.T) {
	t.Parallel()

	bodyText, structured := errdecode.FormatErrorResponseBody([]byte("   "))

	if bodyText != "" || structured != "" {
		t.Errorf("expected empty results for blank body, got bodyText=%q structured=%q", bodyText, structured)
	}
}

func TestDecodeEmbeddedJSON_ReplacesObjectInPlace(t *testing.T) {
	t.Parallel()

	text := "authentication required (or check https://dev.azure.com for error details)\n" +
		`remote: {"message":"\u041d\u0435\u0434\u043e\u043f\u0443\u0441\u0442\u0438\u043c\u0430\u044f \u0432\u0435\u0442\u043a\u0430","typeName":"Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException","errorCode":12345}`

	result := errdecode.DecodeEmbeddedJSON(text)

	if !strings.Contains(result, "Недопустимая ветка") {
		t.Errorf("expected decoded message in result, got: %s", result)
	}

	if !strings.Contains(result, "typeName=Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException") {
		t.Errorf("expected typeName in result, got: %s", result)
	}

	if !strings.Contains(result, "errorCode=12345") {
		t.Errorf("expected errorCode in result, got: %s", result)
	}

	if strings.Contains(result, `\u041d`) {
		t.Errorf("result should not contain escaped unicode, got: %s", result)
	}

	if strings.Contains(result, "{") || strings.Contains(result, "}") {
		t.Errorf("result should not contain the raw JSON braces after replacement, got: %s", result)
	}

	if !strings.HasPrefix(result, "authentication required") {
		t.Errorf("result should preserve text preceding the JSON, got: %s", result)
	}
}

func TestDecodeEmbeddedJSON_ReplacesBareEscapedString(t *testing.T) {
	t.Parallel()

	text := `authentication required: "\u041d\u0435\u0434\u043e\u0441\u0442\u0443\u043f\u043d\u043e"`

	result := errdecode.DecodeEmbeddedJSON(text)

	if !strings.Contains(result, "Недоступно") {
		t.Errorf("expected decoded text in result, got: %s", result)
	}

	if strings.Contains(result, `\u041d`) {
		t.Errorf("result should not contain escaped unicode, got: %s", result)
	}
}

func TestDecodeEmbeddedJSON_ReturnsOriginalWhenNothingToDecode(t *testing.T) {
	t.Parallel()

	text := "simple error message without JSON or escapes"

	result := errdecode.DecodeEmbeddedJSON(text)

	if result != text {
		t.Errorf("expected original text unchanged, got: %s", result)
	}
}

func TestDecodeEmbeddedJSON_IgnoresPlainQuotedTextWithoutEscapes(t *testing.T) {
	t.Parallel()

	// A quoted substring without a \u escape should not be rewritten, since it's
	// likely unrelated plain text rather than a JSON-encoded localized message.
	text := `some error: "just a quoted phrase"`

	result := errdecode.DecodeEmbeddedJSON(text)

	if result != text {
		t.Errorf("expected text with plain quotes to remain unchanged, got: %s", result)
	}
}
