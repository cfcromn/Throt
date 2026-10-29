package mcp

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// outputSchema returns the JSON Schema of the JSON encoding of v, a tool's
// structured content type, for its outputSchema. Deriving it from the type
// keeps it exact: every struct is an object whose fields are properties,
// required unless omitempty or omitzero. Other properties are allowed,
// since minor releases may add fields.
func outputSchema(v any) json.RawMessage {
	b, err := json.Marshal(schemaOf(reflect.TypeOf(v)))
	if err != nil {
		panic(err) // schemaOf builds only maps, slices and strings
	}
	return b
}

var (
	timeType          = reflect.TypeFor[time.Time]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

func schemaOf(t reflect.Type) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t.Implements(textMarshalerType):
		return map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		s := schemaOf(t.Elem())
		s["type"] = []any{s["type"], "null"}
		return s
	case reflect.Struct:
		props, required := map[string]any{}, []string{}
		addFields(t, props, &required)
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaOf(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	}
	panic("mcp: no schema for " + t.String())
}

// addFields adds the JSON fields of struct t, including those of embedded
// structs, to props, and the required ones to required.
func addFields(t reflect.Type, props map[string]any, required *[]string) {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" || !f.IsExported() && !f.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			addFields(f.Type, props, required)
			continue
		}
		if name == "" {
			name = f.Name
		}
		props[name] = schemaOf(f.Type)
		if !strings.Contains(","+opts+",", ",omitempty,") && !strings.Contains(","+opts+",", ",omitzero,") {
			*required = append(*required, name)
		}
	}
}
