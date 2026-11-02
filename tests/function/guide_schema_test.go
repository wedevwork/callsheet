package function

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// Test-only helpers of design 12-4-user-guide: a strict JSON decoder and a
// validator of the closed schema dialect that the MCP tools/list schemas use.
// Neither is a general JSON Schema engine, and an unknown keyword is an error
// rather than something to ignore, so a new keyword in a real schema fails
// visibly and needs an explicit amendment here.

// schemaEnforced are the keywords the walker enforces; schemaAnnotations are
// inert and never change the instance.
var (
	schemaEnforced = []string{"type", "properties", "required", "additionalProperties", "items", "enum", "pattern", "minLength", "maxLength",
		"minItems", "maxItems", "uniqueItems", "minProperties", "minimum", "maximum"}
	schemaAnnotations = []string{"description", "default"}
	schemaTypes       = []string{"object", "array", "string", "integer", "number", "boolean", "null"}
)

// validateAgainstSchema checks schema's whole declaration, including branches
// the instance does not reach, and then value against it. The value and schema
// come from decodeStrictJSON (numbers are json.Number); nothing is coerced and
// no default is inserted.
func validateAgainstSchema(schema map[string]any, value any) error {
	if err := checkSchemaDeclaration("$", schema); err != nil {
		return err
	}
	return checkSchemaInstance("$", schema, value)
}

func schemaBadf(path, format string, a ...any) error {
	return fmt.Errorf("schema %s: %s", path, fmt.Sprintf(format, a...))
}

func schemaRat(v any) (*big.Rat, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, false
	}
	return new(big.Rat).SetString(n.String())
}

// schemaCount reads a nonnegative integral count or length bound.
func schemaCount(path, key string, v any) (*big.Rat, error) {
	r, ok := schemaRat(v)
	if !ok || !r.IsInt() || r.Sign() < 0 {
		return nil, schemaBadf(path, "%s must be a nonnegative integer", key)
	}
	return r, nil
}

func checkSchemaDeclaration(path string, s map[string]any) error {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	bounds := map[string]*big.Rat{}
	for _, k := range keys {
		if !slices.Contains(schemaEnforced, k) && !slices.Contains(schemaAnnotations, k) {
			return schemaBadf(path, "unsupported schema keyword %s", k)
		}
		v := s[k]
		switch k {
		case "type":
			name, ok := v.(string)
			if !ok || !slices.Contains(schemaTypes, name) {
				return schemaBadf(path, "type must be one supported type string")
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return schemaBadf(path, "properties must be an object")
			}
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			slices.Sort(names)
			for _, name := range names {
				child, ok := props[name].(map[string]any)
				if !ok {
					return schemaBadf(path+"."+name, "a property schema must be an object")
				}
				if err := checkSchemaDeclaration(path+"."+name, child); err != nil {
					return err
				}
			}
		case "items":
			child, ok := v.(map[string]any)
			if !ok {
				return schemaBadf(path+"[]", "an items schema must be an object")
			}
			if err := checkSchemaDeclaration(path+"[]", child); err != nil {
				return err
			}
		case "required":
			list, ok := v.([]any)
			if !ok {
				return schemaBadf(path, "required must be a string array")
			}
			for _, e := range list {
				if _, ok := e.(string); !ok {
					return schemaBadf(path, "required must be a string array")
				}
			}
		case "additionalProperties", "uniqueItems":
			if _, ok := v.(bool); !ok {
				return schemaBadf(path, "%s must be a boolean", k)
			}
		case "enum":
			if list, ok := v.([]any); !ok || len(list) == 0 {
				return schemaBadf(path, "enum must be a nonempty array")
			}
		case "pattern":
			p, ok := v.(string)
			if !ok {
				return schemaBadf(path, "pattern must be a string")
			}
			if _, err := regexp.Compile(p); err != nil {
				return schemaBadf(path, "invalid pattern: %v", err)
			}
		case "minLength", "maxLength", "minItems", "maxItems", "minProperties":
			r, err := schemaCount(path, k, v)
			if err != nil {
				return err
			}
			bounds[k] = r
		case "minimum", "maximum":
			r, ok := schemaRat(v)
			if !ok {
				return schemaBadf(path, "%s must be a number", k)
			}
			bounds[k] = r
		}
	}
	for _, pair := range [][2]string{{"minLength", "maxLength"}, {"minItems", "maxItems"}, {"minimum", "maximum"}} {
		if lo, hi := bounds[pair[0]], bounds[pair[1]]; lo != nil && hi != nil && lo.Cmp(hi) > 0 {
			return schemaBadf(path, "contradictory bounds %s and %s", pair[0], pair[1])
		}
	}
	return nil
}

