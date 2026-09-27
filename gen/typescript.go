package gen

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/mxcd/home/pkg/livewire"
)

func tsType(t reflect.Type) string {
	switch classify(t) {
	case kindString:
		return "string"
	case kindNumber:
		return "number"
	case kindBool:
		return "boolean"
	case kindArray:
		elem := tsType(t.Elem())
		if strings.Contains(elem, "|") {
			elem = "(" + elem + ")"
		}
		return elem + "[]"
	case kindMap:
		return "Record<string, " + tsType(t.Elem()) + ">"
	case kindNullable:
		return tsType(t.Elem()) + " | null"
	case kindStruct:
		if t.Name() != "" {
			return t.Name()
		}
		return "{ " + strings.Join(tsFields(t), "; ") + " }"
	}
	return "unknown"
}

func tsFields(t reflect.Type) []string {
	var lines []string
	for _, f := range jsonFields(t) {
		typ := tsType(f.Type)
		if len(f.Enum) > 0 {
			typ = "'" + strings.Join(f.Enum, "' | '") + "'"
			if f.Type.Kind() == reflect.Pointer {
				typ += " | null"
			}
		}
		optional := ""
		if f.Optional {
			optional = "?"
		}
		lines = append(lines, fmt.Sprintf("%s%s: %s", f.Name, optional, typ))
	}
	return lines
}

func typescriptTypes(ts *types) string {
	var b strings.Builder
	b.WriteString(generatedHeader)
	for _, t := range ts.sorted() {
		fmt.Fprintf(&b, "export interface %s {\n", t.Name())
		for _, line := range tsFields(t) {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteString("}\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// tsRef is a type as api.ts names it: named structs live in types.ts, everything else is
// spelled out.
func tsRef(t reflect.Type) string {
	if classify(t) == kindStruct && t.Name() != "" {
		return "T." + t.Name()
	}
	return tsType(t)
}

func typescriptAPI(registry *livewire.Registry) string {
	var b strings.Builder
	b.WriteString(generatedHeader)
	b.WriteString("import { request, target } from './runtime'\nimport type * as T from './types'\n\n")

	var liveTargets []string
	for _, res := range registry.Resources {
		name := upperFirst(res.Name)
		result := tsRef(res.DTO)
		if res.List {
			result += "[]"
		}
		paramsType, paramsArg := tsParams(&b, name+"Params", res.Params)
		fmt.Fprintf(&b, "export function get%s(%s): Promise<%s> {\n  return request('GET', %s%s)\n}\n\n",
			name, paramsArg, result, tsPath(res.Path), tsQuery(res.Params))
		if res.Live() {
			liveTargets = append(liveTargets, fmt.Sprintf("  %s: target<%s, %s>('%s', %t),", res.Name, result, paramsType, res.Name, res.List))
		}
	}
	for _, m := range registry.Mutations {
		_, paramsArg := tsParams(&b, upperFirst(m.Name)+"Params", m.Params)
		args := []string{}
		if paramsArg != "" && !strings.HasSuffix(paramsArg, "= {}") {
			args = append(args, paramsArg)
		}
		body := ""
		if m.HasBody() {
			args = append(args, "body: "+tsRef(m.Request))
			body = ", { body }"
		}
		result := "void"
		if m.Status() != 204 {
			result = tsRef(m.Response)
		}
		fmt.Fprintf(&b, "export function %s(%s): Promise<%s> {\n  return request('%s', %s%s)\n}\n\n",
			m.Name, strings.Join(args, ", "), result, m.Method, tsPath(m.Path), body)
	}
	b.WriteString("export const live = {\n" + strings.Join(liveTargets, "\n") + "\n}\n")
	return b.String()
}

// tsParams writes the parameter interface when the type has parameters and returns its
// name and the function argument declaring it.
func tsParams(b *strings.Builder, name string, t reflect.Type) (typeName, arg string) {
	ps := params(t)
	if len(ps) == 0 {
		return "Record<string, never>", ""
	}
	fmt.Fprintf(b, "export interface %s {\n", name)
	required := false
	for _, p := range ps {
		optional := "?"
		if p.In == "path" {
			optional, required = "", true
		}
		fmt.Fprintf(b, "  %s%s: %s\n", p.Name, optional, strings.TrimSuffix(tsType(p.Type), " | null"))
	}
	b.WriteString("}\n\n")
	if required {
		return name, "params: " + name
	}
	return name, "params: " + name + " = {}"
}

// tsPath renders a gin path as a template literal over params.
func tsPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "${encodeURIComponent(String(params." + p[1:] + "))}"
		}
	}
	return "`" + strings.Join(parts, "/") + "`"
}

func tsQuery(t reflect.Type) string {
	var names []string
	for _, p := range params(t) {
		if p.In == "query" {
			names = append(names, p.Name+": params."+p.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return ", { query: { " + strings.Join(names, ", ") + " } }"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
