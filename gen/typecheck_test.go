package gen_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mxcd/livewire"
	"github.com/mxcd/livewire/gen"
)

type todo struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Note      string            `json:"note,omitempty"`
	DueAt     *time.Time        `json:"dueAt"`
	State     string            `json:"state" enum:"open,done"`
	Labels    []string          `json:"labels"`
	Meta      map[string]string `json:"meta,omitempty"`
	Assignees []person          `json:"assignees,omitempty"`
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
	Board string  `path:"board"`
	State *string `query:"state"`
	Limit *int    `query:"limit"`
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
// nullable, enum, map and nested fields.
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
	)
	return r
}

// usage calls the generated client the way an application does, so the check covers the
// signatures as well as the generated files themselves.
const usage = `import { addTodo, deleteTodo, getBoard, getTodos, live } from './api/api'
import { useLive, useLiveStatus } from './api/vue'

export async function run(): Promise<void> {
  const todos = await getTodos({ board: 'home' })
  const open = await getTodos({ board: 'home', state: 'open', limit: 10 })
  const created = await addTodo({ board: 'home' }, { title: 'Milk' })
  await addTodo({ board: 'home' }, { title: 'Bread', note: null })
  await deleteTodo({ id: created.id })
  const board = await getBoard()
  console.log(todos.length, open.length, board.count)
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
	if err := gen.Generate(registry(), gen.Options{
		TSDir: filepath.Join(dir, "src", "api"), OpenAPIFile: filepath.Join(dir, "openapi.json"),
		Title: "Test", Version: "v1", BasePath: "/api/v1",
		SecuritySchemes: map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "usage.ts"), []byte(usage), 0o644); err != nil {
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
}
