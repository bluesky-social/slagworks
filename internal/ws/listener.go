package ws

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notification represents a parsed Postgres NOTIFY payload.
type Notification struct {
	Table   string
	ID      string
	Deleted bool
}

// ParseNotification parses a NOTIFY payload like "machines:uuid" or
// "ssh_keys:deleted:uuid".
func ParseNotification(payload string) Notification {
	parts := strings.SplitN(payload, ":", 3)
	switch len(parts) {
	case 3:
		return Notification{Table: parts[0], ID: parts[2], Deleted: parts[1] == "deleted"}
	case 2:
		return Notification{Table: parts[0], ID: parts[1]}
	default:
		return Notification{}
	}
}

// NotifyHandler is called for each parsed notification.
type NotifyHandler func(Notification)

// Listener runs LISTEN slagworks on a dedicated Postgres connection
// and dispatches parsed notifications to the handler.
type Listener struct {
	dsn          string
	handler      NotifyHandler
	log          *slog.Logger
	OnConnect    func() // called when LISTEN connection is established
	OnDisconnect func() // called when LISTEN connection is lost
}

func NewListener(dsn string, handler NotifyHandler) *Listener {
	return &Listener{
		dsn:     dsn,
		handler: handler,
		log:     slog.Default(),
	}
}

func (l *Listener) SetLogger(log *slog.Logger) {
	l.log = log
}

// Run connects to Postgres and enters the LISTEN loop. It reconnects
// with exponential backoff on connection errors. It blocks until ctx
// is cancelled.
func (l *Listener) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		connected, err := l.listenLoop(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			backoff = time.Second
		}
		l.log.Warn("pg listener disconnected, reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// listenLoop connects and processes notifications. Returns (true, err) if
// it connected successfully before disconnecting (backoff should reset).
func (l *Listener) listenLoop(ctx context.Context) (connected bool, err error) {
	conn, err := pgx.Connect(ctx, l.dsn)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "LISTEN slagworks"); err != nil {
		return false, err
	}

	l.log.Info("pg listener connected")
	if l.OnConnect != nil {
		l.OnConnect()
	}
	defer func() {
		if l.OnDisconnect != nil {
			l.OnDisconnect()
		}
	}()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		if n.Channel != "slagworks" {
			continue
		}
		parsed := ParseNotification(n.Payload)
		if parsed.Table == "" {
			l.log.Warn("unparseable NOTIFY payload", "payload", n.Payload)
			continue
		}
		l.handler(parsed)
	}
}
