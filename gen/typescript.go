package gen

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/mxcd/livewire"
)

// tsType spells a type in TypeScript. Named types live in types.ts; prefix is how the
// emitting file reaches them ("" in types.ts, "T." in api.ts).
func (ts *types) tsType(t reflect.Type, prefix string) string {
	if _, ok := ts.enums[t]; ok {
		return prefix + typeName(t)
	}
	switch classify(t) {
	case kindString:
		return "string"
	case kindNumber:
		return "number"
	case kindBool:
		return "boolean"
	case kindArray:
		elem := ts.tsType(t.Elem(), prefix)
		if strings.Contains(elem, "|") {
			elem = "(" + elem + ")"
		}
		return elem + "[]"
	case kindMap:
		return "Record<string, " + ts.tsType(t.Elem(), prefix) + ">"
	case kindNullable:
		return ts.tsType(t.Elem(), prefix) + " | null"
	case kindStruct:
		if t.Name() != "" {
			return prefix + typeName(t)
		}
		return "{ " + strings.Join(ts.tsFields(t, prefix), "; ") + " }"
	}
	return "unknown"
}

func (ts *types) tsFields(t reflect.Type, prefix string) []string {
	var lines []string
	for _, f := range jsonFields(t) {
		typ := ts.tsType(f.Type, prefix)
		if len(f.Enum) > 0 {
			typ = tsUnion(f.Enum)
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

func tsUnion(values []string) string { return "'" + strings.Join(values, "' | '") + "'" }

func (ts *types) typescriptTypes() string {
	var b strings.Builder
	b.WriteString(generatedHeader)
	for _, t := range ts.sorted() {
		if values, ok := ts.enums[t]; ok {
			fmt.Fprintf(&b, "export type %s = %s\n\n", typeName(t), tsUnion(values))
			continue
		}
		fmt.Fprintf(&b, "export interface %s {\n", typeName(t))
		for _, line := range ts.tsFields(t, "") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteString("}\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func (ts *types) typescriptAPI(registry *livewire.Registry) string {
	var b strings.Builder
	usesAmbient := false
	var liveTargets []string
	for _, res := range registry.Resources {
		name := upperFirst(res.Name)
		result := ts.tsType(res.DTO, "T.")
		if res.List {
			result += "[]"
		}
		paramsType, paramsArg := ts.tsParams(&b, name+"Params", res.Params)
		path, ambient := ts.tsPath(res.Path)
		usesAmbient = usesAmbient || ambient
		fmt.Fprintf(&b, "export %sfunction get%s(%s): Promise<%s> {\n  return request('GET', %s%s)\n}\n\n",
			tsAsync(ambient), name, paramsArg, result, path, tsInit(res.Params, false))
		if res.Live() {
			liveTargets = append(liveTargets, fmt.Sprintf("  %s: target<%s, %s>('%s', %t%s),", res.Name, result, paramsType, res.Name, res.List, ts.tsAmbient(res.Params)))
		}
	}
	for _, m := range registry.Mutations {
		_, paramsArg := ts.tsParams(&b, upperFirst(m.Name)+"Params", m.Params)
		// Required params come first; optional ones trail the body so callers may omit them.
		optional := strings.HasSuffix(paramsArg, "= {}")
		args := []string{}
		if paramsArg != "" && !optional {
			args = append(args, paramsArg)
		}
		if m.HasBody() {
			args = append(args, "body: "+ts.tsType(m.Request, "T."))
		}
		if optional {
			args = append(args, paramsArg)
		}
		result := "void"
		if m.HasContent() {
			result = ts.tsType(m.Response, "T.")
		}
		path, ambient := ts.tsPath(m.Path)
		usesAmbient = usesAmbient || ambient
		fmt.Fprintf(&b, "export %sfunction %s(%s): Promise<%s> {\n  return request('%s', %s%s)\n}\n\n",
			tsAsync(ambient), m.Name, strings.Join(args, ", "), result, m.Method, path, tsInit(m.Params, m.HasBody()))
	}
	b.WriteString("export const live = {\n" + strings.Join(liveTargets, "\n") + "\n}\n")

	imports := "request, target"
	if usesAmbient {
		imports = "ambientParam, request, target"
	}
	return generatedHeader + "import { " + imports + " } from './runtime'\nimport type * as T from './types'\n\n" + b.String()
}

// tsParams writes the parameter interface when the type has parameters and returns its
// name and the function argument declaring it.
func (ts *types) tsParams(b *strings.Builder, name string, t reflect.Type) (iface, arg string) {
	ps := params(t)
	if len(ps) == 0 {
		return "Record<string, never>", ""
	}
	fmt.Fprintf(b, "export interface %s {\n", name)
	required := false
	for _, p := range ps {
		optional := "?"
		if p.In == "path" && !ts.ambient[p.Name] {
			optional, required = "", true
		}
		fmt.Fprintf(b, "  %s%s: %s\n", p.Name, optional, strings.TrimSuffix(ts.tsType(p.Type, "T."), " | null"))
	}
	b.WriteString("}\n\n")
	if required {
		return name, "params: " + name
	}
	return name, "params: " + name + " = {}"
}

// tsPath renders a gin path as a template literal over params; ambient parameters fall
// back to config.ambient(). It reports whether the path has any.
func (ts *types) tsPath(path string) (string, bool) {
	ambient := false
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if !strings.HasPrefix(p, ":") {
			continue
		}
		if name := p[1:]; ts.ambient[name] {
			parts[i], ambient = "${ambientParam('"+name+"', params."+name+")}", true
		} else {
			parts[i] = "${encodeURIComponent(String(params." + name + "))}"
		}
	}
	return "`" + strings.Join(parts, "/") + "`", ambient
}

// tsAsync makes a function with ambient parameters async, so a missing one rejects like
// every other failure instead of throwing synchronously.
func tsAsync(ambient bool) string {
	if ambient {
		return "async "
	}
	return ""
}

// tsAmbient is a live target's trailing list of ambient path parameters.
func (ts *types) tsAmbient(t reflect.Type) string {
	var names []string
	for _, p := range params(t) {
		if p.In == "path" && ts.ambient[p.Name] {
			names = append(names, "'"+p.Name+"'")
		}
	}
	if len(names) == 0 {
		return ""
	}
	return ", [" + strings.Join(names, ", ") + "]"
}

// tsInit is request's init argument: the query parameters and the body.
func tsInit(t reflect.Type, body bool) string {
	var query, parts []string
	for _, p := range params(t) {
		if p.In == "query" {
			query = append(query, p.Name+": params."+p.Name)
		}
	}
	if len(query) > 0 {
		parts = append(parts, "query: { "+strings.Join(query, ", ")+" }")
	}
	if body {
		parts = append(parts, "body")
	}
	if len(parts) == 0 {
		return ""
	}
	return ", { " + strings.Join(parts, ", ") + " }"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
