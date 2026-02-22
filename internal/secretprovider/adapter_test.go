package secretprovider

import (
	"context"
	"strings"
	"testing"
)

type secretProviderCloseAssertion interface {
	SecretValueProvider

	CloseCalled() int
}

type secretProviderAdapterNop string

func (a secretProviderAdapterNop) GetSecret(_ context.Context, _ string) (string, error) {
	return string(a), nil
}

func (a secretProviderAdapterNop) CloseCalled() int {
	return 0
}

type secretProviderAdapterCloser struct {
	called int
}

func (a *secretProviderAdapterCloser) GetSecret(_ context.Context, _ string) (string, error) {
	return "closer", nil
}

func (a *secretProviderAdapterCloser) Close() {
	a.called++
}

func (a *secretProviderAdapterCloser) CloseCalled() int {
	return a.called
}

type secretProviderAdapterIOCloser struct {
	called int
}

func (a *secretProviderAdapterIOCloser) GetSecret(_ context.Context, _ string) (string, error) {
	return "ioCloser", nil
}

func (a *secretProviderAdapterIOCloser) Close() error {
	a.called++

	return nil
}

func (a *secretProviderAdapterIOCloser) CloseCalled() int {
	return a.called
}

func TestSecretProviderAdapter_Close(t *testing.T) {
	testCases := map[string]struct {
		haveProvider secretProviderCloseAssertion
		wantCalled   int
	}{
		"nop": {
			haveProvider: secretProviderAdapterNop("nop"),
		},
		"closer": {
			haveProvider: new(secretProviderAdapterCloser),
			wantCalled:   1,
		},
		"io_closer": {
			haveProvider: new(secretProviderAdapterIOCloser),
			wantCalled:   1,
		},
	}

	for name, tc := range testCases {
		tr := func(t *testing.T) {
			subject := AdaptSecretValueProvider(name, tc.haveProvider)

			subject.Close()

			if got := tc.haveProvider.CloseCalled(); got != tc.wantCalled {
				t.Errorf("got %d, want %d", got, tc.wantCalled)
			}
		}

		t.Run(name, tr)
	}
}

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
