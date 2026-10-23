package awssecretsmanager

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// fakeBulkClient serves a fixed set of secrets and records the requests it receives.
type fakeBulkClient struct {
	secrets map[string]*string
	// pageSize splits ListSecrets results into pages when greater than zero.
	pageSize int

	listFilters  [][]smtypes.Filter
	batchSizes   []int
	batchErrors  []smtypes.APIErrorType
	listErr      error
	batchErr     error
	listedByName []string
}

func (f *fakeBulkClient) ListSecrets(
	_ context.Context,
	in *secretsmanager.ListSecretsInput,
	_ ...func(*secretsmanager.Options),
) (*secretsmanager.ListSecretsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	f.listFilters = append(f.listFilters, in.Filters)

	prefix := ""

	for _, filter := range in.Filters {
		if filter.Key == smtypes.FilterNameStringTypeName && len(filter.Values) > 0 {
			prefix = filter.Values[0]
		}
	}

	var names []string

	for name := range f.secrets {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	start := 0

	if in.NextToken != nil {
		for i, name := range names {
			if name == *in.NextToken {
				start = i

				break
			}
		}
	}

	end := len(names)

	var next *string

	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
		next = aws.String(names[end])
	}

	out := &secretsmanager.ListSecretsOutput{NextToken: next}
	for _, name := range names[start:end] {
		out.SecretList = append(out.SecretList, smtypes.SecretListEntry{Name: aws.String(name)})
		f.listedByName = append(f.listedByName, name)
	}

	return out, nil
}

func (f *fakeBulkClient) BatchGetSecretValue(
	_ context.Context,
	in *secretsmanager.BatchGetSecretValueInput,
	_ ...func(*secretsmanager.Options),
) (*secretsmanager.BatchGetSecretValueOutput, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}

	f.batchSizes = append(f.batchSizes, len(in.SecretIdList))

	out := &secretsmanager.BatchGetSecretValueOutput{Errors: f.batchErrors}

	for _, id := range in.SecretIdList {
		out.SecretValues = append(out.SecretValues, smtypes.SecretValueEntry{
			Name:         aws.String(id),
			SecretString: f.secrets[id],
		})
	}

	return out, nil
}

func TestProvider_GetSecretsByPath(t *testing.T) {
	t.Parallel()

	fake := &fakeBulkClient{secrets: map[string]*string{
		"prod/app/db-password":   aws.String("one"),
		"prod/app/api-key":       aws.String("two"),
		"prod/app/nested/secret": aws.String("three"),
		"prod/app/binary":        nil,
		"prod/other/token":       aws.String("four"),
		"prod/application":       aws.String("five"),
	}}

	tests := []struct {
		name      string
		path      string
		recursive bool
		want      map[string]string
	}{
		{
			name: "folder without recursion",
			path: "prod/app",
			want: map[string]string{"db-password": "one", "api-key": "two"},
		},
		{
			name:      "folder with trailing slash and recursion",
			path:      "prod/app/",
			recursive: true,
			want:      map[string]string{"db-password": "one", "api-key": "two", "nested/secret": "three"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := (&Provider{bulk: fake}).GetSecretsByPath(t.Context(), tt.path, tt.recursive)
			if err != nil {
				t.Fatalf("GetSecretsByPath() error = %v", err)
			}

			if !maps.Equal(got, tt.want) {
				t.Errorf("GetSecretsByPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProvider_GetSecretsByPath_AllSecrets(t *testing.T) {
	t.Parallel()

	fake := &fakeBulkClient{secrets: map[string]*string{
		"top":      aws.String("1"),
		"dir/leaf": aws.String("2"),
	}}

	got, err := (&Provider{bulk: fake}).GetSecretsByPath(t.Context(), "/", false)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	if !maps.Equal(got, map[string]string{"top": "1"}) {
		t.Errorf("GetSecretsByPath() = %v", got)
	}

	if len(fake.listFilters) == 0 || len(fake.listFilters[0]) != 0 {
		t.Errorf("expected no name filter for the root path, got %v", fake.listFilters)
	}
}

func TestProvider_GetSecretsByPath_PagesAndBatches(t *testing.T) {
	t.Parallel()

	secrets := make(map[string]*string)

	for i := range 45 {
		secrets["bulk/s"+string(rune('a'+i/26))+string(rune('a'+i%26))] = aws.String("v")
	}

	fake := &fakeBulkClient{secrets: secrets, pageSize: 10}

	got, err := (&Provider{bulk: fake}).GetSecretsByPath(t.Context(), "bulk/", false)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	if len(got) != 45 {
		t.Errorf("expected 45 secrets, got %d", len(got))
	}

	if len(fake.batchSizes) != 3 || fake.batchSizes[0] != batchGetLimit || fake.batchSizes[2] != 5 {
		t.Errorf("unexpected batch sizes: %v", fake.batchSizes)
	}
}

func TestProvider_GetSecretsByPath_Errors(t *testing.T) {
	t.Parallel()

	secrets := map[string]*string{"p/a": aws.String("secret-value")}

	t.Run("list error", func(t *testing.T) {
		t.Parallel()

		listErr := errors.New("list failed")

		_, err := (&Provider{bulk: &fakeBulkClient{secrets: secrets, listErr: listErr}}).GetSecretsByPath(t.Context(), "p/", false)
		if !errors.Is(err, listErr) {
			t.Fatalf("expected list error, got %v", err)
		}
	})

	t.Run("batch error", func(t *testing.T) {
		t.Parallel()

		batchErr := errors.New("batch failed")

		_, err := (&Provider{bulk: &fakeBulkClient{secrets: secrets, batchErr: batchErr}}).GetSecretsByPath(t.Context(), "p/", false)
		if !errors.Is(err, batchErr) {
			t.Fatalf("expected batch error, got %v", err)
		}
	})

	t.Run("per secret error does not leak values", func(t *testing.T) {
		t.Parallel()

		fake := &fakeBulkClient{
			secrets: secrets,
			batchErrors: []smtypes.APIErrorType{
				{SecretId: aws.String("p/a"), ErrorCode: aws.String("AccessDeniedException"), Message: aws.String("denied")},
			},
		}

		_, err := (&Provider{bulk: fake}).GetSecretsByPath(t.Context(), "p/", false)
		if err == nil || !strings.Contains(err.Error(), "AccessDeniedException") {
			t.Fatalf("expected access denied error, got %v", err)
		}

		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("error must not contain secret values: %v", err)
		}
	})
}
