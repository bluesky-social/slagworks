package cmd

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
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
func dialStream(ctx context.Context, url string) (*websocket.Conn, error) {
	token := ""
	if apiClient != nil {
		var err error
		if token, err = apiClient.BearerToken(ctx); err != nil {
			return nil, err
		}
	}
	conn, resp, err := websocket.Dial(ctx, url, streamDialOptions(token))
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return conn, err
}
