package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type tenantParams struct {
	Tenant string `path:"tenant"`
}

func (p *tenantParams) tenantID() string { return p.Tenant }

type propertyParams struct {
	tenantParams
	Property string `path:"property"`
}

// bookingParams embeds two levels deep, through unexported types.
type bookingParams struct {
	propertyParams
	ID    string `path:"id"`
	Limit *int   `query:"limit"`
}

type booking struct {
	ID       string `json:"id"`
	Tenant   string `json:"tenant"`
	Property string `json:"property"`
	Limit    int    `json:"limit"`
}

type tenantKey struct{}

var errNoTenant = errors.New("no such tenant")

// hooked is a registry with every hook set: the tenant comes from the path into the
// context, an unknown tenant is a 404 through Errors.
func hooked() *Registry {
	r := &Registry{
		Context: func(ctx context.Context, params any) (context.Context, error) {
			p, ok := params.(interface{ tenantID() string })
			if !ok {
				return ctx, nil
			}
			if p.tenantID() != "acme" {
				return nil, errNoTenant
			}
			return context.WithValue(ctx, tenantKey{}, p.tenantID()), nil
		},
		Errors: func(err error) *Error {
			if errors.Is(err, errNoTenant) {
				return Errorf(http.StatusNotFound, CodeNotFound, "No such tenant")
			}
			return nil
		},
	}
	needsTenant := func(ctx context.Context) error {
		if ctx.Value(tenantKey{}) == nil {
			return errors.New("the check ran before the Context hook")
		}
		return nil
	}
	load := func(ctx context.Context, p *bookingParams) (*booking, error) {
		b := &booking{ID: p.ID, Tenant: ctx.Value(tenantKey{}).(string), Property: p.Property}
		if p.Limit != nil {
			b.Limit = *p.Limit
		}
		return b, nil
	}
	r.Add(
		NewObject("booking", "/t/:tenant/p/:property/bookings/:id", []string{"bookings"}, needsTenant, load).
			Use(func(c *gin.Context) { c.Header("X-Middleware", "resource") }),
		NewMutation("confirm", http.MethodPost, "/t/:tenant/p/:property/bookings/:id", needsTenant,
			func(ctx context.Context, p *bookingParams, _ *NoBody) (*booking, error) { return load(ctx, p) }).
			WithStatus(http.StatusCreated).
			Use(func(c *gin.Context) { c.Header("X-Middleware", "mutation") }),
		NewMutation("remove", http.MethodDelete, "/t/:tenant/p/:property/bookings/:id", needsTenant,
			func(context.Context, *bookingParams, *NoBody) (*NoContent, error) { return nil, nil }).
			WithStatus(http.StatusAccepted),
		NewMutation("rename", http.MethodPut, "/t/:tenant/p/:property/bookings/:id",
			func(context.Context) error { return Errorf(http.StatusForbidden, CodeForbidden, "No") },
			func(context.Context, *bookingParams, *struct {
				Title string `json:"title" binding:"required"`
			}) (*booking, error) {
				return nil, nil
			}),
	)
	return r
}

func serve(r *Registry, method, path string) *httptest.ResponseRecorder {
	return serveBody(r, method, path, "")
}

