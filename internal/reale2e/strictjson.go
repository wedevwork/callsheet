package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// Strict JSON (design 12a-real-e2e, Evidence and checker): every evidence
// and protocol document is exactly one JSON value of valid UTF-8 with no
// duplicate key at any depth, no field unknown to its schema and nothing
// after it. Size bounds are checked by the caller before parsing.

// errDuplicateKey reports a repeated object key.
var errDuplicateKey = errors.New("duplicate JSON object key")

// decodeStrict decodes data into v under the strict rules.
func decodeStrict(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON is not valid UTF-8")
	}
	if err := checkDuplicateKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("invalid JSON: data after the value")
	}
	// Presence: a missing key or a null must never decode to a zero value
	// that reads as success (an exit code 0, a false "resumed").
	var tree any
	gd := json.NewDecoder(bytes.NewReader(data))
	gd.UseNumber()
	if err := gd.Decode(&tree); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return requireFields(tree, reflect.TypeOf(v), "")
}

// requireFields checks that the decoded generic value carries every field
// of t: each JSON field without omitempty must be present, and only
// pointer fields (an explicitly nullable proof, such as an unobserved
// exit) may be null; slices, arrays and nested structs are checked
// element by element.
func requireFields(v any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid JSON: %s must be an object", nameOf(path))
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, omitempty := jsonField(f)
			if name == "" {
				continue
			}
			raw, present := m[name]
			switch {
			case !present && omitempty:
				continue
			case !present:
				return fmt.Errorf("invalid JSON: required field %s is missing", nameOf(path+"."+name))
			case raw == nil && f.Type.Kind() == reflect.Pointer:
				continue
			case raw == nil:
				return fmt.Errorf("invalid JSON: field %s must not be null", nameOf(path+"."+name))
			}
			if err := requireFields(raw, f.Type, path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("invalid JSON: %s must be an array", nameOf(path))
		}
		for i, e := range arr {
			p := fmt.Sprintf("%s[%d]", path, i)
			if e == nil && t.Elem().Kind() != reflect.Pointer {
				return fmt.Errorf("invalid JSON: element %s must not be null", nameOf(p))
			}
			if e != nil {
				if err := requireFields(e, t.Elem(), p); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// jsonField returns a struct field's JSON name ("" when not encoded) and
// whether it is omitempty.
func jsonField(f reflect.StructField) (string, bool) {
	if !f.IsExported() {
		return "", false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, slices.Contains(strings.Split(opts, ","), "omitempty")
}

// nameOf renders a field path for diagnostics.
func nameOf(path string) string {
	if p := strings.TrimPrefix(path, "."); p != "" {
		return p
	}
	return "the document"
}

// checkDuplicateKeys walks every token of one JSON value and rejects a key
// repeated within one object, at any depth.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walkValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("invalid JSON: data after the value")
	}
	return nil
}

// walkValue consumes one value from dec.
func walkValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			k, _ := kt.(string)
			if seen[k] {
				return fmt.Errorf("%w %q", errDuplicateKey, k)
			}
			seen[k] = true
			if err := walkValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkValue(dec); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// encodeJSON renders v as indented JSON with a final newline (evidence
// files are human-inspectable).
func encodeJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
