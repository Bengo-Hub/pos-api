// Package realtime adapts this service's WebSocket library (nhooyr.io/websocket) to the
// fleet-wide write loop, shared-events Pump. The cross-replica registry and relay are the
// shared FanoutHub; only this adapter is service-specific.
package realtime

import (
	"context"

	eventslib "github.com/Bengo-Hub/shared-events"
	"nhooyr.io/websocket"
)

// Socket adapts conn for eventslib.Pump.
func Socket(conn *websocket.Conn) eventslib.Socket {
	return eventslib.Socket{
		Read:  func(ctx context.Context) ([]byte, error) { _, b, err := conn.Read(ctx); return b, err },
		Write: func(ctx context.Context, b []byte) error { return conn.Write(ctx, websocket.MessageText, b) },
		Ping:  conn.Ping,
	}
}
