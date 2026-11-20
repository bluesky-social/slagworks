package cmd

import (
	"context"
	"net/http"

	"github.com/coder/websocket"

	"github.com/bluesky-social/slagworks/internal/ws"
)

// streamDialOptions carries the operator token on a WebSocket upgrade in
// the Authorization header, the same way the REST client sends it. The
// token never goes in the URL: URLs end up in access logs.
func streamDialOptions(token string) *websocket.DialOptions {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return &websocket.DialOptions{HTTPHeader: h}
}

// dialStream opens one of global's operator streams (/api/v1/logs/stream,
// /api/v1/console/stream) with the token apiClient would send now, fetched
// for this dial: global checks it only at the upgrade, so a stream opened
// with a token that later expires keeps running, but a reconnect after that
// must present a refreshed one. An error getting the token is returned
// unchanged.
//
// When global refuses the upgrade (an expired token, a group the operator
// lacks, a parameter it cannot parse), the error carries the reason global
// gave, not only the status (ws.DialError).
//
// Global sends one message per stored row, and the row's size is set
// upstream, not by coder/websocket's 32 KiB default read limit: a log
// envelope arrives in a site-worker batch of up to ws.MaxMessageBytes/4,
// and a cast event of control bytes encodes to several times its raw size.
// A read over the limit closes the connection, so one large row would end
// `machine logs` and stall every reconnecting watch at that row. The limit
// is therefore ws.MaxMessageBytes, the largest message the site-worker link
// carries.
func dialStream(ctx context.Context, streamURL string) (*websocket.Conn, error) {
	token := ""
	if apiClient != nil {
		var err error
		if token, err = apiClient.BearerToken(ctx); err != nil {
			return nil, err
		}
	}
	conn, resp, err := websocket.Dial(ctx, streamURL, streamDialOptions(token))
	if err != nil {
		err = ws.DialError(err, resp)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(ws.MaxMessageBytes)
	return conn, nil
}
