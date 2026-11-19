package cmd

import (
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
