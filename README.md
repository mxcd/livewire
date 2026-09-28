# livewire

Declare an API once in Go and serve it three ways:

- as **REST routes** on [gin](https://github.com/gin-gonic/gin),
- as **live WebSocket subscriptions** fed by PostgreSQL `LISTEN/NOTIFY`, so every open screen
  sees a change the moment it commits,
- as a generated **TypeScript client** (types, typed REST functions, a reconnecting live
  connection, Vue composables) plus an **OpenAPI 3.1** document.

The registry is the single source of truth: a resource names its DTO, its loader and the tables
that make it stale; a mutation names its parameters, request and response types and its
handler. livewire never sees your auth model: checks are plain functions of the request context
your middleware already filled.

```sh
go get github.com/mxcd/livewire
```

## Declarations

```go
// A read. Items marshal to JSON objects with an "id". Tables non-empty means live.
livewire.NewList[T, P](name, path string, tables []string, read Check, load func(ctx, *P) ([]T, error))
livewire.NewObject[T, P](name, path string, tables []string, read Check, load func(ctx, *P) (*T, error))

// A write, REST only: its effect reaches subscribers through the tables it changes.
livewire.NewMutation[P, Req, Resp](name, method, path string, check Check, handle func(ctx, *P, *Req) (*Resp, error))
```

- `Check` is `func(ctx context.Context) error`; return a `*livewire.Error` (for example
  `livewire.Errorf(http.StatusForbidden, livewire.CodeForbidden, "...")`) to answer with that
  status and code. Any other error is a 500 whose text stays out of the response.
- Parameters are a struct with `path:"id"` and `query:"state"` tags (string, int, bool and
  pointers to them, and string slices for query parameters, read from repeated keys and
  comma-separated values alike, `?status=a&status=b` or `?status=a,b`); embedded structs are
  flattened, so `BookingParams` can embed
  `PropertyParams`, which embeds `TenantParams`. `livewire.NoParams`, `livewire.NoBody` and
  `livewire.NoContent` (no body, 204 by default) fill the slots a declaration does not use.
- `mutation.WithStatus(http.StatusCreated)` sets the success status; `.Use(middleware...)` on a
  resource or mutation puts gin middleware in front of its REST route. Live subscriptions do
  not run it, so authorization belongs in the checks.
- Request bodies are validated with gin's `binding` tags; a failure answers
  `{code: "invalid_request", message, fields}` with the fields named as in the JSON body. The
  check runs first, so a caller without access gets the check's error whatever it sends.
- A params struct decoding cannot fill (an unexported or unsupported parameter field, an
  embedded pointer) panics when it is declared.
- An `enum:"a,b"` tag on a string field becomes a union type in TypeScript and an enum in
  OpenAPI.

## Registry hooks

```go
registry := &livewire.Registry{
	// Derive the context checks and loaders see from the decoded *P, e.g. resolve the tenant.
	Context: func(ctx context.Context, params any) (context.Context, error) { ... },
	// Map any error (hook, check, loader, handler, params, body) to the answer.
	Errors: func(err error) *livewire.Error { ... },
	// Write REST errors yourself (live frames still use Errors).
	RenderError: func(c *gin.Context, err error) { ... },
}
```

`Context` runs on every REST request, live subscribe and live re-run (after `Revalidate`); an
error aborts like a failed check.

## Live engine

```go
livewire.InstallTriggers(ctx, sqlDB, livewire.DefaultChannel, "todos", "boards")
engine := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, DSN: databaseURL})
go engine.Run(ctx)
api.GET("/ws", engine.Handler()) // behind the same authentication as the REST routes
```

- `InstallTriggers` puts one generic trigger on each table; it notifies `{table, op, id}` and is
  idempotent, so it runs on every start after the migrations. Leave out tables with large
  binary columns.
- One dedicated `LISTEN` connection, reconnecting with backoff. After every successful `LISTEN`,
  the first one included, every subscription gets a fresh snapshot, so no change between
  subscribe and listen, or during a disconnect, is lost.
- Each subscription re-runs its loader when one of its tables changes (bursts coalesce into one
  re-run) and pushes a `snapshot`, then `diff`s of upserted and removed items (with the order
  when membership or order changed), or a `replace` for objects. A failing re-run retries with
  backoff; a 4xx ends the subscription with an `error` push.
- `EngineOptions.Revalidate` refreshes the socket's identity before every subscribe and re-run,
  so a revoked key or a lost role takes effect on open sockets; `Engine.CloseWhere` closes
  sockets on demand.
- Multi-tenant fan-out: `InstallPartitionedTriggers(ctx, db, channel, "tenant_id", tables...)`
  adds the row's column to the notification as `partition`, and `EngineOptions.Partition`
  names each subscription's partition from its params. A change then only re-runs
  subscriptions of its own partition; an empty partition on either side matches all.

## Generator

```go
gen.Generate(registry, gen.Options{
	TSDir:       "ui/src/api/generated",
	OpenAPIFile: "api/openapi.json",
	Title:       "Todo", Version: "v1", BasePath: "/api/v1",
	SecuritySchemes: map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
	AmbientParams:   []string{"tenant"}, // path params calls may leave out
	Enums:           []gen.EnumValues{gen.Enum(model.StatusOpen, model.StatusDone)},
})
```

It writes `types.ts`, `api.ts` (one function per route and a `live` object of subscription
targets), `runtime.ts` (fetch wrapper with `ApiError`/`NetworkError`, one reconnecting
`LiveConnection` that resubscribes everything) and `vue.ts` (`useLive(target, params, {cache})`
with an optional localStorage snapshot, `useLiveStatus()`). The output type-checks with
`strict`, `exactOptionalPropertyTypes` and `noUncheckedIndexedAccess`; the test suite compiles
it with `tsc` to keep it that way.

- Ambient path parameters may be left out of a call: the client fills them from
  `config.ambient()` (and rejects naming the parameter when that has none either), and `useLive`
  merges them under the explicit params.
- Fields follow encoding/json: of promoted fields with one JSON name the shallowest wins,
  `omitempty` only makes a field optional when encoding/json can omit it (not on structs,
  `time.Time` or `uuid.UUID`), and slices and maps of pointers are assumed to hold no nil
  (`T[]`).
- `useLive` also returns `retry()`, which resubscribes after an error ended the
  subscription; a socket closed as unauthorized (4401) calls `config.onUnauthorized()`.
- Listed enums become named union types (`export type Status = 'open' | 'done'`) and OpenAPI
  enums; generic types get plain names (`ListResponse[model.Booking]` is `ListResponseBooking`).
- Mutations send their query parameters; optional params trail the body, so
  `archive(body)` and `archive(body, { before })` both work.
- `config.onError(error, { method, path })` sees every failed request before it throws, and
  `ApiError.body` keeps the parsed error body.

## Minimal example

```go
type Todo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

type AddTodo struct {
	Title string `json:"title" binding:"required"`
}

func api(db *pgxpool.Pool) *livewire.Registry {
	allow := func(context.Context) error { return nil }
	r := &livewire.Registry{}
	r.Add(
		livewire.NewList("todos", "/todos", []string{"todos"}, allow,
			func(ctx context.Context, _ *livewire.NoParams) ([]Todo, error) {
				rows, _ := db.Query(ctx, "SELECT id, title, done FROM todos ORDER BY title")
				return pgx.CollectRows(rows, pgx.RowToStructByName[Todo])
			}),
		livewire.NewMutation("addTodo", http.MethodPost, "/todos", allow,
			func(ctx context.Context, _ *livewire.NoParams, in *AddTodo) (*Todo, error) {
				todo := Todo{ID: uuid.NewString(), Title: in.Title}
				_, err := db.Exec(ctx, "INSERT INTO todos (id, title) VALUES ($1, $2)", todo.ID, todo.Title)
				return &todo, err
			}),
	)
	return r
}
```

`registry.Mount(router.Group("/api/v1"))` serves `GET /api/v1/todos` and `POST /api/v1/todos`.
In a Vue component:

```ts
import { addTodo, live } from './api/generated/api'
import { useLive } from './api/generated/vue'

const { data: todos, stale } = useLive(live.todos)
await addTodo({ title: 'Buy milk' }) // every open list updates through the socket
```

## License

MIT
