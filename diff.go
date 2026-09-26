package livewire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// listState is what a list subscription last pushed: each item's JSON by id, in order.
type listState struct {
	order []string
	items map[string]json.RawMessage
}

// newListState marshals a loaded list item by item.
func newListState(list any) (*listState, error) {
	raw, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	state := &listState{order: make([]string, 0, len(items)), items: make(map[string]json.RawMessage, len(items))}
	for _, item := range items {
		var keyed struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(item, &keyed); err != nil || len(keyed.ID) == 0 {
			return nil, fmt.Errorf("livewire: list item without an id: %s", item)
		}
		id := string(bytes.Trim(keyed.ID, `"`))
		state.order = append(state.order, id)
		state.items[id] = item
	}
	return state, nil
}

// snapshot is the list's JSON array.
func (s *listState) snapshot() json.RawMessage {
	items := make([]json.RawMessage, len(s.order))
	for i, id := range s.order {
		items[i] = s.items[id]
	}
	raw, _ := json.Marshal(items)
	return raw
}

// diff returns what changed from prev to s, or nil when nothing did.
func (s *listState) diff(prev *listState) *Diff {
	d := &Diff{Upserts: []json.RawMessage{}, Removes: []string{}}
	for _, id := range s.order {
		if old, ok := prev.items[id]; !ok || !bytes.Equal(old, s.items[id]) {
			d.Upserts = append(d.Upserts, s.items[id])
		}
	}
	for _, id := range prev.order {
		if _, ok := s.items[id]; !ok {
			d.Removes = append(d.Removes, id)
		}
	}
	if !slices.Equal(prev.order, s.order) {
		d.Order = s.order
	}
	if len(d.Upserts) == 0 && len(d.Removes) == 0 && d.Order == nil {
		return nil
	}
	return d
}
