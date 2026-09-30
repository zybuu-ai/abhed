package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// canonical returns a JSON value in the form the chain hashes: object keys
// sorted by their bytes, a later duplicate key replacing an earlier one, no
// whitespace, numbers exactly as written, strings escaped as encoding/json
// escapes them with HTML escaping off. It is idempotent, so a payload read
// back from the record is already canonical.
func canonical(data []byte) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return []byte("null"), nil
	}
	// Decoding would put U+FFFD in place of invalid bytes, and two keys that
	// differed only there would become one: refused rather than lost.
	if !utf8.Valid(data) {
		return nil, errors.New("the JSON is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return encode(v)
}

// encode writes v as canonical JSON. Maps are written with sorted keys by
// encoding/json itself; structs in the order of their fields.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
