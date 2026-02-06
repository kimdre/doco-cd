package test

import (
	"regexp"
	"strings"
)

// ConvertTestName converts a test name to a format suitable for stack names or similar uses.
// e.g. "TestHandlerData_ProjectApiHandler/Restart_Project_-_Invalid_Method" should be converted to "testhandlerdata-projectapihandler_restart-project-invalid-method".
func ConvertTestName(testName string) string {
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
	return reg.ReplaceAllString(strings.ToLower(testName), "-")
}
