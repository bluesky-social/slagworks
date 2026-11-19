package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// refuseNUL answers 400 to an operator request that carries a NUL byte
// (U+0000) anywhere a handler reads text from: the path, the query string,
// or the JSON body. Postgres cannot store U+0000 in text or jsonb,
// and no field this API accepts has a use for it. Let through, it fails
// the handler's first write or lookup with SQLSTATE 22021, which handlers
// report as a 500 carrying the raw database error.
//
// It is one check at the edge rather than one per field because the byte
// reaches Postgres through path parameters, query parameters and
// map-bound bodies (PATCH /machines/:id) as well as through named fields,
// and a per-field check is one a new handler can forget.
//
// A body is read as JSON whatever its Content-Type and put back, so the
// handler reads exactly what it would have without this check. JSON is the
// only body encoding the API takes: jsonBodyBinder refuses any other
// Content-Type at Bind, and the one handler that decodes its body itself
// (netboxMigrateChassisSerial) decodes it as JSON without looking at
// the header, so the label must not be what decides whether the body is
// examined. A body that is not JSON carries nothing a handler will store,
// and is left for the handler to refuse.
func refuseNUL(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		req := c.Request()
		if strings.ContainsRune(req.URL.Path, 0) {
			return nulRefusal("the request path")
		}
		for k, vs := range c.QueryParams() {
			if containsNUL(k, vs) {
				return nulRefusal(fmt.Sprintf("query parameter %q", k))
			}
		}
		if req.ContentLength == 0 {
			return next(c)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			if echo.StatusCode(err) != 0 {
				return err // BodyLimit's 413
			}
			return echo.ErrBadRequest.Wrap(err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		if where := nulInJSONBody(body); where != "" {
			return nulRefusal(where)
		}
		return next(c)
	}
}

func nulRefusal(where string) error {
	return echo.NewHTTPError(http.StatusBadRequest,
		where+" contains a NUL byte (U+0000), which slagworks cannot store; remove it and resend the request")
}

func containsNUL(key string, values []string) bool {
	if strings.ContainsRune(key, 0) {
		return true
	}
	for _, v := range values {
		if strings.ContainsRune(v, 0) {
			return true
		}
	}
	return false
}

// nulInJSONBody reports where a JSON body carries U+0000 in a string or an
// object key, or returns "". Detection scans every token rather than a
// decoded value, which could hide one: a later duplicate key replaces the
// NUL in a decoded map ({"version":"v\u0000","version":null}) but not in a
// bound struct, where null leaves a string as it was; and a number that
// float64 cannot hold fails a decode into any, while a struct skips the
// unknown field. The top-level field is then named when a decode shows it.
func nulInJSONBody(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // Token decodes each number; as float64, 1e400 fails it
	for {
		tok, err := dec.Token()
		if err != nil {
			return "" // the end of the body, or a syntax error Bind reports
		}
		if s, ok := tok.(string); ok && strings.ContainsRune(s, 0) {
			break
		}
	}
	var obj map[string]any
	dec = json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&obj) == nil {
		for k, v := range obj {
			if strings.ContainsRune(k, 0) || jsonHasNUL(v) {
				return fmt.Sprintf("body field %q", k)
			}
		}
	}
	return "the request body"
}

func jsonHasNUL(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case []any:
		for _, e := range v {
			if jsonHasNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range v {
			if strings.ContainsRune(k, 0) || jsonHasNUL(e) {
				return true
			}
		}
	}
	return false
}
