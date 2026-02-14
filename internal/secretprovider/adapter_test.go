package secretprovider

import (
	"context"
	"strings"
	"testing"
)

func TestSecretProviderAdapter_ResolveSecretReferences(t *testing.T) {
	impl := SecretValueProviderFunc(secretProviderMock)
	subject := AdaptSecretValueProvider("mock", impl)
	have := map[string]string{
		"KEY01": "first-value",
		"KEY02": "second-value",
		"KEY03": "third-value",
		"KEY04": "fourth-value",
	}

	got, err := subject.ResolveSecretReferences(t.Context(), have)
	if err != nil {
		t.Errorf("Unwanted error: %v", err)
	}

	for k, v := range got {
		switch k {
		case "KEY01":
			assertSecretLookup(t, k, v, "FIRST-VALUE")
		case "KEY02":
			assertSecretLookup(t, k, v, "SECOND-VALUE")
		case "KEY03":
			assertSecretLookup(t, k, v, "THIRD-VALUE")
		case "KEY04":
			assertSecretLookup(t, k, v, "FOURTH-VALUE")
		default:
			t.Errorf("Unexpected secret reference %q (%q)", k, v)
		}
	}
}

func assertSecretLookup(t *testing.T, haveKey, gotValue, wantValue string) {
	if gotValue != wantValue {
		t.Errorf("invalid resolution for key %q; got %q, want %q", haveKey, gotValue, wantValue)
	}
}

func secretProviderMock(_ context.Context, lookup string) (string, error) {
	return strings.ToUpper(lookup), nil
}
