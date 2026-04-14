package webhook

import (
	"testing"
)

func TestBuildTemplateFuncMap(t *testing.T) {
	funcMap := BuildTemplateFuncMap()

	t.Run("b64enc", func(t *testing.T) {
		b64enc := funcMap["b64enc"].(func(string) string)
		got := b64enc("hello world")
		expected := "aGVsbG8gd29ybGQ="
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("b64dec", func(t *testing.T) {
		b64dec := funcMap["b64dec"].(func(string) (string, error))
		got, err := b64dec("aGVsbG8gd29ybGQ=")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := "hello world"
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("b64dec_error", func(t *testing.T) {
		b64dec := funcMap["b64dec"].(func(string) (string, error))
		_, err := b64dec("invalid!!!base64")
		if err == nil {
			t.Fatalf("expected error for invalid base64")
		}
	})

	t.Run("urlencode", func(t *testing.T) {
		urlencode := funcMap["urlencode"].(func(string) string)
		got := urlencode("hello world & special=chars")
		expected := "hello+world+%26+special%3Dchars"
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("urldecode", func(t *testing.T) {
		urldecode := funcMap["urldecode"].(func(string) (string, error))
		got, err := urldecode("hello+world+%26+special%3Dchars")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := "hello world & special=chars"
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("urldecode_error", func(t *testing.T) {
		urldecode := funcMap["urldecode"].(func(string) (string, error))
		_, err := urldecode("%ZZ")
		if err == nil {
			t.Fatalf("expected error for invalid URL encoding")
		}
	})

	t.Run("json", func(t *testing.T) {
		jsonFunc := funcMap["json"].(func(interface{}) (string, error))
		got, err := jsonFunc(map[string]string{"key": "value"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := `{"key":"value"}`
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("toUpper", func(t *testing.T) {
		toUpper := funcMap["toUpper"].(func(string) string)
		got := toUpper("Hello World")
		expected := "HELLO WORLD"
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("toLower", func(t *testing.T) {
		toLower := funcMap["toLower"].(func(string) string)
		got := toLower("Hello World")
		expected := "hello world"
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("trim", func(t *testing.T) {
		trim := funcMap["trim"].(func(string) string)
		got := trim("  hello world  \n")
		expected := "hello world"
		if got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})
}

