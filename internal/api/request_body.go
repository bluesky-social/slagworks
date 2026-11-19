package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// jsonBodyBinder is the API server's echo Binder. Request bodies are JSON
// (docs/dev/api.md), so a non-empty body with any other Content-Type is
// refused with 415. echo's DefaultBinder on its own also binds form,
// multipart and XML bodies, and a JSON document sent as a form (what
// `curl -d '{...}'` sends without a Content-Type header) binds no field of a
// JSON-tagged struct and reports no error: the handler would act on an empty
// body and silently drop every flag the caller set.
//
// A request without a body (ContentLength 0) still binds nothing and
// succeeds, as with DefaultBinder: deploy, commission and bmc-reconcile take
// an optional body.
type jsonBodyBinder struct {
	echo.DefaultBinder
}

func (b *jsonBodyBinder) Bind(c *echo.Context, target any) error {
	req := c.Request()
	if req.ContentLength != 0 {
		// Parsed the way echo's BindBody parses it, so every body that
		// passes here takes BindBody's JSON branch.
		base, _, _ := strings.Cut(req.Header.Get(echo.HeaderContentType), ";")
		if mt := strings.TrimSpace(base); mt != echo.MIMEApplicationJSON {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, fmt.Sprintf(
				"request body has Content-Type %q; send it as JSON with Content-Type: application/json", mt))
		}
	}
	return b.DefaultBinder.Bind(c, target)
}
