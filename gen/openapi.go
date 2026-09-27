package gen

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/mxcd/livewire"
)

type object = map[string]any

func (ts *types) schema(t reflect.Type) object {
	if _, ok := ts.enums[t]; ok {
		return object{"$ref": "#/components/schemas/" + typeName(t)}
	}
	switch classify(t) {
	case kindString:
		if t.Name() == "Time" && t.PkgPath() == "time" {
			return object{"type": "string", "format": "date-time"}
		}
		return object{"type": "string"}
	case kindNumber:
		if strings.HasPrefix(t.Kind().String(), "float") {
			return object{"type": "number"}
		}
		return object{"type": "integer"}
	case kindBool:
		return object{"type": "boolean"}
	case kindArray:
		return object{"type": "array", "items": ts.schema(t.Elem())}
	case kindMap:
		return object{"type": "object", "additionalProperties": ts.schema(t.Elem())}
	case kindNullable:
		return object{"anyOf": []any{ts.schema(t.Elem()), object{"type": "null"}}}
	case kindStruct:
		if t.Name() != "" {
			return object{"$ref": "#/components/schemas/" + typeName(t)}
		}
		return ts.structSchema(t)
	}
	return object{}
}

func (ts *types) structSchema(t reflect.Type) object {
	properties := object{}
	required := []string{}
	for _, f := range jsonFields(t) {
		s := ts.schema(f.Type)
		if len(f.Enum) > 0 {
			s = object{"type": "string", "enum": f.Enum}
		}
		properties[f.Name] = s
		if !f.Optional || f.Required {
			required = append(required, f.Name)
		}
	}
	out := object{"type": "object", "properties": properties}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func (ts *types) openAPI(registry *livewire.Registry, options Options) ([]byte, error) {
	schemas := object{}
	for _, t := range ts.sorted() {
		if values, ok := ts.enums[t]; ok {
			schemas[typeName(t)] = object{"type": "string", "enum": values}
		} else {
			schemas[typeName(t)] = ts.structSchema(t)
		}
	}
	errorResponse := object{"description": "Error", "content": object{"application/json": object{"schema": ts.schema(reflect.TypeFor[livewire.Error]())}}}
	paths := map[string]object{}
	operation := func(path, method string, op object) {
		p := openAPIPath(path)
		if paths[p] == nil {
			paths[p] = object{}
		}
		paths[p][strings.ToLower(method)] = op
	}
	for _, res := range registry.Resources {
		result := ts.schema(res.DTO)
		if res.List {
			result = object{"type": "array", "items": result}
		}
		description := "Read " + res.Name + "."
		if res.Live() {
			description += " Live: subscribe to target \"" + res.Name + "\" on the WebSocket for pushed updates."
		}
		operation(res.Path, http.MethodGet, object{
			"operationId": "get" + upperFirst(res.Name),
			"description": description,
			"parameters":  ts.openAPIParams(res.Params),
			"responses": object{
				"200":     object{"description": "OK", "content": object{"application/json": object{"schema": result}}},
				"default": errorResponse,
			},
		})
	}
	for _, m := range registry.Mutations {
		op := object{
			"operationId": m.Name,
			"parameters":  ts.openAPIParams(m.Params),
			"responses":   object{"default": errorResponse},
		}
		if m.HasBody() {
			op["requestBody"] = object{"required": true, "content": object{"application/json": object{"schema": ts.schema(m.Request)}}}
		}
		status := strconv.Itoa(m.Status())
		if m.HasContent() {
			op["responses"].(object)[status] = object{"description": http.StatusText(m.Status()), "content": object{"application/json": object{"schema": ts.schema(m.Response)}}}
		} else {
			op["responses"].(object)[status] = object{"description": "No content"}
		}
		operation(m.Path, m.Method, op)
	}
	document := object{
		"openapi":    "3.1.0",
		"info":       object{"title": options.Title, "version": options.Version},
		"servers":    []any{object{"url": options.BasePath}},
		"paths":      paths,
		"components": object{"schemas": schemas},
	}
	if len(options.SecuritySchemes) > 0 {
		names := make([]string, 0, len(options.SecuritySchemes))
		for name := range options.SecuritySchemes {
			names = append(names, name)
		}
		sort.Strings(names)
		security := make([]any, len(names))
		for i, name := range names {
			security[i] = object{name: []any{}}
		}
		document["components"].(object)["securitySchemes"] = options.SecuritySchemes
		document["security"] = security
	}
	return json.MarshalIndent(document, "", "  ")
}

func (ts *types) openAPIParams(t reflect.Type) []any {
	out := []any{}
	for _, p := range params(t) {
		typ := p.Type
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		out = append(out, object{"name": p.Name, "in": p.In, "required": p.In == "path", "schema": ts.schema(typ)})
	}
	return out
}
