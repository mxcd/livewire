// Package livewire declares an API once and serves it three ways: as REST routes, as live
// WebSocket subscriptions fed by Postgres LISTEN/NOTIFY, and as a generated TypeScript client
// plus OpenAPI document (package gen). The registry is the single source of truth: a
// resource names its DTO, its loader and the tables that make it stale; a mutation names its
// parameters, request and response types and its handler.
package livewire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
)

// Check authorizes a read or a write. The context carries whatever the application's auth
// middleware put into the request context; livewire never looks at it.
type Check func(ctx context.Context) error

// NoParams is the parameter type of a resource or mutation without parameters.
type NoParams struct{}

// NoBody is the request type of a mutation without a body.
type NoBody struct{}

// NoContent is the response type of a mutation that answers 204.
type NoContent struct{}

// Resource is a read: a list of items with an "id" field, or a single object. With Tables
// set it is live: a change to any of them re-runs the loader for every open subscription.
type Resource struct {
	Name   string // wire target and TS identifier, e.g. "todos"
	Path   string // REST path below the API base, e.g. "/todos"
	List   bool
	DTO    reflect.Type // the item type of a list, the object type otherwise
	Params reflect.Type
	Tables []string
	Read   Check
	load   func(ctx context.Context, params any) (any, error)
}

// Live says whether the resource can be subscribed to.
func (r *Resource) Live() bool { return len(r.Tables) > 0 }

// NewList declares a list resource. Every item must marshal to a JSON object with an "id".
func NewList[T any, P any](name, path string, tables []string, read Check, load func(ctx context.Context, params *P) ([]T, error)) *Resource {
	return &Resource{
		Name: name, Path: path, List: true, Tables: tables, Read: read,
		DTO: reflect.TypeFor[T](), Params: reflect.TypeFor[P](),
		load: func(ctx context.Context, params any) (any, error) {
			items, err := load(ctx, params.(*P))
			if items == nil {
				items = []T{}
			}
			return items, err
		},
	}
}

// NewObject declares a single-object resource.
func NewObject[T any, P any](name, path string, tables []string, read Check, load func(ctx context.Context, params *P) (*T, error)) *Resource {
	return &Resource{
		Name: name, Path: path, Tables: tables, Read: read,
		DTO: reflect.TypeFor[T](), Params: reflect.TypeFor[P](),
		load: func(ctx context.Context, params any) (any, error) { return load(ctx, params.(*P)) },
	}
}

// Mutation is a write. It is REST only: its effect reaches live subscribers through the
// tables it changes.
type Mutation struct {
	Name     string // TS identifier, e.g. "addTodo"
	Method   string
	Path     string
	Params   reflect.Type
	Request  reflect.Type
	Response reflect.Type
	Check    Check
	handle   func(ctx context.Context, params, body any) (any, error)
}

// NewMutation declares a write. Use NoParams, NoBody and NoContent where they apply; a
// NoContent response answers 204.
func NewMutation[P any, Req any, Resp any](name, method, path string, check Check, handle func(ctx context.Context, params *P, body *Req) (*Resp, error)) *Mutation {
	return &Mutation{
		Name: name, Method: method, Path: path, Check: check,
		Params: reflect.TypeFor[P](), Request: reflect.TypeFor[Req](), Response: reflect.TypeFor[Resp](),
		handle: func(ctx context.Context, params, body any) (any, error) {
			return handle(ctx, params.(*P), body.(*Req))
		},
	}
}

// HasBody says whether the mutation reads a request body.
func (m *Mutation) HasBody() bool { return m.Request != reflect.TypeFor[NoBody]() }

// Status is the success status: 204 for NoContent, 200 otherwise.
func (m *Mutation) Status() int {
	if m.Response == reflect.TypeFor[NoContent]() {
		return http.StatusNoContent
	}
	return http.StatusOK
}

// Registry holds every declaration.
type Registry struct {
	Resources []*Resource
	Mutations []*Mutation
}

// Add appends declarations; a duplicate name or route panics, it is a programming error.
func (r *Registry) Add(items ...any) {
	for _, item := range items {
		switch v := item.(type) {
		case *Resource:
			if r.Resource(v.Name) != nil {
				panic(fmt.Sprintf("livewire: duplicate resource %q", v.Name))
			}
			r.Resources = append(r.Resources, v)
		case *Mutation:
			for _, m := range r.Mutations {
				if m.Name == v.Name || (m.Method == v.Method && m.Path == v.Path) {
					panic(fmt.Sprintf("livewire: duplicate mutation %q %s %s", v.Name, v.Method, v.Path))
				}
			}
			r.Mutations = append(r.Mutations, v)
		default:
			panic(fmt.Sprintf("livewire: cannot register %T", item))
		}
	}
}

// Resource returns the resource with the given name, or nil.
func (r *Registry) Resource(name string) *Resource {
	for _, res := range r.Resources {
		if res.Name == name {
			return res
		}
	}
	return nil
}

// Error is the error body of every REST response and WebSocket error frame.
type Error struct {
	Status  int               `json:"-"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an Error. Codes are stable identifiers the UI translates.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Common error codes.
const (
	CodeInvalidRequest = "invalid_request"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeInternal       = "internal"
)

// asError maps any error to an Error; unknown errors are internal and keep their text out
// of the response.
func asError(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "Internal error"}
}
