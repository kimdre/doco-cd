package test

import (
	"regexp"
	"strings"
)

// ConvertTestName converts a test name to a format suitable for stack names or similar uses.
// e.g. "TestHandlerData_ProjectApiHandler/Restart_Project_-_Invalid_Method" should be converted to "testhandlerdata-projectapihandler_restart-project-invalid-method".
// Returns a string that is lowercase, with non-alphanumeric characters replaced by hyphens, and truncated to 63 characters if necessary.
func ConvertTestName(testName string) string {
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

	s := reg.ReplaceAllString(strings.ToLower(testName), "-")
	if len(s) > 63 {
		s = s[:63]
	}

	return s
}
