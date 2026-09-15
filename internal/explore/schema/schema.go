// Package schema defines the exploration domain's data: response schemas and the guard that an expanded
// schema keeps the minimum fields, the raw task and explorer payload, the explorer envelope and
// model-identity policy, the formulation record, and each mode's output types.
package schema

import (
	"fmt"
	"strings"
)

// FieldType is the type of a schema field. Response schemas describe flat fields, not nested JSON Schema.
type FieldType string

// The field types.
const (
	TypeString  FieldType = "string"
	TypeNumber  FieldType = "number"
	TypeBoolean FieldType = "boolean"
	TypeObject  FieldType = "object" // a free-form JSON object whose sub-fields are not checked
)

func (t FieldType) valid() bool {
	switch t {
	case TypeString, TypeNumber, TypeBoolean, TypeObject:
		return true
	}
	return false
}

// Field is one field of a response schema. `Repeated` makes it a list of Type (e.g. claims[]).
type Field struct {
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
	Repeated bool      `json:"repeated,omitempty"`
}

// Schema is an ordered list of fields, used by both the minimum schema and expanded schemas so
// ExpandedSatisfiesMinimum can compare them structurally.
type Schema struct {
	Fields []Field `json:"fields"`
}

// field returns the field named name (ok=false if absent).
func (s Schema) field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Validate checks the schema is internally well-formed: non-empty unique field names and valid
// types. It does not compare against the minimum (that is the guard's job).
func (s Schema) Validate() error {
	if len(s.Fields) == 0 {
		return fmt.Errorf("schema: no fields")
	}
	seen := map[string]bool{}
	for _, f := range s.Fields {
		if strings.TrimSpace(f.Name) == "" {
			return fmt.Errorf("schema: empty field name")
		}
		if seen[f.Name] {
			return fmt.Errorf("schema: duplicate field %q", f.Name)
		}
		seen[f.Name] = true
		if !f.Type.valid() {
			return fmt.Errorf("schema: field %q has invalid type %q", f.Name, f.Type)
		}
	}
	return nil
}

// reservedFields are the minimum schema's fields. Their meaning is fixed, so an expanded schema must keep
// each with the same type, required flag and repetition.
var reservedFields = []Field{
	{Name: "claims", Type: TypeString, Required: true, Repeated: true},
	{Name: "evidence", Type: TypeString, Required: true, Repeated: false},
	{Name: "confidence", Type: TypeNumber, Required: true, Repeated: false},
	{Name: "sources", Type: TypeString, Required: true, Repeated: true},
	{Name: "assumptions", Type: TypeString, Required: true, Repeated: true},
	{Name: "uncertainties", Type: TypeString, Required: true, Repeated: true},
}

// MinimumSchema returns a copy of the minimum response schema. An expanded schema may add fields but not
// change these.
func MinimumSchema() Schema {
	fields := make([]Field, len(reservedFields))
	copy(fields, reservedFields)
	return Schema{Fields: fields}
}

// RenderSchema renders s as one instruction line per field for an explorer prompt. The model sees only
// the prompt text, so the exact field names and types must appear there.
func RenderSchema(s Schema) string {
	var b strings.Builder
	for _, f := range s.Fields {
		typ := string(f.Type)
		if f.Repeated {
			typ = "array of " + typ + "s"
		}
		req := "required"
		if !f.Required {
			req = "optional"
		}
		fmt.Fprintf(&b, "- %q: %s (%s)\n", f.Name, typ, req)
	}
	return b.String()
}

// ExpandedSatisfiesMinimum checks that expanded is well-formed and keeps every minimum field with the
// same type, required flag and repetition. Adding fields is allowed.
func ExpandedSatisfiesMinimum(expanded Schema) error {
	if err := expanded.Validate(); err != nil {
		return err
	}
	for _, base := range reservedFields {
		got, ok := expanded.field(base.Name)
		if !ok {
			return fmt.Errorf("guard: expanded schema drops reserved baseline field %q", base.Name)
		}
		if got.Type != base.Type {
			return fmt.Errorf("guard: reserved field %q re-typed %q→%q (baseline is immutable)", base.Name, base.Type, got.Type)
		}
		if got.Required != base.Required {
			return fmt.Errorf("guard: reserved field %q relaxed required %v→%v (baseline is immutable)", base.Name, base.Required, got.Required)
		}
		if got.Repeated != base.Repeated {
			return fmt.Errorf("guard: reserved field %q changed repeated %v→%v (baseline is immutable)", base.Name, base.Repeated, got.Repeated)
		}
	}
	return nil
}

// ValidateResponse checks an explorer's decoded response object against schema `s`: every required
// field must be present and match its type/repeated shape; unknown fields are allowed (the schema
// is a floor, and explorers may enrich). A repeated field must be a JSON array of the element type;
// a scalar field must match Type. Returns a descriptive error on the first violation.
func ValidateResponse(s Schema, response map[string]any) error {
	for _, f := range s.Fields {
		v, present := response[f.Name]
		if !present {
			if f.Required {
				return fmt.Errorf("response: missing required field %q", f.Name)
			}
			continue
		}
		if v == nil {
			if f.Required {
				return fmt.Errorf("response: required field %q is null", f.Name)
			}
			continue
		}
		if f.Repeated {
			arr, ok := v.([]any)
			if !ok {
				return fmt.Errorf("response: field %q must be a list", f.Name)
			}
			for i, el := range arr {
				if err := checkScalar(f.Type, el); err != nil {
					return fmt.Errorf("response: field %q[%d]: %w", f.Name, i, err)
				}
			}
			continue
		}
		if err := checkScalar(f.Type, v); err != nil {
			return fmt.Errorf("response: field %q: %w", f.Name, err)
		}
	}
	return nil
}

// checkScalar verifies a single decoded JSON value matches a FieldType (JSON numbers decode to
// float64; objects to map[string]any).
func checkScalar(t FieldType, v any) error {
	switch t {
	case TypeString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
	case TypeNumber:
		switch v.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("expected number, got %T", v)
		}
	case TypeBoolean:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", v)
		}
	case TypeObject:
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("expected object, got %T", v)
		}
	default:
		return fmt.Errorf("unknown field type %q", t)
	}
	return nil
}
