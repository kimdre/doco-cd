package clone

import (
	"net/netip"
	"testing"
)

type withUnexported struct {
	Exported   []string
	unexported string
}

func TestDeepPreservesUnexportedFields(t *testing.T) {
	t.Parallel()

	// netip.Addr consists solely of unexported fields. Zeroing them during a
	// clone would silently turn a valid address into an invalid one.
	addr := netip.MustParseAddr("10.0.0.1")
	src := struct {
		Nameservers []netip.Addr
		Nested      withUnexported
	}{
		Nameservers: []netip.Addr{addr},
		Nested:      withUnexported{Exported: []string{"a"}, unexported: "keep"},
	}

	cloned := New(&src)

	if got := cloned.Nameservers[0]; got != addr {
		t.Fatalf("cloned address = %v, want %v", got, addr)
	}

	if cloned.Nested.unexported != "keep" {
		t.Fatalf("unexported field = %q, want %q", cloned.Nested.unexported, "keep")
	}

	// Exported reference fields must still be independent of the source.
	cloned.Nested.Exported[0] = "changed"
	cloned.Nameservers[0] = netip.MustParseAddr("192.168.0.1")

	if src.Nested.Exported[0] != "a" {
		t.Fatalf("source slice = %q, want %q", src.Nested.Exported[0], "a")
	}

	if src.Nameservers[0] != addr {
		t.Fatalf("source address = %v, want %v", src.Nameservers[0], addr)
	}
}

func TestDeepCopiesNestedReferences(t *testing.T) {
	t.Parallel()

	value := "original"
	src := struct {
		Map     map[string]any
		Slice   []map[string]string
		Pointer *string
	}{
		Map:     map[string]any{"nested": map[string]any{"key": "original"}},
		Slice:   []map[string]string{{"key": "original"}},
		Pointer: &value,
	}

	cloned := New(&src)
	cloned.Map["nested"].(map[string]any)["key"] = "changed"
	cloned.Slice[0]["key"] = "changed"
	*cloned.Pointer = "changed"

	if got := src.Map["nested"].(map[string]any)["key"]; got != "original" {
		t.Fatalf("source map value = %q, want %q", got, "original")
	}

	if got := src.Slice[0]["key"]; got != "original" {
		t.Fatalf("source slice value = %q, want %q", got, "original")
	}

	if value != "original" {
		t.Fatalf("source pointer value = %q, want %q", value, "original")
	}
}

func TestNewNilSource(t *testing.T) {
	t.Parallel()

	if New[withUnexported](nil) != nil {
		t.Fatal("expected nil clone for nil source")
	}
}

func TestDeepNilArguments(t *testing.T) {
	t.Parallel()

	dst := withUnexported{unexported: "keep"}

	Deep(&dst, nil)
	Deep[withUnexported](nil, &dst)

	if dst.unexported != "keep" {
		t.Fatalf("dst was modified: %q", dst.unexported)
	}
}

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
