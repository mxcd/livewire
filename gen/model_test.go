package gen

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"
)

type model struct {
	ID     string `json:"id"`
	RoomId string `json:"roomId"`
	Note   string
}

// view re-declares roomId one level up: the shallower field wins.
type view struct {
	model
	RoomID string `json:"roomId"`
	Extra  string `json:"extra"`
}

type Left struct {
	Name string `json:"name"`
	Tag  string
}

type Right struct {
	Name string `json:"name"`
	Tag  string `json:"Tag"`
}

// ambiguous embeds Left and Right: name is tagged twice on one level (dropped), Tag once
// (kept). Built at run time because go vet rejects the repeated tag in a declaration.
var ambiguous = reflect.StructOf([]reflect.StructField{
	{Name: "Left", Type: reflect.TypeFor[Left](), Anonymous: true},
	{Name: "Right", Type: reflect.TypeFor[Right](), Anonymous: true},
})

type wrapA struct{ model }
type wrapB struct{ model }

// twice reaches model's fields twice on one level: they cancel out.
type twice struct {
	wrapA
	wrapB
	Own string `json:"own"`
}

type omissions struct {
	At      time.Time `json:"at,omitempty"`
	UUID    [16]byte  `json:"uuid,omitempty"`
	Empty   [0]int    `json:"empty,omitempty"`
	Text    string    `json:"text,omitempty"`
	Zero    time.Time `json:"zero,omitzero"`
	Always  string    `json:"always"`
	Pointer *model    `json:"pointer,omitempty"`
}

// TestJSONFieldsMatchEncodingJSON checks jsonFields against encoding/json itself: the zero
// value of each type marshals exactly the fields that are not optional.
func TestJSONFieldsMatchEncodingJSON(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[view](), ambiguous, reflect.TypeFor[twice](), reflect.TypeFor[omissions]()} {
		raw, err := json.Marshal(reflect.New(typ).Elem().Interface())
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		_ = json.Unmarshal(raw, &object)
		var want, got []string
		for key := range object {
			want = append(want, key)
		}
		for _, f := range jsonFields(typ) {
			if !f.Optional {
				got = append(got, f.Name)
			}
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: required fields %v, encoding/json writes %v", typ, got, want)
		}
	}
	var names []string
	for _, f := range jsonFields(reflect.TypeFor[view]()) {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"id", "Note", "roomId", "extra"}) {
		t.Errorf("view fields in order: %v", names)
	}
	if fields := jsonFields(ambiguous); len(fields) != 1 || fields[0].Name != "Tag" {
		t.Errorf("ambiguous: %+v", fields)
	}
}

func TestPointerElements(t *testing.T) {
	ts := &types{byName: map[string]reflect.Type{}, enums: map[reflect.Type][]string{}}
	if got := ts.tsType(reflect.TypeFor[[]*model](), ""); got != "model[]" {
		t.Errorf("slice of pointers: %s", got)
	}
	if got := ts.tsType(reflect.TypeFor[map[string]*string](), ""); got != "Record<string, string>" {
		t.Errorf("map of pointers: %s", got)
	}
	if got := ts.tsType(reflect.TypeFor[*[]string](), ""); got != "string[] | null" {
		t.Errorf("pointer to a slice: %s", got)
	}
	if got := ts.schema(reflect.TypeFor[[]*model]())["items"]; !reflect.DeepEqual(got, object{"$ref": "#/components/schemas/model"}) {
		t.Errorf("OpenAPI items: %v", got)
	}
}
