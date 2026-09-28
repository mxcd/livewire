package gen_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mxcd/livewire"
	"github.com/mxcd/livewire/gen"
)

type todo struct {
	ID        string                `json:"id"`
	Title     string                `json:"title"`
	Note      string                `json:"note,omitempty"`
	DueAt     *time.Time            `json:"dueAt"`
	State     string                `json:"state" enum:"open,done"`
	Labels    []string              `json:"labels"`
	Meta      map[string]string     `json:"meta,omitempty"`
	Assignees []person              `json:"assignees,omitempty"`
	Priority  priority              `json:"priority"`
	Previous  *priority             `json:"previous"`
	History   map[string][]priority `json:"history,omitempty"`
	Urgency   priority              `json:"urgency" enum:"now,later"`
}

// priority is listed in Options.Enums; the tag on todo.Urgency still wins.
type priority string

type tenantParams struct {
	Tenant string `path:"tenant"`
}

// scopedParams and archiveParams embed the ambient tenant.
type scopedParams struct {
	tenantParams
	ID string `path:"id"`
}

type archiveParams struct {
	tenantParams
	Before   *string   `query:"before"`
	Priority *priority `query:"priority"`
}

type Page[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

type person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type board struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type listParams struct {
	Board string   `path:"board"`
	State *string  `query:"state"`
	Limit *int     `query:"limit"`
	Tags  []string `query:"tag"`
}

type idParams struct {
	ID string `path:"id"`
}

type addTodo struct {
	Title string  `json:"title" binding:"required"`
	Note  *string `json:"note,omitempty"`
}

func allow(context.Context) error { return nil }

// registry exercises every shape the generator emits: a live list with path and optional
// query parameters, an object, a mutation with a body and one answering 204, optional,
// nullable, enum, map and nested fields, an ambient tenant, a listed enum, a generic DTO and
// mutations with query parameters and custom success statuses.
func registry() *livewire.Registry {
	r := &livewire.Registry{}
	r.Add(
		livewire.NewList("todos", "/boards/:board/todos", []string{"todos"}, allow,
			func(context.Context, *listParams) ([]todo, error) { return nil, nil }),
		livewire.NewObject("board", "/board", []string{"boards"}, allow,
			func(context.Context, *livewire.NoParams) (*board, error) { return &board{}, nil }),
		livewire.NewList("labels", "/labels", nil, allow,
			func(context.Context, *livewire.NoParams) ([]person, error) { return nil, nil }),
		livewire.NewMutation("addTodo", http.MethodPost, "/boards/:board/todos", allow,
			func(context.Context, *listParams, *addTodo) (*todo, error) { return &todo{}, nil }),
		livewire.NewMutation("deleteTodo", http.MethodDelete, "/todos/:id", allow,
			func(context.Context, *idParams, *livewire.NoBody) (*livewire.NoContent, error) { return nil, nil }),
		livewire.NewObject("page", "/tenants/:tenant/page", []string{"todos"}, allow,
			func(context.Context, *tenantParams) (*Page[todo], error) { return &Page[todo]{}, nil }),
		livewire.NewMutation("archiveTodos", http.MethodPost, "/tenants/:tenant/archive", allow,
			func(context.Context, *archiveParams, *addTodo) (*Page[todo], error) { return &Page[todo]{}, nil }),
		livewire.NewMutation("purgeTodos", http.MethodDelete, "/tenants/:tenant/todos", allow,
			func(context.Context, *archiveParams, *livewire.NoBody) (*livewire.NoContent, error) { return nil, nil }).
			WithStatus(http.StatusAccepted),
		livewire.NewMutation("renameTodo", http.MethodPut, "/tenants/:tenant/todos/:id", allow,
			func(context.Context, *scopedParams, *addTodo) (*todo, error) { return &todo{}, nil }).
			WithStatus(http.StatusCreated),
	)
	return r
}

func generate(t *testing.T, dir string) {
	t.Helper()
	if err := gen.Generate(registry(), gen.Options{
		TSDir: filepath.Join(dir, "src", "api"), OpenAPIFile: filepath.Join(dir, "openapi.json"),
		Title: "Test", Version: "v1", BasePath: "/api/v1",
		SecuritySchemes: map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
		AmbientParams:   []string{"tenant"},
		Enums:           []gen.EnumValues{gen.Enum[priority]("low", "high")},
	}); err != nil {
		t.Fatal(err)
	}
}

// usage calls the generated client the way an application does, so the check covers the
// signatures as well as the generated files themselves.
const usage = `import { addTodo, archiveTodos, deleteTodo, getBoard, getPage, getTodos, live, purgeTodos, renameTodo } from './api/api'
import { ApiError, config, NetworkError } from './api/runtime'
import type { PageTodo, priority } from './api/types'
import { useLive, useLiveStatus } from './api/vue'

config.ambient = () => ({ tenant: 'acme' })
config.onError = (error, request) => {
  if (error instanceof ApiError) console.log(error.status, error.code, error.body, request.method, request.path)
  if (error instanceof NetworkError) console.log(error.message)
}

export async function tenanted(): Promise<void> {
  const page: PageTodo = await getPage()
  await getPage({ tenant: 'other' })
  await archiveTodos({ title: 'Old' })
  const archived = await archiveTodos({ title: 'Old' }, { before: '2026-01-01', priority: 'high' })
  await purgeTodos()
  const nothing: void = await purgeTodos({ tenant: 'acme', before: '2026-01-01' })
  const renamed = await renameTodo({ id: 'a' }, { title: 'New' })
  const p: priority = renamed.priority
  const previous: priority | null = renamed.previous
  const urgency: 'now' | 'later' = renamed.urgency
  const history: priority[] | undefined = renamed.history?.['2026']
  const live1 = useLive(live.page)
  const live2 = useLive(live.page, { tenant: 'other' })
  console.log(page.total, archived.items[0]?.title, nothing, p, previous, urgency, history, live1.data.value?.total, live2.stale.value)
}

export async function run(): Promise<void> {
  const todos = await getTodos({ board: 'home' })
  const open = await getTodos({ board: 'home', state: 'open', limit: 10, tag: ['a', 'b'] })
  const tags: string[] | undefined = ({} as Parameters<typeof getTodos>[0]).tag
  const created = await addTodo({ board: 'home' }, { title: 'Milk' })
  await addTodo({ board: 'home' }, { title: 'Bread', note: null })
  await deleteTodo({ id: created.id })
  const board = await getBoard()
  console.log(todos.length, open.length, board.count, tags)
  useLive(live.todos, { board: 'home', tag: ['a'] })
  const { data, stale, error } = useLive(live.todos, { board: 'home' }, { cache: true })
  const object = useLive(live.board)
  const status = useLiveStatus()
  console.log(data.value?.[0]?.title, stale.value, error.value?.code, object.data.value?.name, status.value)
}
`

// TestGeneratedClientTypeChecks compiles the generated TypeScript with strict,
// exactOptionalPropertyTypes and noUncheckedIndexedAccess, the settings a Quasar or Vite
// application ships with. It needs bun (and the network for the first install).
func TestGeneratedClientTypeChecks(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed; CI runs this test")
	}
	dir := t.TempDir()
	for _, name := range []string{"package.json", "bun.lock", "tsconfig.json"} {
		content, err := os.ReadFile(filepath.Join("testdata", "tsc", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	generate(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "src", "usage.ts"), []byte(usage), 0o644); err != nil {
		t.Fatal(err)
	}
	behavior, err := os.ReadFile(filepath.Join("testdata", "tsc", "behavior.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "behavior.ts"), behavior, 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bun, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bun %v: %v\n%s", args, err, out)
		}
	}
	run("install", "--frozen-lockfile")
	run("x", "tsc", "-p", ".")
	run("run", "src/behavior.ts")
}

// TestGeneratedShapes checks what the type check cannot see: the OpenAPI document and the
// argument order of the generated functions. It needs no bun.
func TestGeneratedShapes(t *testing.T) {
	dir := t.TempDir()
	generate(t, dir)
	api, err := os.ReadFile(filepath.Join(dir, "src", "api", "api.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export async function archiveTodos(body: T.addTodo, params: ArchiveTodosParams = {}): Promise<T.PageTodo> {\n  return request('POST', `/tenants/${ambientParam('tenant', params.tenant)}/archive`, { query: { before: params.before, priority: params.priority }, body })",
		"export async function purgeTodos(params: PurgeTodosParams = {}): Promise<void> {",
		"export async function renameTodo(params: RenameTodoParams, body: T.addTodo): Promise<T.todo> {",
		"  priority?: T.priority\n",
		"page: target<T.PageTodo, PageParams>('page', false, ['tenant']),",
		"export function addTodo(params: AddTodoParams, body: T.addTodo): Promise<T.todo> {\n  return request('POST', `/boards/${encodeURIComponent(String(params.board))}/todos`, { query: { state: params.state, limit: params.limit, tag: params.tag }, body })",
		"  tag?: string[]\n",
	} {
		if !strings.Contains(string(api), want) {
			t.Errorf("api.ts lacks %s", want)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct{ Name, In string }
			Responses  map[string]struct{ Content map[string]any }
		}
		Components struct{ Schemas map[string]json.RawMessage }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Paths map[string]map[string]struct{ Parameters []map[string]any }
	}
	_ = json.Unmarshal(raw, &generic)
	if ps := generic.Paths["/boards/{board}/todos"]["get"].Parameters; len(ps) != 4 || ps[3]["name"] != "tag" || ps[3]["style"] != "form" ||
		ps[3]["explode"] != true || fmt.Sprint(ps[3]["schema"]) != "map[items:map[type:string] type:array]" {
		t.Errorf("list query parameter: %v", ps)
	}
	archive := doc.Paths["/tenants/{tenant}/archive"]["post"]
	if len(archive.Parameters) != 3 || archive.Parameters[1].Name != "before" || archive.Parameters[1].In != "query" {
		t.Errorf("archive parameters: %+v", archive.Parameters)
	}
	if r, ok := doc.Paths["/tenants/{tenant}/todos/{id}"]["put"].Responses["201"]; !ok || r.Content == nil {
		t.Error("renameTodo does not answer 201 with content")
	}
	if r, ok := doc.Paths["/tenants/{tenant}/todos"]["delete"].Responses["202"]; !ok || r.Content != nil {
		t.Error("purgeTodos does not answer 202 without content")
	}
	for _, c := range [][2]string{
		{"priority", `{"enum":["low","high"],"type":"string"}`},
		{"PageTodo", `"items":{"items":{"$ref":"#/components/schemas/todo"},"type":"array"}`},
		{"todo", `"previous":{"anyOf":[{"$ref":"#/components/schemas/priority"},{"type":"null"}]}`},
		{"todo", `"urgency":{"enum":["now","later"],"type":"string"}`},
	} {
		var compact strings.Builder
		_ = json.NewEncoder(&compact).Encode(doc.Components.Schemas[c[0]])
		if !strings.Contains(compact.String(), c[1]) {
			t.Errorf("schema %s = %s, want %s", c[0], compact.String(), c[1])
		}
	}
}
