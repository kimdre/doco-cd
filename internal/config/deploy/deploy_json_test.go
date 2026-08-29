package deploy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigJSONExcludesInternalState(t *testing.T) {
	config := Config{}
	config.Internal.Environment = map[string]string{"SECRET": "value"}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "Internal") || strings.Contains(string(data), "SECRET") {
		t.Fatalf("JSON exposes internal state: %s", data)
	}
}
