package git_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestFormatGitErrorMessage_DecodesLocalizedAzureDevOpsError(t *testing.T) {
	t.Parallel()

	errMsg := `authentication required (or check https://dev.azure.com for error details)
remote: {"message":"\u041d\u0435\u0434\u043e\u043f\u0443\u0441\u0442\u0438\u043c\u0430\u044f \u0432\u0435\u0442\u043a\u0430","typeName":"Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException","errorCode":12345}`
	err := errors.New(errMsg)

	formatted := git.FormatGitErrorMessage(err)

	if !strings.Contains(formatted, "Недопустимая ветка") {
		t.Errorf("formatted error should contain decoded message, got: %s", formatted)
	}

	if !strings.Contains(formatted, "typeName=Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException") {
		t.Errorf("formatted error should contain typeName, got: %s", formatted)
	}

	if !strings.Contains(formatted, "errorCode=12345") {
		t.Errorf("formatted error should contain errorCode, got: %s", formatted)
	}

	if strings.Contains(formatted, "\\u041d") {
		t.Errorf("formatted error should not contain escaped unicode, got: %s", formatted)
	}
}

func TestFormatGitErrorMessage_DecodesJSONStringError(t *testing.T) {
	t.Parallel()

	errMsg := `authentication required: "\u041d\u0435\u0434\u043e\u0441\u0442\u0443\u043f\u043d\u043e"`
	err := errors.New(errMsg)

	formatted := git.FormatGitErrorMessage(err)

	if !strings.Contains(formatted, "Недоступно") {
		t.Errorf("formatted error should contain decoded JSON string message, got: %s", formatted)
	}

	if strings.Contains(formatted, "\\u041d") {
		t.Errorf("formatted error should not contain escaped unicode, got: %s", formatted)
	}
}

func TestFormatGitErrorMessage_ReturnsOriginalWhenNoJSON(t *testing.T) {
	t.Parallel()

	errMsg := "simple error message without JSON"
	err := errors.New(errMsg)

	formatted := git.FormatGitErrorMessage(err)

	if formatted != errMsg {
		t.Errorf("formatted error should match original when no JSON found, got: %s", formatted)
	}
}

func TestFormatGitErrorMessage_NilError(t *testing.T) {
	t.Parallel()

	formatted := git.FormatGitErrorMessage(nil)

	if formatted != "" {
		t.Errorf("formatted nil error should be empty string, got: %s", formatted)
	}
}
