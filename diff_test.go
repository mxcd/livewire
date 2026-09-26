package livewire

import (
	"encoding/json"
	"slices"
	"testing"
)

type item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func state(t *testing.T, items ...item) *listState {
	t.Helper()
	s, err := newListState(items)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDiff(t *testing.T) {
	a, b, c := item{"a", "Milch"}, item{"b", "Brot"}, item{"c", "Eier"}

	if d := state(t, a, b).diff(state(t, a, b)); d != nil {
		t.Fatalf("unchanged list produced a diff: %+v", d)
	}

	// An edit in place: one upsert, same order.
	d := state(t, a, item{"b", "Vollkornbrot"}).diff(state(t, a, b))
	if len(d.Upserts) != 1 || len(d.Removes) != 0 || d.Order != nil {
		t.Fatalf("edit: %+v", d)
	}

	// An insert and a removal change membership, so the order comes along.
	d = state(t, c, a).diff(state(t, a, b))
	var upserted item
	_ = json.Unmarshal(d.Upserts[0], &upserted)
	if len(d.Upserts) != 1 || upserted != c || !slices.Equal(d.Removes, []string{"b"}) || !slices.Equal(d.Order, []string{"c", "a"}) {
		t.Fatalf("insert+remove: %+v", d)
	}

	// A pure reorder: no upserts, only the order.
	d = state(t, b, a).diff(state(t, a, b))
	if len(d.Upserts) != 0 || len(d.Removes) != 0 || !slices.Equal(d.Order, []string{"b", "a"}) {
		t.Fatalf("reorder: %+v", d)
	}

	if _, err := newListState([]struct{ Name string }{{"x"}}); err == nil {
		t.Fatal("an item without an id was accepted")
	}
}