func serveBody(r *Registry, method, path, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	r.Mount(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestREST(t *testing.T) {
	r := hooked()

	w := serve(r, http.MethodGet, "/t/acme/p/sea/bookings/b1?limit=3")
	var got booking
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got != (booking{"b1", "acme", "sea", 3}) || w.Header().Get("X-Middleware") != "resource" {
		t.Fatalf("get: %d %s %v", w.Code, w.Body, w.Header())
	}

	w = serve(r, http.MethodGet, "/t/other/p/sea/bookings/b1")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Fatalf("unknown tenant: %d %s", w.Code, w.Body)
	}

	w = serve(r, http.MethodPost, "/t/acme/p/sea/bookings/b1")
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"tenant":"acme"`) || w.Header().Get("X-Middleware") != "mutation" {
		t.Fatalf("mutation: %d %s", w.Code, w.Body)
	}

	w = serve(r, http.MethodDelete, "/t/acme/p/sea/bookings/b1")
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("no content: %d %q", w.Code, w.Body)
	}

	// The check runs before the body is read: no access answers 403 whatever the body.
	w = serveBody(r, http.MethodPut, "/t/acme/p/sea/bookings/b1", "{not json")
	if w.Code != http.StatusForbidden {
		t.Fatalf("check before body: %d %s", w.Code, w.Body)
	}

	var rendered error
	r.RenderError = func(c *gin.Context, err error) {
		rendered = err
		c.String(http.StatusTeapot, "custom")
	}
	w = serve(r, http.MethodDelete, "/t/other/p/sea/bookings/b1")
	if w.Code != http.StatusTeapot || !errors.Is(rendered, errNoTenant) {
		t.Fatalf("render error: %d %v", w.Code, rendered)
	}

	plain := NewMutation("plain", http.MethodDelete, "/x", nil,
		func(context.Context, *NoParams, *NoBody) (*NoContent, error) { return nil, nil })
	if plain.Status() != http.StatusNoContent || plain.HasContent() {
		t.Fatalf("NoContent default: %d", plain.Status())
	}
}

// TestLiveHooks subscribes over a real socket: the Context hook feeds the loader, Errors
// maps a failed subscribe and Partition sees the decoded params.
func TestLiveHooks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := NewEngine(&EngineOptions{Registry: hooked(), Partition: func(params any) string {
		return params.(*bookingParams).Tenant
	}})
	router := gin.New()
	router.GET("/ws", engine.Handler())
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	read := func() (frame struct {
		ResponseFrame
		PushFrame
	}) {
		t.Helper()
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}

	_ = conn.WriteJSON(ClientFrame{ID: "1", Op: OpSubscribe, Target: "booking", Params: map[string]string{"tenant": "acme", "property": "sea", "id": "b1"}})
	if f := read(); !f.OK {
		t.Fatalf("subscribe: %+v", f.ResponseFrame)
	}
	if f := read(); f.Kind != PushReplace || !strings.Contains(string(f.Payload), `"tenant":"acme"`) {
		t.Fatalf("push: %+v", f.PushFrame)
	}
	var partition string
	engine.each(func(s *subscription) { partition = s.partition })
	if partition != "acme" {
		t.Fatalf("partition %q", partition)
	}

	_ = conn.WriteJSON(ClientFrame{ID: "2", Op: OpSubscribe, Target: "booking", Params: map[string]string{"tenant": "other", "property": "sea", "id": "b1"}})
	if f := read(); f.OK || f.Error == nil || f.Error.Code != CodeNotFound {
		t.Fatalf("unknown tenant: %+v", f.ResponseFrame)
	}
}

func TestCheckParams(t *testing.T) {
	type unexported struct {
		id string `path:"id"`
	}
	type unsupported struct {
		At float64 `query:"at"`
	}
	type pointerEmbed struct {
		*tenantParams
	}
	type pathList struct {
		IDs []string `path:"ids"`
	}
	for name, declare := range map[string]func(){
		"parameter id is the unexported field": func() {
			NewObject("x", "/x/:id", nil, nil, func(context.Context, *unexported) (*booking, error) { return nil, nil })
		},
		"parameter at has the unsupported type float64": func() {
			NewList("x", "/x", nil, nil, func(context.Context, *unsupported) ([]booking, error) { return nil, nil })
		},
		"parameter ids has the unsupported type []string": func() {
			NewObject("x", "/x/:ids", nil, nil, func(context.Context, *pathList) (*booking, error) { return nil, nil })
		},
		"embed params structs by value": func() {
			NewMutation("x", http.MethodPost, "/x/:tenant", nil, func(context.Context, *pointerEmbed, *NoBody) (*NoContent, error) { return nil, nil })
		},
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), name) {
					t.Errorf("want a panic with %q, got %v", name, r)
				}
			}()
			declare()
		}()
	}
}

type status string

type listParams struct {
	Status []string `query:"status"`
	Kinds  []status `query:"kind"`
}

// TestListParams decodes a string-slice query parameter from repeated keys, commas and a
// mix of both on REST, and from one comma-separated value on a live subscribe.
func TestListParams(t *testing.T) {
	r := &Registry{}
	r.Add(NewList("items", "/items", nil, func(context.Context) error { return nil },
		func(_ context.Context, p *listParams) ([]item, error) {
			var items []item
			for _, s := range p.Status {
				items = append(items, item{ID: s, Name: fmt.Sprint(p.Kinds, p.Status == nil)})
			}
			return items, nil
		}))
	for query, want := range map[string]string{
		"status=a&status=b":           `[{"id":"a","name":"[] false"},{"id":"b","name":"[] false"}]`,
		"status=a,b&kind=x":           `[{"id":"a","name":"[x] false"},{"id":"b","name":"[x] false"}]`,
		"status=a,,b&status=c&kind=,": `[{"id":"a","name":"[] false"},{"id":"b","name":"[] false"},{"id":"c","name":"[] false"}]`,
		"other=1":                     `[]`,
	} {
		if w := serve(r, http.MethodGet, "/items?"+query); w.Body.String() != want {
			t.Errorf("?%s: %d %s, want %s", query, w.Code, w.Body, want)
		}
	}

	params, err := decodeParams(reflect.TypeFor[listParams](), mapSource(map[string]string{"status": "a,b", "kind": "x,y"}))
	if p := params.(*listParams); err != nil || !slices.Equal(p.Status, []string{"a", "b"}) || !slices.Equal(p.Kinds, []status{"x", "y"}) {
		t.Fatalf("live: %+v %v", params, err)
	}
	params, _ = decodeParams(reflect.TypeFor[listParams](), mapSource(map[string]string{"status": ""}))
	if p := params.(*listParams); p.Status != nil {
		t.Fatalf("an empty value decoded to %#v", p.Status)
	}
}
