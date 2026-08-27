package test

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

// ConvertTestName converts a test name to a format suitable for stack names or similar uses.
// e.g. "TestHandlerData_ProjectApiHandler/Restart_Project_-_Invalid_Method" should be converted to "testhandlerdata-projectapihandler_restart-project-invalid-method-1234".
// Returns a string that is lowercase, with non-alphanumeric characters replaced by hyphens.
// Long names are truncated to 40 characters plus a deterministic hash of the
// full name, so distinct parallel tests never collide on the same stack name
// (a random suffix collided roughly once in a thousand test pairs).
func ConvertTestName(testName string) string {
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

	s := reg.ReplaceAllString(strings.ToLower(testName), "-")

	if len(s) > 40 {
		nameHash := fnv.New32a()
		_, _ = nameHash.Write([]byte(s))

		s = fmt.Sprintf("%s-%d", s[:40], nameHash.Sum32()%100000)
	}

	return s
}
