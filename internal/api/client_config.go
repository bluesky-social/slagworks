package api

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// ClientConfig is what a CLI needs to sign an operator in to this server.
// Both values are public by nature: the issuer is where the operator's
// browser goes, and the client ID identifies a public OIDC client.
type ClientConfig struct {
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
}

// clientConfig serves GET /api/v1/client-config without authentication:
// slagctl calls it before it has a token, so that an operator configures
// only the API URL and the CLI cannot disagree with the server about the
// identity provider.
func (s *Server) clientConfig(c *echo.Context) error {
	return c.JSON(http.StatusOK, ClientConfig{
		OIDCIssuer:   s.oidcIssuer,
		OIDCClientID: s.oidcAudience,
	})
}
