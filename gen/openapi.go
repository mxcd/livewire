package gen

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/mxcd/home/pkg/livewire"
)

type object = map[string]any

func schema(t reflect.Type) object {
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
		return object{"type": "array", "items": schema(t.Elem())}
	case kindMap:
		return object{"type": "object", "additionalProperties": schema(t.Elem())}
	case kindNullable:
		return object{"anyOf": []any{schema(t.Elem()), object{"type": "null"}}}
	case kindStruct:
		if t.Name() != "" {
			return object{"$ref": "#/components/schemas/" + t.Name()}
		}
		return structSchema(t)
	}
	return object{}
}

func structSchema(t reflect.Type) object {
	properties := object{}
	required := []string{}
	for _, f := range jsonFields(t) {
		s := schema(f.Type)
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

func openAPI(registry *livewire.Registry, ts *types, options Options) ([]byte, error) {
	schemas := object{}
	for _, t := range ts.sorted() {
		schemas[t.Name()] = structSchema(t)
	}
	errorResponse := object{"description": "Error", "content": object{"application/json": object{"schema": schema(reflect.TypeFor[livewire.Error]())}}}
	paths := map[string]object{}
	operation := func(path, method string, op object) {
		p := openAPIPath(path)
		if paths[p] == nil {
			paths[p] = object{}
		}
		paths[p][strings.ToLower(method)] = op
	}
	for _, res := range registry.Resources {
		result := schema(res.DTO)
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
			"parameters":  openAPIParams(res.Params),
			"responses": object{
				"200":     object{"description": "OK", "content": object{"application/json": object{"schema": result}}},
				"default": errorResponse,
			},
		})
	}
	for _, m := range registry.Mutations {
		op := object{
			"operationId": m.Name,
			"parameters":  openAPIParams(m.Params),
			"responses":   object{"default": errorResponse},
		}
		if m.HasBody() {
			op["requestBody"] = object{"required": true, "content": object{"application/json": object{"schema": schema(m.Request)}}}
		}
		if m.Status() == http.StatusNoContent {
			op["responses"].(object)["204"] = object{"description": "No content"}
		} else {
			op["responses"].(object)["200"] = object{"description": "OK", "content": object{"application/json": object{"schema": schema(m.Response)}}}
		}
		operation(m.Path, m.Method, op)
	}
	document := object{
		"openapi": "3.1.0",
		"info":    object{"title": options.Title, "version": options.Version},
		"servers": []any{object{"url": options.BasePath}},
		"paths":   paths,
		"components": object{
			"schemas": schemas,
			"securitySchemes": object{
				"bearer":  object{"type": "http", "scheme": "bearer", "description": "API key"},
				"session": object{"type": "apiKey", "in": "cookie", "name": "basicauth_session"},
			},
		},
		"security": []any{object{"bearer": []any{}}, object{"session": []any{}}},
	}
	return json.MarshalIndent(document, "", "  ")
}

func openAPIParams(t reflect.Type) []any {
	out := []any{}
	for _, p := range params(t) {
		typ := p.Type
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		out = append(out, object{"name": p.Name, "in": p.In, "required": p.In == "path", "schema": schema(typ)})
	}
	return out
}
