package livewire

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// EngineOptions configures the live engine.
type EngineOptions struct {
	Registry *Registry
	// DSN of the dedicated LISTEN connection.
	DSN string
	// Channel the triggers NOTIFY on; DefaultChannel when empty.
	Channel string
	// OnChange sees every row change before subscriptions re-run, e.g. to drop a cache.
	OnChange func(Change)
	// CheckOrigin overrides the WebSocket origin check (same host by default).
	CheckOrigin func(r *http.Request) bool
	// Revalidate refreshes a socket's identity before every subscribe and re-run, so a
	// revoked key or a lost role takes effect on open sockets too. It returns the context
	// the checks and loaders then see; an error closes the socket. Nil keeps the context
	// of the upgrade request for the socket's lifetime.
	Revalidate func(ctx context.Context) (context.Context, error)
	// Partition names a subscription's partition (e.g. its tenant) from its decoded params,
	// once at subscribe time. A change carrying a partition (InstallPartitionedTriggers)
	// skips subscriptions of another partition; an empty partition on either side matches
	// all. Nil leaves every subscription unpartitioned.
	Partition func(params any) string
}

// Engine serves live subscriptions: it re-runs a subscription's loader whenever a table
// the resource reads changes and pushes the result as a diff (lists) or replacement
// (objects).
//
// ponytail: one goroutine and one full re-run per affected subscription per change burst.
// Fine for hundreds of subscriptions; add a bounded query semaphore when that grows.
type Engine struct {
	options  *EngineOptions
	upgrader websocket.Upgrader
	mutex    sync.Mutex
	sockets  map[*socket]struct{}
}

// NewEngine builds an engine; Run starts listening.
func NewEngine(options *EngineOptions) *Engine {
	if options.Channel == "" {
		options.Channel = DefaultChannel
	}
	return &Engine{
		options:  options,
		upgrader: websocket.Upgrader{CheckOrigin: options.CheckOrigin},
		sockets:  map[*socket]struct{}{},
	}
}

// Run listens for row changes until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	listen(ctx, e.options.DSN, e.options.Channel, e.dispatch, e.resync)
}

func (e *Engine) dispatch(change Change) {
	if e.options.OnChange != nil {
		e.options.OnChange(change)
	}
	e.each(func(s *subscription) {
		if change.Partition != "" && s.partition != "" && change.Partition != s.partition {
			return
		}
		if slices.Contains(s.resource.Tables, change.Table) {
			s.markDirty(false)
		}
	})
}

// resync re-sends a snapshot on every subscription after notifications may have been lost.
func (e *Engine) resync() {
	e.each(func(s *subscription) { s.markDirty(true) })
}

func (e *Engine) each(fn func(*subscription)) {
	e.mutex.Lock()
	sockets := make([]*socket, 0, len(e.sockets))
	for s := range e.sockets {
		sockets = append(sockets, s)
	}
	e.mutex.Unlock()
	for _, s := range sockets {
		s.mutex.Lock()
		for _, sub := range s.subscriptions {
			fn(sub)
		}
		s.mutex.Unlock()
	}
}

// CloseWhere closes every socket whose request context matches, e.g. those opened with a
// key that was just revoked.
func (e *Engine) CloseWhere(match func(ctx context.Context) bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	for s := range e.sockets {
		if match(s.ctx) {
			s.close(CloseUnauthorized, "access revoked")
		}
	}
}

// Handler upgrades the request to the live socket. Put it behind the same authentication
// as the REST routes: the request context it sees is the one every Read check gets.
func (e *Engine) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		conn, err := e.upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(c.Request.Context())
		s := &socket{engine: e, conn: conn, ctx: ctx, cancel: cancel, send: make(chan []byte, 64), subscriptions: map[string]*subscription{}}
		e.mutex.Lock()
		e.sockets[s] = struct{}{}
		e.mutex.Unlock()
		go s.writeLoop()
		s.readLoop()
		e.mutex.Lock()
		delete(e.sockets, s)
		e.mutex.Unlock()
		s.close(websocket.CloseNormalClosure, "")
	}
}

const (
	pingInterval = 30 * time.Second
	readTimeout  = 75 * time.Second
	writeTimeout = 10 * time.Second
)

type socket struct {
	engine        *Engine
	conn          *websocket.Conn
	ctx           context.Context
	cancel        context.CancelFunc
	send          chan []byte
	closeOnce     sync.Once
	mutex         sync.Mutex
	subscriptions map[string]*subscription
	lastID        int
}

func (s *socket) readLoop() {
	s.conn.SetReadLimit(64 << 10)
	_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))
	s.conn.SetPongHandler(func(string) error { return s.conn.SetReadDeadline(time.Now().Add(readTimeout)) })
	for {
		var frame ClientFrame
		if err := s.conn.ReadJSON(&frame); err != nil {
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))
		switch frame.Op {
		case OpPing:
			s.enqueue(ResponseFrame{ID: frame.ID, OK: true})
		case OpSubscribe:
			s.subscribe(frame)
		case OpUnsubscribe:
			s.unsubscribe(frame.Subscription)
			s.enqueue(ResponseFrame{ID: frame.ID, OK: true})
		default:
			s.enqueue(ResponseFrame{ID: frame.ID, Error: Errorf(http.StatusBadRequest, CodeInvalidRequest, "Unknown op %q", frame.Op)})
		}
	}
}

func (s *socket) writeLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case message := <-s.send:
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := s.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				s.close(websocket.CloseAbnormalClosure, "")
				return
			}
		case <-ticker.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				s.close(websocket.CloseAbnormalClosure, "")
				return
			}
		}
	}
}

