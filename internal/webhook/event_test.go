package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestIsBranchOrTagDeletionEvent(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		provider       ScmProvider
		eventHeader    string
		payload        ParsedPayload
		expectedResult bool
		expectedError  bool
	}{
		// GitHub tests
		{
			name:        "GitHub branch deletion event",
			provider:    Github,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "branch",
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "GitHub tag deletion event",
			provider:    Github,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "tag",
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "GitHub push event with zero SHA (branch deletion)",
			provider:    Github,
			eventHeader: "push",
			payload: ParsedPayload{
				Before: "abc123",
				After:  git.ZeroSHA,
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "GitHub push event (not a deletion)",
			provider:    Github,
			eventHeader: "push",
			payload: ParsedPayload{
				Before: "abc123",
				After:  "def456",
			},
			expectedResult: false,
			expectedError:  false,
		},
		{
			name:        "GitHub delete event with invalid ref_type",
			provider:    Github,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "unknown",
			},
			expectedResult: false,
			expectedError:  false,
		},
		// Gitea tests
		{
			name:        "Gitea branch deletion event",
			provider:    Gitea,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "branch",
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "Gitea tag deletion event",
			provider:    Gitea,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "tag",
			},
			expectedResult: true,
			expectedError:  false,
		},
		// Gogs tests
		{
			name:        "Gogs branch deletion event",
			provider:    Gogs,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "branch",
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "Gogs tag deletion event",
			provider:    Gogs,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "tag",
			},
			expectedResult: true,
			expectedError:  false,
		},
		// Forgejo tests
		{
			name:        "Forgejo branch deletion event",
			provider:    Forgejo,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "branch",
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "Forgejo tag deletion event",
			provider:    Forgejo,
			eventHeader: "delete",
			payload: ParsedPayload{
				RefType: "tag",
			},
			expectedResult: true,
			expectedError:  false,
		},
		// GitLab tests
		{
			name:        "GitLab branch deletion with Push Hook",
			provider:    Gitlab,
			eventHeader: "Push Hook",
			payload: ParsedPayload{
				After:     git.ZeroSHA,
				CommitSHA: "",
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "GitLab tag deletion with Tag Push Hook",
			provider:    Gitlab,
			eventHeader: "Tag Push Hook",
			payload: ParsedPayload{
				After:     git.ZeroSHA,
				CommitSHA: "",
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:        "GitLab push event (not a deletion)",
			provider:    Gitlab,
			eventHeader: "Push Hook",
			payload: ParsedPayload{
				After:     "abc123def456",
				CommitSHA: "abc123def456",
			},
			expectedResult: false,
			expectedError:  false,
		},
		{
			name:        "GitLab deletion with non-null checkout_sha",
			provider:    Gitlab,
			eventHeader: "Push Hook",
			payload: ParsedPayload{
				After:     git.ZeroSHA,
				CommitSHA: "abc123",
			},
			expectedResult: false,
			expectedError:  false,
		},
		{
			name:        "GitLab wrong event type",
			provider:    Gitlab,
			eventHeader: "Merge Request Hook",
			payload: ParsedPayload{
				After:     git.ZeroSHA,
				CommitSHA: "",
			},
			expectedResult: false,
			expectedError:  false,
		},
		// Error cases
		{
			name:           "Missing event header",
			provider:       Github,
			eventHeader:    "",
			payload:        ParsedPayload{},
			expectedResult: false,
			expectedError:  true,
		},
		{
			name:           "Unknown provider",
			provider:       Unknown,
			eventHeader:    "push",
			payload:        ParsedPayload{},
			expectedResult: false,
			expectedError:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a test request
			r := httptest.NewRequest(http.MethodPost, "/webhook", nil)

			// Set the event header if provided
			if tc.eventHeader != "" {
				headerName := ScmProviderEventHeaders[tc.provider]
				r.Header.Set(headerName, tc.eventHeader)
			}

			// Call the function
			result, err := IsBranchOrTagDeletionEvent(r, tc.payload, tc.provider)

			// Check error expectation
			if tc.expectedError {
				if err == nil {
					t.Errorf("expected an error, but got none")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check result
			if result != tc.expectedResult {
				t.Errorf("expected result to be %v, got %v", tc.expectedResult, result)
			}
		})
	}
}
