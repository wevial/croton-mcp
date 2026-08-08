package strictjson

import (
	"strings"
	"testing"
)

func TestDecodeObjectRejectsExcessiveNesting(t *testing.T) {
	t.Parallel()

	data := []byte(`{"value":` + strings.Repeat("[", 64) + `0` + strings.Repeat("]", 64) + `}`)
	var target map[string]any
	if DecodeObject(data, len(data), &target) {
		t.Fatal("deeply nested JSON object was accepted")
	}
}

func TestDecodeObjectAcceptsBoundedNestedObject(t *testing.T) {
	t.Parallel()

	data := []byte(`{"value":{"items":[1,2,3]}}`)
	var target map[string]any
	if !DecodeObject(data, len(data), &target) {
		t.Fatal("bounded nested JSON object was rejected")
	}
}

func TestValidateObjectAllowsCaseDistinctOpenMapKeys(t *testing.T) {
	t.Parallel()

	data := []byte(`{"experimental":{"VendorFeature":{},"vendorfeature":{}}}`)
	if !ValidateObject(data, len(data)) {
		t.Fatal("case-distinct keys were rejected by structural validation")
	}
}

func TestValidateObjectRejectsExactDuplicateKeys(t *testing.T) {
	t.Parallel()

	data := []byte(`{"method":"resources/list","method":"ping"}`)
	if ValidateObject(data, len(data)) {
		t.Fatal("exact duplicate keys were accepted")
	}
}

func TestDecodeObjectStillRejectsCaseFoldedAliases(t *testing.T) {
	t.Parallel()

	for _, data := range [][]byte{
		[]byte(`{"value":1,"Value":2}`),
		[]byte(`{"sender":"safe","ſender":"attacker"}`),
	} {
		var target map[string]any
		if DecodeObject(data, len(data), &target) {
			t.Fatalf("case-folded aliases were accepted by schema decoding: %s", data)
		}
	}
}