// enqueue queues a frame; a client too slow to drain its queue is disconnected and will
// resubscribe for fresh snapshots.
func (s *socket) enqueue(frame any) {
	message, err := json.Marshal(frame)
	if err != nil {
		log.Error().Err(err).Msg("livewire: marshal frame")
		return
	}
	select {
	case s.send <- message:
	case <-s.ctx.Done():
	default:
		s.close(websocket.CloseTryAgainLater, "too slow")
	}
}

func (s *socket) close(code int, reason string) {
	s.closeOnce.Do(func() {
		_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
		s.cancel()
		_ = s.conn.Close()
	})
}

func (s *socket) subscribe(frame ClientFrame) {
	registry := s.engine.options.Registry
	res := registry.Resource(frame.Target)
	if res == nil || !res.Live() {
		s.enqueue(ResponseFrame{ID: frame.ID, Error: Errorf(http.StatusNotFound, CodeNotFound, "No live target %q", frame.Target)})
		return
	}
	params, err := decodeParams(res.Params, mapSource(frame.Params))
	if err == nil {
		var ctx context.Context
		if ctx, err = s.identity(); err != nil {
			return
		}
		if ctx, err = registry.context(ctx, params); err == nil {
			err = res.Read(ctx)
		}
	}
	if err != nil {
		s.enqueue(ResponseFrame{ID: frame.ID, Error: registry.asError(err)})
		return
	}
	partition := ""
	if s.engine.options.Partition != nil {
		partition = s.engine.options.Partition(params)
	}
	s.mutex.Lock()
	s.lastID++
	ctx, cancel := context.WithCancel(s.ctx)
	sub := &subscription{id: strconv.Itoa(s.lastID), socket: s, resource: res, params: params, partition: partition, ctx: ctx, cancel: cancel, dirty: make(chan struct{}, 1)}
	s.subscriptions[sub.id] = sub
	s.mutex.Unlock()
	s.enqueue(ResponseFrame{ID: frame.ID, OK: true, Subscription: sub.id})
	sub.markDirty(true)
	go sub.run()
}

// identity revalidates the socket's caller; on failure the socket is closed.
func (s *socket) identity() (context.Context, error) {
	revalidate := s.engine.options.Revalidate
	if revalidate == nil {
		return s.ctx, nil
	}
	ctx, err := revalidate(s.ctx)
	if err != nil {
		s.close(CloseUnauthorized, "access revoked")
		return nil, err
	}
	return ctx, nil
}

func (s *socket) unsubscribe(id string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if sub, ok := s.subscriptions[id]; ok {
		sub.cancel()
		delete(s.subscriptions, id)
	}
}

type subscription struct {
	id        string
	socket    *socket
	resource  *Resource
	params    any
	partition string
	ctx       context.Context
	cancel    context.CancelFunc
	// dirty holds at most one pending re-run: changes arriving while a re-run is in flight
	// collapse into one follow-up.
	dirty    chan struct{}
	snapshot atomic.Bool
	failures int
	seq      int64
	list     *listState
	object   json.RawMessage
}

func (s *subscription) markDirty(snapshot bool) {
	if snapshot {
		s.snapshot.Store(true)
	}
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *subscription) run() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.dirty:
		}
		if !s.refresh() {
			s.socket.unsubscribe(s.id)
			return
		}
	}
}

// refresh re-runs the loader and pushes what changed. It returns false when the
// subscription is over.
func (s *subscription) refresh() (alive bool) {
	snapshot := s.snapshot.Swap(false)
	defer func() {
		// A panicking loader must cost one subscription a retry, never the process.
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Str("target", s.resource.Name).Msg("livewire: re-run panicked")
			s.retryLater(snapshot)
			alive = true
		}
	}()
	ctx, err := s.socket.identity()
	if err != nil {
		return false
	}
	registry := s.socket.engine.options.Registry
	data, err := registry.read(ctx, s.resource, s.params)
	if s.ctx.Err() != nil {
		return false
	}
	if err != nil {
		e := registry.asError(err)
		if e.Status >= http.StatusInternalServerError {
			log.Error().Err(err).Str("target", s.resource.Name).Msg("livewire: re-run failed")
			s.retryLater(snapshot)
			return true
		}
		payload, _ := json.Marshal(e)
		s.push(PushError, payload)
		return false
	}
	s.failures = 0
	if !s.resource.List {
		raw, err := json.Marshal(data)
		if err != nil {
			log.Error().Err(err).Str("target", s.resource.Name).Msg("livewire: marshal")
			return true
		}
		if snapshot || !bytes.Equal(raw, s.object) {
			s.object = raw
			s.push(PushReplace, raw)
		}
		return true
	}
	state, err := newListState(data)
	if err != nil {
		log.Error().Err(err).Str("target", s.resource.Name).Msg("livewire: list state")
		return true
	}
	previous := s.list
	s.list = state
	if snapshot || previous == nil {
		s.push(PushSnapshot, state.snapshot())
		return true
	}
	if d := state.diff(previous); d != nil {
		payload, _ := json.Marshal(d)
		s.push(PushDiff, payload)
	}
	return true
}

// retryLater re-runs a failed subscription with backoff (1 s doubling to 30 s), so a
// transient database error does not leave it stale until the next change.
func (s *subscription) retryLater(snapshot bool) {
	if snapshot {
		s.snapshot.Store(true)
	}
	delay := min(time.Second<<min(s.failures, 5), 30*time.Second)
	s.failures++
	time.AfterFunc(delay, func() {
		if s.ctx.Err() == nil {
			s.markDirty(false)
		}
	})
}

func (s *subscription) push(kind string, payload json.RawMessage) {
	s.seq++
	s.socket.enqueue(PushFrame{Sub: s.id, Seq: s.seq, Kind: kind, Payload: payload})
}
