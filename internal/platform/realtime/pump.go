// Package realtime holds the WebSocket write loop shared by pos-api's hubs (notifications,
// KDS, print agent). The cross-replica registry and relay live in shared-events (FanoutHub);
// this is only the socket side.
package realtime

import (
	"context"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"nhooyr.io/websocket"
)

const (
	// WriteTimeout bounds every frame write, so a stalled client cannot hold the loop.
	WriteTimeout = 5 * time.Second
	// PingInterval keeps idle connections alive through proxies and detects dead peers.
	PingInterval = 25 * time.Second
)

// Pump serves one WebSocket connection from a FanoutHub subscription until the client
// disconnects or ctx ends. It writes hello first (when non-nil), then every message from
// sub.C, sends a protocol ping every PingInterval, and passes each client frame to reply:
// a non-nil return is written back (the hubs answer the client's JSON "ping" with "pong").
func Pump(ctx context.Context, conn *websocket.Conn, sub *eventslib.Sub, hello []byte, reply func(frame []byte) []byte) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	replies := make(chan []byte, 4)
	go func() {
		defer cancel()
		for {
			_, frame, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if reply == nil {
				continue
			}
			if out := reply(frame); out != nil {
				select {
				case replies <- out:
				default: // client is spamming pings; drop
				}
			}
		}
	}()

	write := func(b []byte) error {
		wctx, wcancel := context.WithTimeout(ctx, WriteTimeout)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	if hello != nil {
		if err := write(hello); err != nil {
			return
		}
	}
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.C:
			if !ok {
				return
			}
			if err := write(msg); err != nil {
				return
			}
		case out := <-replies:
			if err := write(out); err != nil {
				return
			}
		case <-ticker.C:
			pctx, pcancel := context.WithTimeout(ctx, WriteTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}
