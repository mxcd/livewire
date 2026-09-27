package livewire

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestDispatchPartition(t *testing.T) {
	e := NewEngine(&EngineOptions{Registry: &Registry{}})
	s := &socket{subscriptions: map[string]*subscription{}}
	e.sockets[s] = struct{}{}
	subs := map[string]*subscription{}
	for _, partition := range []string{"a", "b", ""} {
		sub := &subscription{resource: &Resource{Tables: []string{"bookings"}}, partition: partition, dirty: make(chan struct{}, 1)}
		s.subscriptions[partition] = sub
		subs[partition] = sub
	}
	dirty := func() (out []string) {
		for _, p := range []string{"a", "b", ""} {
			select {
			case <-subs[p].dirty:
				out = append(out, p)
			default:
			}
		}
		return out
	}

	e.dispatch(Change{Table: "bookings", Partition: "a"})
	if got := dirty(); fmt.Sprint(got) != "[a ]" {
		t.Fatalf("partition a re-ran %q", got)
	}
	e.dispatch(Change{Table: "bookings"})
	if got := dirty(); fmt.Sprint(got) != "[a b ]" {
		t.Fatalf("no partition re-ran %q", got)
	}
	e.dispatch(Change{Table: "rooms", Partition: "a"})
	if got := dirty(); len(got) != 0 {
		t.Fatalf("another table re-ran %q", got)
	}
}

// TestTriggers installs both trigger kinds in a scratch schema of the database named by
// LIVEWIRE_TEST_DATABASE_URL and checks the notifications they send.
func TestTriggers(t *testing.T) {
	dsn := os.Getenv("LIVEWIRE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("LIVEWIRE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("livewire_test_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*config)
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	defer db.Close()
	exec := func(db *sql.DB, statement string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	exec(db, "CREATE TABLE bookings (id text PRIMARY KEY, tenant_id text)")
	exec(db, "CREATE TABLE notes (id text PRIMARY KEY)")
	if err := InstallPartitionedTriggers(ctx, db, schema, "tenant_id", "bookings"); err != nil {
		t.Fatal(err)
	}
	if err := InstallTriggers(ctx, db, schema, "notes"); err != nil {
		t.Fatal(err)
	}

	listenCtx, stop := context.WithCancel(ctx)
	ready, done := make(chan struct{}), make(chan struct{})
	changes := make(chan Change, 4)
	var once sync.Once
	go func() {
		defer close(done)
		listen(listenCtx, dsn, schema, func(c Change) { changes <- c }, func() { once.Do(func() { close(ready) }) })
	}()
	defer func() { stop(); <-done }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("LISTEN never came up")
	}

	exec(db, "INSERT INTO bookings VALUES ('b1', 'acme')")
	exec(db, "INSERT INTO bookings VALUES ('b2', NULL)")
	exec(db, "INSERT INTO notes VALUES ('n1')")
	for _, want := range []Change{
		{Table: "bookings", Op: "INSERT", ID: "b1", Partition: "acme"},
		{Table: "bookings", Op: "INSERT", ID: "b2"},
		{Table: "notes", Op: "INSERT", ID: "n1"},
	} {
		select {
		case got := <-changes:
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("no notification for %+v", want)
		}
	}
}
