package clone

import "testing"

func TestPointer(t *testing.T) {
	t.Parallel()

	value := 1
	cloned := Pointer(&value)
	*cloned = 2

	if value != 1 {
		t.Fatalf("original value = %d, want 1", value)
	}

	if Pointer[int](nil) != nil {
		t.Fatal("expected nil pointer clone")
	}
}

func TestStringAnyMap(t *testing.T) {
	t.Parallel()

	source := map[string]any{
		"map": map[string]any{
			"value": "original",
		},
		"slice": []any{
			map[string]any{
				"value": "original",
			},
		},
	}

	cloned := StringAnyMap(source)
	cloned["map"].(map[string]any)["value"] = "changed"
	cloned["slice"].([]any)[0].(map[string]any)["value"] = "changed"

	if got := source["map"].(map[string]any)["value"]; got != "original" {
		t.Fatalf("original map value = %q, want %q", got, "original")
	}

	if got := source["slice"].([]any)[0].(map[string]any)["value"]; got != "original" {
		t.Fatalf("original slice value = %q, want %q", got, "original")
	}

	if StringAnyMap(nil) != nil {
		t.Fatal("expected nil map clone")
	}
}
