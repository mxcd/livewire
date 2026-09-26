package livewire

import "encoding/json"

// The frames of the live endpoint. Every frame is one JSON text message; a payload carries
// the same JSON as the REST body of the same read.

// Ops a client sends.
const (
	OpSubscribe   = "subscribe"
	OpUnsubscribe = "unsubscribe"
	OpPing        = "ping"
)

// Kinds of a push.
const (
	// PushSnapshot is a list's full body: seq 1, and again after a resync.
	PushSnapshot = "snapshot"
	// PushDiff is a list change as a Diff.
	PushDiff = "diff"
	// PushReplace is the full body of an object resource.
	PushReplace = "replace"
	// PushError ends the subscription; the payload is an Error.
	PushError = "error"
)

// ClientFrame is a request. Params are the resource's parameters by tag name.
type ClientFrame struct {
	ID           string            `json:"id"`
	Op           string            `json:"op" enum:"subscribe,unsubscribe,ping"`
	Target       string            `json:"target,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	Subscription string            `json:"subscription,omitempty"`
}

// ResponseFrame answers one request by its id.
type ResponseFrame struct {
	ID           string `json:"id"`
	OK           bool   `json:"ok"`
	Subscription string `json:"subscription,omitempty"`
	Error        *Error `json:"error,omitempty"`
}

// PushFrame is a subscription's change; Seq counts from 1 per subscription.
type PushFrame struct {
	Sub     string          `json:"sub"`
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind" enum:"snapshot,diff,replace,error"`
	Payload json.RawMessage `json:"payload"`
}

// Diff is a list change: replace upserted items by id, drop removed ids, and when Order is
// present re-sort to it (the full id list, sent when membership or order changed).
type Diff struct {
	Upserts []json.RawMessage `json:"upserts"`
	Removes []string          `json:"removes"`
	Order   []string          `json:"order,omitempty"`
}

// CloseUnauthorized is the close code when the socket's identity lost access.
const CloseUnauthorized = 4401
