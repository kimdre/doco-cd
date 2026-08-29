package test

import (
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
)

// ConvertTestName converts a test name to a format suitable for stack names or similar uses.
// e.g. "TestHandlerData_ProjectApiHandler/Restart_Project" becomes
// "testhandlerdata_projectapihandler-restar-1123893393".
// Returns a string that is lowercase, with non-alphanumeric characters replaced by hyphens.
// Long names are truncated to 40 characters plus a deterministic FNV-32 hash
// of the full name, so distinct parallel tests are protected against colliding
// on the same stack name (a random suffix collided roughly once in a thousand
// test pairs and produced flaky Compose name conflicts).
func ConvertTestName(testName string) string {
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

	s := reg.ReplaceAllString(strings.ToLower(testName), "-")

	if len(s) > 40 {
		nameHash := fnv.New32a()
		_, _ = nameHash.Write([]byte(s))

		s = s[:40] + "-" + strconv.FormatUint(uint64(nameHash.Sum32()), 10)
	}

	return s
}
