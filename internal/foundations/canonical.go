// Package foundations holds the Go mirror of the Step 89B canonical forms.
//
// The Python modules and this package sign and verify the same bytes. The
// canonical rule is: sorted object keys, tight separators, UTF-8 strings, no
// HTML escaping, and no NaN or Infinity. The shared vectors under
// spec/sparknet/foundations-vectors.json pin the two implementations together.
package foundations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotJSON is returned when a value cannot be represented as JSON.
var ErrNotJSON = errors.New("foundations: value is not canonical JSON")

// CanonicalJSON returns the canonical UTF-8 JSON encoding of value.
//
// It matches Python's json.dumps(sort_keys=True, separators=(",", ":"),
// ensure_ascii=False, allow_nan=False) for the value shapes in the vectors:
// objects, arrays, strings, integers, finite floats, booleans and null.
func CanonicalJSON(value any) ([]byte, error) {
	// Marshal first so struct tags and custom marshallers are honoured, then
	// decode into a generic value and encode again. The second pass is what
	// sorts object keys: encoding/json sorts map keys, but it preserves struct
	// field declaration order. UseNumber keeps large integers exact.
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var normal any
	if err := decoder.Decode(&normal); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normal); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// CanonicalString returns the canonical JSON as a string.
func CanonicalString(value any) (string, error) {
	raw, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