func schemaTypeMatches(name string, v any) bool {
	switch name {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := schemaRat(v)
		return ok
	case "integer":
		r, ok := schemaRat(v)
		return ok && r.IsInt()
	}
	return false
}

// jsonValueEqual compares two decoded values recursively: map order is
// irrelevant and numbers compare by value.
func jsonValueEqual(a, b any) bool {
	if ra, ok := schemaRat(a); ok {
		rb, ok := schemaRat(b)
		return ok && ra.Cmp(rb) == 0
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonValueEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonValueEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func schemaBound(s map[string]any, key string) (*big.Rat, bool) {
	v, ok := s[key]
	if !ok {
		return nil, false
	}
	return schemaRat(v)
}

// schemaBelow reports whether n is below the lower bound key of s; schemaAbove
// whether it is above the upper bound.
func schemaBelow(s map[string]any, key string, n int) bool {
	lo, ok := schemaBound(s, key)
	return ok && big.NewRat(int64(n), 1).Cmp(lo) < 0
}

func schemaAbove(s map[string]any, key string, n int) bool {
	hi, ok := schemaBound(s, key)
	return ok && big.NewRat(int64(n), 1).Cmp(hi) > 0
}

func checkSchemaInstance(path string, s map[string]any, v any) error {
	if name, ok := s["type"].(string); ok && !schemaTypeMatches(name, v) {
		return fmt.Errorf("%s: value is not of type %s", path, name)
	}
	if list, ok := s["enum"].([]any); ok && !slices.ContainsFunc(list, func(e any) bool { return jsonValueEqual(e, v) }) {
		return fmt.Errorf("%s: value is not in the enum", path)
	}
	switch x := v.(type) {
	case string:
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			return fmt.Errorf("%s: value does not match the pattern", path)
		}
		n := utf8.RuneCountInString(x)
		if schemaBelow(s, "minLength", n) || schemaAbove(s, "maxLength", n) {
			return fmt.Errorf("%s: length %d is out of bounds", path, n)
		}
	case json.Number:
		r, _ := schemaRat(x)
		if lo, ok := schemaBound(s, "minimum"); ok && r.Cmp(lo) < 0 {
			return fmt.Errorf("%s: value is below the minimum", path)
		}
		if hi, ok := schemaBound(s, "maximum"); ok && r.Cmp(hi) > 0 {
			return fmt.Errorf("%s: value is above the maximum", path)
		}
	case []any:
		if schemaBelow(s, "minItems", len(x)) || schemaAbove(s, "maxItems", len(x)) {
			return fmt.Errorf("%s: %d items is out of bounds", path, len(x))
		}
		if unique, _ := s["uniqueItems"].(bool); unique {
			for i := range x {
				for j := i + 1; j < len(x); j++ {
					if jsonValueEqual(x[i], x[j]) {
						return fmt.Errorf("%s: items %d and %d are duplicates", path, i, j)
					}
				}
			}
		}
		if child, ok := s["items"].(map[string]any); ok {
			for i, e := range x {
				if err := checkSchemaInstance(fmt.Sprintf("%s[%d]", path, i), child, e); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		if schemaBelow(s, "minProperties", len(x)) {
			return fmt.Errorf("%s: %d properties is too few", path, len(x))
		}
		if list, ok := s["required"].([]any); ok {
			for _, e := range list {
				if _, present := x[e.(string)]; !present {
					return fmt.Errorf("%s: missing required property %s", path, e)
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		names := make([]string, 0, len(x))
		for name := range x {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			child, known := props[name].(map[string]any)
			if known {
				if err := checkSchemaInstance(path+"."+name, child, x[name]); err != nil {
					return err
				}
				continue
			}
			if extra, ok := s["additionalProperties"].(bool); ok && !extra {
				return fmt.Errorf("%s: unknown property %s", path, name)
			}
		}
	}
	return nil
}

// decodeStrictJSON decodes exactly one JSON value with json.Number numbers,
// refusing a duplicate key at any depth and any trailing data.
func decodeStrictJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeStrictValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("json: trailing data after the value")
	}
	return v, nil
}

func decodeStrictValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if d == '[' {
		out := []any{}
		for dec.More() {
			v, err := decodeStrictValue(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf("json: %w", err)
		}
		return out, nil
	}
	out := map[string]any{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("json: %w", err)
		}
		key := keyTok.(string)
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("json: duplicate key %q", key)
		}
		v, err := decodeStrictValue(dec)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return out, nil
}

// TestGuideJSONDecoder (FP-3, FP-6): the strict decoder's unit table.
func TestGuideJSONDecoder(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"object", `{"a": 1, "b": [true, null, "x"], "c": {"d": 1.5}}`, ""},
		{"scalar", `"x"`, ""},
		{"empty-object", `{}`, ""},
		{"same-key-in-siblings", `[{"a": 1}, {"a": 2}]`, ""},
		{"duplicate-top", `{"a": 1, "a": 2}`, `duplicate key "a"`},
		{"duplicate-nested", `{"a": {"b": 1, "b": 2}}`, `duplicate key "b"`},
		{"duplicate-in-array", `[{"c": 1, "c": 2}]`, `duplicate key "c"`},
		{"trailing-value", `{} {}`, "trailing data"},
		{"trailing-text", `{"a": 1} x`, "trailing data"},
		{"truncated", `{"a": 1`, "json:"},
		{"empty", ``, "json:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeStrictJSON(c.text)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
	v, err := decodeStrictJSON(`{"n": 12345678901234567890}`)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := v.(map[string]any)["n"].(json.Number); !ok || n.String() != "12345678901234567890" {
		t.Fatalf("number %#v was not kept exactly", v)
	}
}

// TestGuideSchemaValidator (FP-3, FP-6): the closed dialect's unit table. Every
// enforced keyword has a violating instance; every unsupported keyword and
// malformed declaration fails, also inside a branch the instance never reaches.
func TestGuideSchemaValidator(t *testing.T) {
	schemaOf := func(t *testing.T, text string) map[string]any {
		t.Helper()
		v, err := decodeStrictJSON(text)
		if err != nil {
			t.Fatal(err)
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("schema %s is not an object", text)
		}
		return m
	}
	valueOf := func(t *testing.T, text string) any {
		t.Helper()
		v, err := decodeStrictJSON(text)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const nested = `{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"type":"array","minItems":1,"items":{"type":"object","properties":{"k":{"type":"string"}}}},"b":{"type":"integer"}}}`
	t.Run("accepts", func(t *testing.T) {
		for _, c := range []struct{ name, schema, value string }{
			{"object", `{"type":"object"}`, `{"any":1}`},
			{"array", `{"type":"array"}`, `[1]`},
			{"string", `{"type":"string"}`, `"s"`},
			{"integer", `{"type":"integer"}`, `3`},
			{"integral-float", `{"type":"integer"}`, `3.0`},
			{"number", `{"type":"number"}`, `3.5`},
			{"boolean", `{"type":"boolean"}`, `false`},
			{"null", `{"type":"null"}`, `null`},
			{"nested", nested, `{"a":[{"k":"v"}],"b":2}`},
			{"optional-absent", nested, `{"a":[{}]}`},
			{"min-max-low", `{"type":"integer","minimum":1,"maximum":3}`, `1`},
			{"min-max-high", `{"type":"integer","minimum":1,"maximum":3}`, `3`},
			{"length-low", `{"type":"string","minLength":2,"maxLength":3}`, `"ab"`},
			{"length-high", `{"type":"string","minLength":2,"maxLength":3}`, `"abc"`},
			{"length-unicode", `{"type":"string","maxLength":2}`, `"éé"`},
			{"items-low", `{"type":"array","minItems":1,"maxItems":2}`, `[1]`},
			{"items-high", `{"type":"array","minItems":1,"maxItems":2}`, `[1,2]`},
			{"properties-low", `{"type":"object","minProperties":1}`, `{"a":1}`},
			{"enum", `{"enum":["a",1]}`, `1.0`},
			{"pattern", `{"type":"string","pattern":"^a+$"}`, `"aaa"`},
			{"additional-true", `{"type":"object","properties":{},"additionalProperties":true}`, `{"x":1}`},
			{"additional-false-no-extra", `{"type":"object","properties":{"x":{}},"additionalProperties":false}`, `{"x":1}`},
			{"unique", `{"type":"array","uniqueItems":true}`, `[1,2,{"a":1},{"a":2}]`},
			{"annotations", `{"type":"integer","description":"d","default":5}`, `1`},
		} {
			t.Run(c.name, func(t *testing.T) {
				if err := validateAgainstSchema(schemaOf(t, c.schema), valueOf(t, c.value)); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("rejects-instances", func(t *testing.T) {
		for _, c := range []struct{ name, schema, value, want string }{
			{"missing-required", nested, `{}`, "$: missing required property a"},
			{"extra-key", nested, `{"a":[{}],"c":1}`, "$: unknown property c"},
			{"wrong-scalar", `{"type":"string"}`, `1`, "$: value is not of type string"},
			{"wrong-container", `{"type":"array"}`, `{}`, "$: value is not of type array"},
			{"string-is-no-number", `{"type":"number"}`, `"1"`, "$: value is not of type number"},
			{"fractional-integer", `{"type":"integer"}`, `1.5`, "$: value is not of type integer"},
			{"null-mismatch", `{"type":"null"}`, `0`, "$: value is not of type null"},
			{"boolean-mismatch", `{"type":"boolean"}`, `"true"`, "$: value is not of type boolean"},
			{"nested-path", nested, `{"a":[{"k":1}]}`, "$.a[0].k: value is not of type string"},
			{"enum-mismatch", `{"enum":["a"]}`, `"b"`, "$: value is not in the enum"},
			{"pattern-mismatch", `{"type":"string","pattern":"^a$"}`, `"b"`, "$: value does not match the pattern"},
			{"short-unicode", `{"type":"string","minLength":2}`, `"é"`, "$: length 1 is out of bounds"},
			{"long-unicode", `{"type":"string","maxLength":1}`, `"éé"`, "$: length 2 is out of bounds"},
			{"small-array", `{"type":"array","minItems":1}`, `[]`, "$: 0 items is out of bounds"},
			{"large-array", `{"type":"array","maxItems":1}`, `[1,2]`, "$: 2 items is out of bounds"},
			{"duplicate-primitive", `{"type":"array","uniqueItems":true}`, `[1,1.0]`, "$: items 0 and 1 are duplicates"},
			{"duplicate-reordered", `{"type":"array","uniqueItems":true}`, `[{"a":1,"b":2},{"b":2,"a":1}]`, "$: items 0 and 1 are duplicates"},
			{"too-few-properties", `{"type":"object","minProperties":2}`, `{"a":1}`, "$: 1 properties is too few"},
			{"below-minimum", `{"type":"number","minimum":1}`, `0.5`, "$: value is below the minimum"},
			{"above-maximum", `{"type":"number","maximum":1}`, `1.5`, "$: value is above the maximum"},
		} {
			t.Run(c.name, func(t *testing.T) {
				err := validateAgainstSchema(schemaOf(t, c.schema), valueOf(t, c.value))
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want one containing %q", err, c.want)
				}
			})
		}
	})
	t.Run("rejects-declarations", func(t *testing.T) {
		for _, key := range []string{"$ref", "$defs", "definitions", "$schema", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "format",
			"patternProperties", "additionalItems", "contains", "const", "maxProperties"} {
			for _, shape := range []struct{ name, format, path string }{
				{"root", `{"%s":{}}`, "schema $:"},
				{"absent-property", `{"properties":{"unused":{"%s":{}}}}`, "schema $.unused:"},
				{"absent-items", `{"properties":{"unused":{"items":{"%s":{}}}}}`, "schema $.unused[]:"},
			} {
				t.Run(key+"/"+shape.name, func(t *testing.T) {
					err := validateAgainstSchema(schemaOf(t, fmt.Sprintf(shape.format, key)), map[string]any{})
					want := shape.path + " unsupported schema keyword " + key
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("err = %v, want one containing %q", err, want)
					}
				})
			}
		}
		for _, c := range []struct{ name, schema, want string }{
			{"invalid-regex", `{"pattern":"("}`, "invalid pattern"},
			{"pattern-type", `{"pattern":1}`, "pattern must be a string"},
			{"type-array", `{"type":["string","null"]}`, "type must be one supported type string"},
			{"unknown-type", `{"type":"file"}`, "type must be one supported type string"},
			{"boolean-property", `{"properties":{"a":true}}`, "a property schema must be an object"},
			{"boolean-items", `{"items":true}`, "an items schema must be an object"},
			{"properties-type", `{"properties":[]}`, "properties must be an object"},
			{"required-type", `{"required":"a"}`, "required must be a string array"},
			{"required-element", `{"required":[1]}`, "required must be a string array"},
			{"additional-schema", `{"additionalProperties":{}}`, "additionalProperties must be a boolean"},
			{"unique-type", `{"uniqueItems":"yes"}`, "uniqueItems must be a boolean"},
			{"empty-enum", `{"enum":[]}`, "enum must be a nonempty array"},
			{"enum-type", `{"enum":"a"}`, "enum must be a nonempty array"},
			{"negative-count", `{"minItems":-1}`, "minItems must be a nonnegative integer"},
			{"fractional-count", `{"maxLength":1.5}`, "maxLength must be a nonnegative integer"},
			{"string-count", `{"minProperties":"1"}`, "minProperties must be a nonnegative integer"},
			{"string-minimum", `{"minimum":"1"}`, "minimum must be a number"},
			{"length-contradiction", `{"minLength":2,"maxLength":1}`, "contradictory bounds minLength and maxLength"},
			{"items-contradiction", `{"minItems":2,"maxItems":1}`, "contradictory bounds minItems and maxItems"},
			{"numeric-contradiction", `{"minimum":2,"maximum":1}`, "contradictory bounds minimum and maximum"},
		} {
			t.Run(c.name, func(t *testing.T) {
				err := validateAgainstSchema(schemaOf(t, c.schema), map[string]any{})
				if err == nil || !strings.HasPrefix(err.Error(), "schema ") || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want a schema diagnostic containing %q", err, c.want)
				}
			})
		}
	})
	t.Run("no-insertion-or-coercion", func(t *testing.T) {
		schema := schemaOf(t, `{"type":"object","properties":{"limit":{"type":"integer","default":5},"s":{"type":"string"}}}`)
		value := valueOf(t, `{"s":"1"}`)
		if err := validateAgainstSchema(schema, value); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(value, map[string]any{"s": "1"}) {
			t.Fatalf("the instance changed: %#v", value)
		}
		if err := validateAgainstSchema(schema, valueOf(t, `{"limit":"5"}`)); err == nil {
			t.Fatal("a string was coerced into an integer")
		}
	})
}
