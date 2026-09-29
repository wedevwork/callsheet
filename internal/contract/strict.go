package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"
)

// strictUnmarshaler is implemented by types that validate their own JSON
// value inside a strictly decoded document (iteration 05).
type strictUnmarshaler interface {
	unmarshalStrict(raw json.RawMessage, what string) error
}

// decodeStrict decodes raw into v (a non-nil pointer) with the contract's
// object machinery: every object is one JSON object without duplicate or
// unknown keys; every struct field is required unless its json tag says
// omitempty; null is accepted only for pointer fields; slices must be
// arrays (never null); strings must be valid UTF-8 with paired surrogate
// escapes; integers are exact; booleans are JSON booleans; []byte fields
// are canonical standard padded base64. Types implementing
// strictUnmarshaler decode themselves.
func decodeStrict(raw json.RawMessage, v any, what string) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errInvalid("%s: internal decode target", what)
	}
	return decodeValue(bytes.TrimSpace(raw), rv.Elem(), what)
}

// DecodeStrict is decodeStrict for callers outside the contract package
// (iteration 07a): the MCP server decodes its tool argument DTOs with it
// instead of duplicating the integer, unknown-key, duplicate-key and
// string rules. v must be a non-nil pointer to a struct of the supported
// field kinds; what names the value in the invalid_argument message. A
// pointer field accepts null, so a caller that forbids null (MCP optional
// arguments are absent, never null) checks it before decoding.
func DecodeStrict(raw []byte, v any, what string) error { return decodeStrict(raw, v, what) }

var (
	strictType = reflect.TypeFor[strictUnmarshaler]()
	bytesType  = reflect.TypeFor[[]byte]()
	rawType    = reflect.TypeFor[json.RawMessage]()
)

func decodeValue(raw []byte, v reflect.Value, what string) error {
	if v.CanAddr() && v.Addr().Type().Implements(strictType) {
		if isNull(raw) && v.Kind() != reflect.Pointer {
			return errInvalid("%s must not be null", what)
		}
		return v.Addr().Interface().(strictUnmarshaler).unmarshalStrict(raw, what)
	}
	if v.Type() == rawType {
		if len(raw) == 0 {
			return errInvalid("%s is missing", what)
		}
		v.SetBytes(append([]byte(nil), raw...))
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if isNull(raw) {
			v.SetZero()
			return nil
		}
		p := reflect.New(v.Type().Elem())
		if err := decodeValue(raw, p.Elem(), what); err != nil {
			return err
		}
		v.Set(p)
		return nil
	case reflect.Struct:
		return decodeStruct(raw, v, what)
	case reflect.Slice:
		if v.Type() == bytesType {
			b, err := strictBase64(raw, what)
			if err != nil {
				return err
			}
			v.SetBytes(b)
			return nil
		}
		elems, err := array(raw, what)
		if err != nil {
			return err
		}
		s := reflect.MakeSlice(v.Type(), len(elems), len(elems))
		for i, e := range elems {
			if err := decodeValue(bytes.TrimSpace(e), s.Index(i), what); err != nil {
				return err
			}
		}
		v.Set(s)
		return nil
	case reflect.String:
		s, err := strictString(raw, what)
		if err != nil {
			return err
		}
		v.SetString(s)
		return nil
	case reflect.Int, reflect.Int64:
		n, ok := ParseInteger(string(raw))
		if !ok {
			return errInvalid("%s must be an integer", what)
		}
		v.SetInt(int64(n))
		return nil
	case reflect.Bool:
		switch string(raw) {
		case "true":
			v.SetBool(true)
		case "false":
			v.SetBool(false)
		default:
			return errInvalid("%s must be a boolean", what)
		}
		return nil
	}
	return errInvalid("%s: unsupported field type", what)
}

// fieldName returns a struct field's JSON name and whether it is optional.
func fieldName(f reflect.StructField) (string, bool, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" || !f.IsExported() {
		return "", false, false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, opts == "omitempty", true
}

