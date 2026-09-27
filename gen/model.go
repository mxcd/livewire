// Package gen turns a livewire registry into a TypeScript client (types, REST functions,
// live targets, runtime) and an OpenAPI 3.1 document.
package gen

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

var (
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	rawMessage    = reflect.TypeFor[json.RawMessage]()
)

// field is one JSON property of a struct.
type field struct {
	Name     string
	Type     reflect.Type
	Optional bool     // omitempty or omitzero: may be absent
	Enum     []string // from an `enum:"a,b"` tag
	Required bool     // `binding:"required"`
}

// jsonFields lists the JSON properties of a struct as encoding/json writes them,
// embedded structs flattened.
func jsonFields(t reflect.Type) []field {
	var fields []field
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out := field{Name: name, Type: f.Type,
			Optional: strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero"),
			Required: strings.Contains(f.Tag.Get("binding"), "required")}
		if enum := f.Tag.Get("enum"); enum != "" {
			out.Enum = strings.Split(enum, ",")
		}
		fields = append(fields, out)
	}
	return fields
}

// kind classifies a Go type for both emitters.
type kind int

const (
	kindUnknown kind = iota
	kindString
	kindNumber
	kindBool
	kindArray
	kindMap
	kindNullable
	kindStruct
)

func classify(t reflect.Type) kind {
	switch {
	case t == rawMessage || t.Kind() == reflect.Interface:
		return kindUnknown
	case t.Kind() == reflect.Pointer:
		return kindNullable
	case t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler):
		return kindString
	case t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler):
		return kindUnknown
	}
	switch t.Kind() {
	case reflect.String:
		return kindString
	case reflect.Bool:
		return kindBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return kindNumber
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return kindString
		}
		return kindArray
	case reflect.Map:
		return kindMap
	case reflect.Struct:
		return kindStruct
	}
	return kindUnknown
}

// types collects the named struct types reachable from the registry.
type types struct {
	byName map[string]reflect.Type
}

func (ts *types) add(t reflect.Type) {
	switch classify(t) {
	case kindNullable, kindArray, kindMap:
		ts.add(t.Elem())
	case kindStruct:
		if t.Name() != "" {
			if other, ok := ts.byName[t.Name()]; ok {
				if other != t {
					panic(fmt.Sprintf("gen: two types named %s (%s, %s)", t.Name(), other.PkgPath(), t.PkgPath()))
				}
				return
			}
			ts.byName[t.Name()] = t
		}
		for _, f := range jsonFields(t) {
			ts.add(f.Type)
		}
	}
}

func (ts *types) sorted() []reflect.Type {
	names := make([]string, 0, len(ts.byName))
	for name := range ts.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]reflect.Type, len(names))
	for i, name := range names {
		out[i] = ts.byName[name]
	}
	return out
}

// param is one path or query parameter.
type param struct {
	Name, In string
	Type     reflect.Type
}

func params(t reflect.Type) []param {
	var out []param
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if name := f.Tag.Get("path"); name != "" {
			out = append(out, param{name, "path", f.Type})
		} else if name := f.Tag.Get("query"); name != "" {
			out = append(out, param{name, "query", f.Type})
		}
	}
	return out
}

// openAPIPath turns gin's /todos/:id into /todos/{id}.
func openAPIPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}
