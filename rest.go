package livewire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
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
		group.GET(res.Path, func(c *gin.Context) {
			params, err := decodeParams(res.Params, ginSource(c))
			if err == nil {
				var payload any
				if payload, err = res.Get(c.Request.Context(), params); err == nil {
					c.JSON(http.StatusOK, payload)
					return
				}
			}
			writeError(c, err)
		})
	}
	for _, m := range r.Mutations {
		m := m
		group.Handle(m.Method, m.Path, func(c *gin.Context) {
			if err := m.serve(c); err != nil {
				writeError(c, err)
			}
		})
	}
}

// Get authorizes and loads a resource.
func (r *Resource) Get(ctx context.Context, params any) (any, error) {
	if err := r.Read(ctx); err != nil {
		return nil, err
	}
	return r.load(ctx, params)
}

func (m *Mutation) serve(c *gin.Context) error {
	params, err := decodeParams(m.Params, ginSource(c))
	if err != nil {
		return err
	}
	body := reflect.New(m.Request).Interface()
	if m.HasBody() {
		if err := c.ShouldBindWith(body, binding.JSON); err != nil {
			return bindError(err)
		}
	}
	ctx := c.Request.Context()
	if err := m.Check(ctx); err != nil {
		return err
	}
	response, err := m.handle(ctx, params, body)
	if err != nil {
		return err
	}
	if m.Status() == http.StatusNoContent {
		c.Status(http.StatusNoContent)
		return nil
	}
	c.JSON(m.Status(), response)
	return nil
}

func writeError(c *gin.Context, err error) {
	e := asError(err)
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

// paramSource looks a parameter up by its tag kind ("path" or "query") and name.
type paramSource func(kind, name string) (string, bool)

func ginSource(c *gin.Context) paramSource {
	return func(kind, name string) (string, bool) {
		if kind == "path" {
			v := c.Param(name)
			return v, v != ""
		}
		return c.GetQuery(name)
	}
}

// decodeParams fills a new value of t from fields tagged `path:"name"` or `query:"name"`.
// Supported kinds are string, int, bool and pointers to them; an absent parameter stays
// zero or nil.
func decodeParams(t reflect.Type, source paramSource) (any, error) {
	out := reflect.New(t)
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		kind, name := paramTag(field)
		if name == "" {
			continue
		}
		raw, ok := source(kind, name)
		if !ok || raw == "" {
			continue
		}
		target := out.Elem().Field(i)
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
				return nil, &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "A parameter is not valid", Fields: map[string]string{name: "int"}}
			}
			target.SetInt(int64(n))
		case reflect.Bool:
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "A parameter is not valid", Fields: map[string]string{name: "bool"}}
			}
			target.SetBool(b)
		default:
			panic(fmt.Sprintf("livewire: parameter %s has the unsupported type %s", name, target.Type()))
		}
	}
	return out.Interface(), nil
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