func decodeStruct(raw []byte, v reflect.Value, what string) error {
	o, err := decodeObject(raw, what)
	if err != nil {
		return err
	}
	return decodeFields(o, v, what)
}

// decodeFields decodes an already split object into the struct v.
func decodeFields(o object, v reflect.Value, what string) error {
	t := v.Type()
	known := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		name, optional, ok := fieldName(t.Field(i))
		if !ok {
			continue
		}
		known[name] = i
		val, present := o.raw[name]
		if !present {
			if optional {
				continue
			}
			return errInvalid("%s lacks the required field %q", what, name)
		}
		if err := decodeValue(bytes.TrimSpace(val), v.Field(i), what+" field "+strconvQuote(name)); err != nil {
			return err
		}
	}
	for _, k := range o.keys {
		if _, ok := known[k]; !ok {
			return errInvalid("%s has an unknown field %q", what, safeKey(k))
		}
	}
	return nil
}

// reflectValue is the addressable struct a pointer v points to.
func reflectValue(v any) reflect.Value { return reflect.ValueOf(v).Elem() }

func strconvQuote(s string) string { return `"` + s + `"` }

// strictString decodes one JSON string: the raw token must be valid UTF-8
// and every \u surrogate escape must be paired, so encoding/json never
// substitutes a replacement character for invalid input.
func strictString(raw []byte, what string) (string, error) {
	if len(raw) < 2 || raw[0] != '"' {
		return "", errInvalid("%s must be a string", what)
	}
	if !utf8.Valid(raw) || !pairedSurrogates(raw) {
		return "", errInvalid("%s is not valid UTF-8", what)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", errInvalid("%s must be a string", what)
	}
	return s, nil
}

// JSONString strictly decodes one JSON string token: the raw bytes must
// be valid UTF-8 and every \u surrogate escape paired, so no replacement
// character is ever substituted for invalid input (a literal or escaped
// U+FFFD is ordinary text).
func JSONString(raw []byte) (string, bool) {
	s, err := strictString(raw, "value")
	return s, err == nil
}

// pairedSurrogates reports whether every UTF-16 surrogate escape in the
// JSON string token raw is a high surrogate immediately followed by a low
// one.
func pairedSurrogates(raw []byte) bool {
	hex4 := func(b []byte) (rune, bool) {
		if len(b) < 4 {
			return 0, false
		}
		var r rune
		for _, c := range b[:4] {
			r <<= 4
			switch {
			case c >= '0' && c <= '9':
				r |= rune(c - '0')
			case c >= 'a' && c <= 'f':
				r |= rune(c-'a') + 10
			case c >= 'A' && c <= 'F':
				r |= rune(c-'A') + 10
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if i+1 >= len(raw) {
			return false
		}
		if raw[i+1] != 'u' {
			i++
			continue
		}
		r, ok := hex4(raw[i+2:])
		if !ok {
			return false
		}
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return false
		case r >= 0xD800 && r <= 0xDBFF:
			j := i + 6
			if j+1 >= len(raw) || raw[j] != '\\' || raw[j+1] != 'u' {
				return false
			}
			lo, ok := hex4(raw[j+2:])
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return false
			}
			i = j + 5
		default:
			i += 5
		}
	}
	return true
}

// strictBase64 decodes a canonical standard padded base64 JSON string.
func strictBase64(raw []byte, what string) ([]byte, error) {
	s, err := strictString(raw, what)
	if err != nil {
		return nil, err
	}
	// Canonical: padded (a multiple of four), no line breaks (which the
	// decoder would skip) and zero padding bits (Strict).
	if len(s)%4 != 0 || strings.ContainsAny(s, "\r\n") {
		return nil, errInvalid("%s must be canonical standard padded base64", what)
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, errInvalid("%s must be canonical standard padded base64", what)
	}
	return b, nil
}
