package livewire

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// DefaultChannel is the NOTIFY channel the triggers send on.
const DefaultChannel = "livewire"

// Change is one row change as the trigger reports it. ID is empty for tables without an
// "id" column (join tables).
type Change struct {
	Table string `json:"table"`
	Op    string `json:"op"`
	ID    string `json:"id"`
}

// InstallTriggers makes every given table NOTIFY channel on each row change. It is
// idempotent, so it runs on every start after the schema migration.
func InstallTriggers(ctx context.Context, db *sql.DB, channel string, tables ...string) error {
	statements := []string{`CREATE OR REPLACE FUNCTION livewire_notify() RETURNS trigger AS $$
DECLARE
	row jsonb := to_jsonb(CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END);
BEGIN
	PERFORM pg_notify(TG_ARGV[0], json_build_object('table', TG_TABLE_NAME, 'op', TG_OP, 'id', COALESCE(row->>'id', ''))::text);
	RETURN NULL;
END;
$$ LANGUAGE plpgsql`}
	for _, table := range tables {
		statements = append(statements, fmt.Sprintf(
			`CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION livewire_notify(%s)`,
			quoteIdent(table), quoteLiteral(channel)))
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("livewire: installing triggers: %w", err)
		}
	}
	return nil
}

// listen holds one dedicated connection on channel and calls onChange for every
// notification. Whenever LISTEN is (re-)established it calls onResync, because
// notifications sent before, at startup or while disconnected, are gone. It returns when
// ctx ends.
func listen(ctx context.Context, dsn, channel string, onChange func(Change), onResync func()) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN "+quoteIdent(channel))
		}
		if err != nil {
			if conn != nil {
				_ = conn.Close(context.Background())
			}
			log.Warn().Err(err).Dur("retry", backoff).Msg("livewire: listen connection failed")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		onResync()
		for {
			notification, err := conn.WaitForNotification(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn().Err(err).Msg("livewire: listen connection lost")
				}
				break
			}
			var change Change
			if err := json.Unmarshal([]byte(notification.Payload), &change); err != nil {
				log.Warn().Str("payload", notification.Payload).Msg("livewire: unreadable notification")
				continue
			}
			onChange(change)
		}
		_ = conn.Close(context.Background())
	}
}

func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func quoteLiteral(value string) string { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }
