// Package strictjson decodes bounded, unambiguous JSON objects.
package strictjson

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

const maxNestingDepth = 32

// DecodeObject decodes exactly one JSON object into target. It rejects
// oversized input, non-object top-level values, null values, duplicate or
// case-folded-alias keys at any depth, unknown target fields, and trailing
// values.
func DecodeObject(data []byte, maximumBytes int, target any) bool {
	if len(data) == 0 {
		data = []byte(`{}`)
	}
	if !validateObject(data, maximumBytes, true, true) {
		return false
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil
}

// ValidateObject reports whether data contains exactly one bounded JSON
// object with no exact duplicate keys. Unlike DecodeObject it permits null
// values and case-distinct keys because protocol-defined open maps may use
// case-sensitive vendor identifiers.
func ValidateObject(data []byte, maximumBytes int) bool {
	return validateObject(data, maximumBytes, false, false)
}

func validateObject(data []byte, maximumBytes int, rejectNull, foldAliases bool) bool {
	if len(data) == 0 || maximumBytes <= 0 || len(data) > maximumBytes {
		return false
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	token, err := decoder.Token()
	delimiter, ok := token.(json.Delim)
	if err != nil || !ok || delimiter != '{' || !consumeObject(decoder, 1, rejectNull, foldAliases) {
		return false
	}

	_, err = decoder.Token()
	return err == io.EOF
}

func consumeObject(decoder *json.Decoder, depth int, rejectNull, foldAliases bool) bool {
	if depth > maxNestingDepth {
		return false
	}

	seen := make(map[string]struct{})
	var foldedKeys []string
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		if foldAliases {
			for _, existing := range foldedKeys {
				if strings.EqualFold(existing, key) {
					return false
				}
			}
			foldedKeys = append(foldedKeys, key)
		}

		if !consumeValue(decoder, depth, rejectNull, foldAliases) {
			return false
		}
	}

	token, err := decoder.Token()
	delimiter, ok := token.(json.Delim)
	return err == nil && ok && delimiter == '}'
}

func consumeValue(decoder *json.Decoder, depth int, rejectNull, foldAliases bool) bool {
	token, err := decoder.Token()
	if err != nil || (rejectNull && token == nil) {
		return false
	}

	delimiter, composite := token.(json.Delim)
	if !composite {
		return true
	}

	switch delimiter {
	case '{':
		return consumeObject(decoder, depth+1, rejectNull, foldAliases)
	case '[':
		if depth >= maxNestingDepth {
			return false
		}
		for decoder.More() {
			if !consumeValue(decoder, depth+1, rejectNull, foldAliases) {
				return false
			}
		}

		token, err = decoder.Token()
		closing, ok := token.(json.Delim)
		return err == nil && ok && closing == ']'
	default:
		return false
	}
}
