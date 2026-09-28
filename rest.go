package livewire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
	"github.com/rs/zerolog/log"
)

// Mount registers every resource as a GET route and every mutation under its method.
func (r *Registry) Mount(group gin.IRoutes) {
	jsonFieldNames.Do(func() {
		if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
			v.RegisterTagNameFunc(func(f reflect.StructField) string {
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if name == "-" {
					return ""
				}
				return name
			})
		}
	})
	for _, res := range r.Resources {
		res := res
		group.GET(res.Path, append(slices.Clone(res.Middleware), func(c *gin.Context) {
			params, err := decodeParams(res.Params, ginSource(c))
			if err == nil {
				var payload any
				if payload, err = r.read(c.Request.Context(), res, params); err == nil {
					c.JSON(http.StatusOK, payload)
					return
				}
			}
			r.writeError(c, err)
		})...)
	}
	for _, m := range r.Mutations {
		m := m
		group.Handle(m.Method, m.Path, append(slices.Clone(m.Middleware), func(c *gin.Context) {
			if err := r.serve(c, m); err != nil {
				r.writeError(c, err)
			}
		})...)
	}
}

// Get authorizes and loads a resource. It does not run Registry.Context.
func (r *Resource) Get(ctx context.Context, params any) (any, error) {
	if err := r.Read(ctx); err != nil {
		return nil, err
	}
	return r.load(ctx, params)
}

// read derives the context through the Context hook, then authorizes and loads.
func (r *Registry) read(ctx context.Context, res *Resource, params any) (any, error) {
	ctx, err := r.context(ctx, params)
	if err != nil {
		return nil, err
	}
	return res.Get(ctx, params)
}

func (r *Registry) serve(c *gin.Context, m *Mutation) error {
	params, err := decodeParams(m.Params, ginSource(c))
	if err != nil {
		return err
	}
	ctx, err := r.context(c.Request.Context(), params)
	if err != nil {
		return err
	}
	// The check comes before the body, so a caller without access never learns more than
	// that, whatever it sends.
	if err := m.Check(ctx); err != nil {
		return err
	}
	body := reflect.New(m.Request).Interface()
	if m.HasBody() {
		if err := c.ShouldBindWith(body, binding.JSON); err != nil {
			return bindError(err)
		}
	}
	response, err := m.handle(ctx, params, body)
	if err != nil {
		return err
	}
	if !m.HasContent() {
		c.Status(m.Status())
		return nil
	}
	c.JSON(m.Status(), response)
	return nil
}

func (r *Registry) writeError(c *gin.Context, err error) {
	if r.RenderError != nil {
		r.RenderError(c, err)
		return
	}
	e := r.asError(err)
	if e.Status >= http.StatusInternalServerError {
		log.Error().Err(err).Str("path", c.FullPath()).Msg("request failed")
	}
	c.AbortWithStatusJSON(e.Status, e)
}

func bindError(err error) *Error {
	e := Errorf(http.StatusBadRequest, CodeInvalidRequest, "The request is not valid")
	var validation validator.ValidationErrors
	if errors.As(err, &validation) {
		e.Fields = map[string]string{}
		for _, fe := range validation {
			e.Fields[fe.Field()] = fe.Tag()
		}
	} else if errors.Is(err, io.EOF) {
		e.Message = "The request body is missing"
	}
	return e
}

// paramSource looks a parameter up by its tag kind ("path" or "query") and name; a query
// key may repeat.
type paramSource func(kind, name string) ([]string, bool)

func ginSource(c *gin.Context) paramSource {
	return func(kind, name string) ([]string, bool) {
		if kind == "path" {
			v := c.Param(name)
			return []string{v}, v != ""
		}
		return c.GetQueryArray(name)
	}
}

// mapSource reads the params of a live subscribe frame.
func mapSource(params map[string]string) paramSource {
	return func(_, name string) ([]string, bool) {
		v, ok := params[name]
		return []string{v}, ok
	}
}

// decodeParams fills a new value of t from fields tagged `path:"name"` or `query:"name"`,
// embedded structs flattened. Supported kinds are string, int, bool and pointers to them,
// plus string slices for query parameters: every value of a repeated key, each split on
// commas, empty items dropped. An absent parameter stays zero or nil.
func decodeParams(t reflect.Type, source paramSource) (any, error) {
	out := reflect.New(t)
	if err := decodeFields(out.Elem(), source); err != nil {
		return nil, err
	}
	return out.Interface(), nil
}

func decodeFields(v reflect.Value, source paramSource) error {
	for i := 0; i < v.NumField(); i++ {
		field := v.Type().Field(i)
		kind, name := paramTag(field)
		if name == "" {
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				if err := decodeFields(v.Field(i), source); err != nil {
					return err
				}
			}
			continue
		}
		values, ok := source(kind, name)
		if !ok || len(values) == 0 {
			continue
		}
		target := v.Field(i)
		if target.Kind() == reflect.Slice {
			for _, value := range values {
				for _, item := range strings.Split(value, ",") {
					if item != "" {
						target.Set(reflect.Append(target, reflect.ValueOf(item).Convert(target.Type().Elem())))
					}
				}
			}
			continue
		}
		raw := values[0]
		if raw == "" {
			continue
		}
		if target.Kind() == reflect.Pointer {
			target.Set(reflect.New(target.Type().Elem()))
			target = target.Elem()
		}
		switch target.Kind() {
		case reflect.String:
			target.SetString(raw)
		case reflect.Int:
			n, err := strconv.Atoi(raw)
			if err != nil {
				return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "A parameter is not valid", Fields: map[string]string{name: "int"}}
			}
			target.SetInt(int64(n))
		case reflect.Bool:
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "A parameter is not valid", Fields: map[string]string{name: "bool"}}
			}
			target.SetBool(b)
		default:
			panic(fmt.Sprintf("livewire: parameter %s has the unsupported type %s", name, target.Type()))
		}
	}
	return nil
}

// checkParams panics when decodeParams could not fill t, so a bad params struct fails at
// declaration time instead of on the first request that sets the parameter.
func checkParams(t reflect.Type) {
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("livewire: params type %s is not a struct", t))
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		kind, name := paramTag(f)
		base := f.Type
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		list := kind == "query" && f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.String
		switch {
		case name != "" && !f.IsExported():
			panic(fmt.Sprintf("livewire: parameter %s is the unexported field %s.%s", name, t, f.Name))
		case name != "" && !list && base.Kind() != reflect.String && base.Kind() != reflect.Int && base.Kind() != reflect.Bool:
			panic(fmt.Sprintf("livewire: parameter %s has the unsupported type %s", name, f.Type))
		case name == "" && f.Anonymous && f.Type.Kind() == reflect.Struct:
			checkParams(f.Type)
		case name == "" && f.Anonymous && base.Kind() == reflect.Struct:
			panic(fmt.Sprintf("livewire: %s embeds %s; embed params structs by value", t, f.Type))
		}
	}
}

// paramTag returns the kind ("path" or "query") and name of a parameter field.
func paramTag(field reflect.StructField) (kind, name string) {
	if name = field.Tag.Get("path"); name != "" {
		return "path", name
	}
	if name = field.Tag.Get("query"); name != "" {
		return "query", name
	}
	return "", ""
}

// jsonFieldNames makes validation errors name fields as the JSON body does.
var jsonFieldNames sync.Once
