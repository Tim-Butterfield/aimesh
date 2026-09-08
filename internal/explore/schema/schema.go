// Package schema is exploremesh's domain schema (design §6): the machine-validatable response
// schema + the `expanded ⊇ minimum` guard, the raw-task / explorer-task-payload artifacts, the
// explorer outer envelope + model-identity policy, the formulation run-state, and the fixed
// collator-output schema. It depends on meshcore only (never reviewmesh).
package schema

import (
	"fmt"
	"strings"
)

// FieldType is the JSON-Schema-subset type of a schema field. The set is deliberately small: a
// response schema describes flat, machine-checkable fields, not arbitrary nested JSON Schema.
type FieldType string

const (
	TypeString  FieldType = "string"
	TypeNumber  FieldType = "number"
	TypeBoolean FieldType = "boolean"
	TypeObject  FieldType = "object" // a free-form JSON object (map); sub-fields are not enforced in v1
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

// Schema is an ordered list of fields — a JSON-Schema subset that both the minimum baseline and
// the collator's expanded schema use, so the guard (ExpandedSatisfiesMinimum) can compare them
// structurally rather than by prose.
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

// reservedFields are exploremesh's minimum-schema baseline field names — their MEANING is fixed by
// exploremesh, so the collator may not repurpose one (the guard enforces identical type/required/
// repeated). `uncertainties[]` is first-class so "what we still don't know" feeds the disagreement
// register rather than being buried in prose.
var reservedFields = []Field{
	{Name: "claims", Type: TypeString, Required: true, Repeated: true},
	{Name: "evidence", Type: TypeString, Required: true, Repeated: false},
	{Name: "confidence", Type: TypeNumber, Required: true, Repeated: false},
	{Name: "sources", Type: TypeString, Required: true, Repeated: true},
	{Name: "assumptions", Type: TypeString, Required: true, Repeated: true},
	{Name: "uncertainties", Type: TypeString, Required: true, Repeated: true},
}

// MinimumSchema returns exploremesh's reserved v1 baseline schema. The collator EXTENDS this (adds
// fields); it may never re-type, rename, drop, or relax a baseline field.
func MinimumSchema() Schema {
	fields := make([]Field, len(reservedFields))
	copy(fields, reservedFields)
	return Schema{Fields: fields}
}

// RenderSchema renders a Schema as an explicit per-field instruction block for an explorer prompt.
// The schema is otherwise validated-but-UNSEEN by the model (it lives in ExplorerTaskPayload, not the
// prompt text), so a formulation-free prompt that merely says "match the provided schema" gives the
// model nothing to match — it improvises field names and fails validation. Rendering the exact field
// names + types into the prompt is what makes real models emit the reserved fields.
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

// IsReserved reports whether name is a reserved baseline field.
func IsReserved(name string) bool {
	for _, f := range reservedFields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// ExpandedSatisfiesMinimum is the `expanded ⊇ minimum` guard. It is NOT name-only: every baseline
// field must be present with IDENTICAL type, required, and repeated. The collator may only ADD new
// fields — never re-type, rename, drop, or relax a baseline field. A violation is returned as an
// error (never a silent pass). It also validates the expanded schema is internally well-formed.
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
