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
	"sync"
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err != nil {
		err = fmt.Errorf("invalid JSON: %w", err)
	} else if _, tok := dec.Token(); tok != io.EOF {
		err = errors.New("invalid JSON: data after the value")
	}
	// A duplicate key is reported before any decode error. Without one,
	// data is exactly one valid value, which rawKeysUnique may judge.
	if err != nil || !rawKeysUnique(data) {
		if derr := walkDuplicateKeys(data); derr != nil {
			return derr
		}
	}
	if err != nil {
		return err
	}
	// Presence: a missing key or a null must never decode to a zero value
	// that reads as success (an exit code 0, a false "resumed").
	if fieldsPresent(data, reflect.TypeOf(v)) {
		return nil
	}
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

// fieldsPresent reports, for data holding exactly one valid JSON value
// without duplicate keys, that requireFields accepts it against t. It walks
// the raw bytes instead of decoding them; an escaped key, or anything
// requireFields might refuse, answers false, leaving the verdict and its
// wording to requireFields.
func fieldsPresent(data []byte, t reflect.Type) bool {
	_, ok := presentValue(data, skipSpace(data, 0), t)
	return ok
}

// presentValue checks the value starting at data[i] against t, returning
// the index just past it.
func presentValue(data []byte, i int, t reflect.Type) (int, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	ok := true
	switch t.Kind() {
	case reflect.Struct:
		if data[i] != '{' {
			return i, false
		}
		fields := fieldsOf(t)
		var seenBuf [64]bool
		seen := seenBuf[:]
		if len(fields) > len(seen) {
			seen = make([]bool, len(fields))
		}
		for i = skipSpace(data, i+1); data[i] != '}'; {
			j, escaped := stringEnd(data, i)
			if escaped {
				return i, false
			}
			key := data[i+1 : j-1]
			i = skipSpace(data, skipSpace(data, j)+1)
			k := slices.IndexFunc(fields, func(f fieldInfo) bool { return f.name == string(key) })
			switch {
			case k < 0:
				i = skipValue(data, i)
			case isNull(data, i):
				if fields[k].typ.Kind() != reflect.Pointer {
					return i, false
				}
				seen[k], i = true, i+len("null")
			default:
				seen[k] = true
				if i, ok = presentValue(data, i, fields[k].typ); !ok {
					return i, false
				}
			}
			if i = skipSpace(data, i); data[i] == ',' {
				i = skipSpace(data, i+1)
			}
		}
		for k, f := range fields {
			if !seen[k] && !f.omitempty {
				return i, false
			}
		}
		return i + 1, true
	case reflect.Slice, reflect.Array:
		if data[i] != '[' {
			return i, false
		}
		for i = skipSpace(data, i+1); data[i] != ']'; {
			if isNull(data, i) {
				if t.Elem().Kind() != reflect.Pointer {
					return i, false
				}
				i += len("null")
			} else if i, ok = presentValue(data, i, t.Elem()); !ok {
				return i, false
			}
			if i = skipSpace(data, i); data[i] == ',' {
				i = skipSpace(data, i+1)
			}
		}
		return i + 1, true
	}
	return skipValue(data, i), true
}

// fieldInfo is one JSON-encoded struct field as requireFields sees it.
type fieldInfo struct {
	name      string
	omitempty bool
	typ       reflect.Type
}

// structFields caches fieldsOf per struct type.
var structFields sync.Map

// fieldsOf lists t's JSON-encoded fields in order.
func fieldsOf(t reflect.Type) []fieldInfo {
	if fs, ok := structFields.Load(t); ok {
		return fs.([]fieldInfo)
	}
	var fs []fieldInfo
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if name, omitempty := jsonField(f); name != "" {
			fs = append(fs, fieldInfo{name, omitempty, f.Type})
		}
	}
	structFields.Store(t, fs)
	return fs
}

// skipSpace returns the index of the first non-space byte from i.
func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// stringEnd returns the index just past the string starting at data[i]
// and whether it holds an escape.
func stringEnd(data []byte, i int) (int, bool) {
	escaped := false
	for i++; data[i] != '"'; i++ {
		if data[i] == '\\' {
			escaped = true
			i++
		}
	}
	return i + 1, escaped
}

// isNull reports that the value at data[i] is the literal null.
func isNull(data []byte, i int) bool {
	return bytes.HasPrefix(data[i:], []byte("null"))
}

// skipValue returns the index just past the valid JSON value at data[i].
func skipValue(data []byte, i int) int {
	depth := 0
	for {
		switch data[i] {
		case '"':
			i, _ = stringEnd(data, i)
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			i++
		default:
			for i < len(data) && !strings.ContainsRune(",}] \t\n\r", rune(data[i])) {
				i++
			}
		}
		if depth == 0 {
			return i
		}
		i = skipSpace(data, i)
		if data[i] == ',' || data[i] == ':' {
			i++
		}
		i = skipSpace(data, i)
	}
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
// repeated within one object, at any depth. Valid UTF-8 JSON whose keys
// carry no escape and repeat no bytes within an object is accepted without
// the walk: its decoded keys are exactly its raw keys. Anything else is
// judged (and its error worded) by the walk.
func checkDuplicateKeys(data []byte) error {
	if utf8.Valid(data) && json.Valid(data) && rawKeysUnique(data) {
		return nil
	}
	return walkDuplicateKeys(data)
}

// walkDuplicateKeys is checkDuplicateKeys by a walk over every token.
func walkDuplicateKeys(data []byte) error {
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

// rawKeysUnique reports, for valid JSON data, that no object key contains
// an escape and no object repeats a key's raw bytes.
func rawKeysUnique(data []byte) bool {
	// Small objects are searched linearly; a large one switches to a set,
	// keeping the scan linear in the data.
	const linearKeys = 32
	type object struct {
		keys      [][]byte
		set       map[string]bool
		expectKey bool
	}
	var stack []*object
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			j, escaped := i+1, false
			for data[j] != '"' {
				if data[j] == '\\' {
					escaped = true
					j++
				}
				j++
			}
			if n := len(stack); n > 0 && stack[n-1] != nil && stack[n-1].expectKey {
				o, key := stack[n-1], data[i+1:j]
				if escaped || o.set[string(key)] || slices.ContainsFunc(o.keys, func(k []byte) bool { return bytes.Equal(k, key) }) {
					return false
				}
				o.expectKey = false
				if o.set != nil {
					o.set[string(key)] = true
				} else if o.keys = append(o.keys, key); len(o.keys) == linearKeys {
					o.set = map[string]bool{}
					for _, k := range o.keys {
						o.set[string(k)] = true
					}
					o.keys = nil
				}
			}
			i = j
		case '{':
			stack = append(stack, &object{expectKey: true})
		case '[':
			stack = append(stack, nil)
		case '}', ']':
			stack = stack[:len(stack)-1]
		case ',':
			if n := len(stack); n > 0 && stack[n-1] != nil {
				stack[n-1].expectKey = true
			}
		}
	}
	return true
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
