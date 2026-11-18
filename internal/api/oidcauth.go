package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/beyond/tokenverify"
)

// Operator authentication is OIDC-only: slagctl obtains an Authentik
// access token (loopback+PKCE or device flow) and presents it as a bearer.
// slagworksd validates the token's RS256 signature against Authentik's JWKS,
// checks issuer/audience/expiry, and requires the operator be a member of the
// configured group. There is no static-token path.
//
// The token verification + group authorization live in the shared
// github.com/bluesky-social/beyond/tokenverify package; this file is just the echo middleware glue that wires that
// package's Verifier + Authorizer into the request pipeline.

// ctxKeyOperator is the echo-context key under which the verified operator
// identity is stashed for handlers/audit logging.
const ctxKeyOperator = "operator"

// operatorFromContext returns the verified operator identity stashed by
// oidcAuth, or nil if the request didn't pass through the middleware.
func operatorFromContext(c *echo.Context) *tokenverify.Claims {
	v, _ := c.Get(ctxKeyOperator).(*tokenverify.Claims)
	return v
}

// oidcAuth is the operator-auth middleware. It extracts the bearer token,
// verifies it via the configured tokenverify.Verifier, runs the configured
// tokenverify.Authorizer over the claims, and stashes the identity in the
// echo context.
//
// Status codes:
//   - 401 missing/malformed bearer, or invalid/expired token.
//   - 403 valid token but not authorized (wrong group).
func (s *Server) oidcAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		auth := c.Request().Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			return echo.NewHTTPError(http.StatusUnauthorized, "missing bearer token")
		}
		raw := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if raw == "" {
			return echo.NewHTTPError(http.StatusUnauthorized, "empty bearer token")
		}

		claims, err := s.verifier.Verify(c.Request().Context(), raw)
		if err != nil {
			// Log the rejection with a best-effort UNVERIFIED decode of the
			// token's iss/aud/exp. This is the single most useful signal for
			// diagnosing auth failures: an issuer-skew rejection (e.g. a token
			// minted under a different Authentik host than this server's
			// SLAGWORKS_OIDC_ISSUER) is invisible from the bare 401 otherwise,
			// and the verifier's error text does not name the offending iss.
			// The decode is for logging ONLY — the request is already rejected;
			// we never trust these fields for a decision.
			iss, aud, exp := unverifiedTokenFields(raw)
			s.log.Warn("operator auth: token verification failed",
				"err", err,
				"token_iss", iss,
				"token_aud", aud,
				"token_exp", exp,
				"expected_iss", s.oidcIssuer,
				"expected_aud", s.oidcAudience,
				"remote_ip", c.RealIP(),
				"uri", c.Request().RequestURI,
			)
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
		}
		if err := s.authz(claims); err != nil {
			s.log.Warn("operator auth: not authorized",
				"err", err,
				"sub", claims.Subject,
				"preferred_username", claims.PreferredUsername,
				"groups", claims.Groups,
				"remote_ip", c.RealIP(),
				"uri", c.Request().RequestURI,
			)
			return echo.NewHTTPError(http.StatusForbidden, "not authorized: "+err.Error())
		}
		c.Set(ctxKeyOperator, claims)
		return next(c)
	}
}

// credentialForbiddenMsg is the 403 body for an operator whose token carries
// no credential group. It names the setting but echoes neither the caller's
// groups nor the configured ones; credentialAuth logs both.
const credentialForbiddenMsg = "not authorized to read device credentials: this server restricts them to the groups in " +
	"SLAGWORKS_OIDC_CREDENTIAL_GROUPS (--oidc-credential-groups) and your token carries none of them; " +
	"ask a slagworks administrator to add you to one, then run `slagctl auth login` for a token that carries it"

// credentialAuth gates the routes that return a stored device credential in
// plaintext. It runs after oidcAuth, so the caller has already passed the
// operator group check; when credential groups are configured (credAuthz
// non-nil) the caller must also carry one of them. With none configured
// every operator passes.
//
// Status codes:
//   - 403 operator token without a credential group.
//   - 500 no verified operator in the context: the route was registered
//     without oidcAuth in front, so fail closed.
func (s *Server) credentialAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		claims := operatorFromContext(c)
		if claims == nil {
			s.log.Error("credential gate reached without a verified operator; the route is registered without oidcAuth",
				"uri", c.Request().RequestURI)
			return echo.NewHTTPError(http.StatusInternalServerError,
				"credential route is registered without operator authentication; report this server bug")
		}
		if s.credAuthz == nil {
			return next(c)
		}
		if err := s.credAuthz(claims); err != nil {
			s.log.Info("credential read refused: token carries no credential group",
				"err", err,
				"sub", claims.Subject,
				"preferred_username", claims.PreferredUsername,
				"groups", claims.Groups,
				"remote_ip", c.RealIP(),
				"uri", c.Request().RequestURI,
			)
			return echo.NewHTTPError(http.StatusForbidden, credentialForbiddenMsg)
		}
		return next(c)
	}
}

// unverifiedTokenFields extracts iss/aud/exp from a JWT's payload WITHOUT
// verifying the signature. It exists solely to enrich the rejection log line
// on the 401 path — the token has already failed verification, so these values
// are untrusted and used only for operator diagnostics (issuer skew, wrong
// audience, expiry). Any parse failure yields empty strings rather than an
// error: a malformed/opaque bearer just logs blank fields.
func unverifiedTokenFields(raw string) (iss, aud, exp string) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ""
	}
	// aud may be a string or a []string per the JWT spec; exp is a number.
	// Decode loosely and stringify so a weird shape can't panic the log path.
	var claims struct {
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
		Exp json.Number     `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", ""
	}
	return claims.Iss, strings.Trim(string(claims.Aud), `"`), claims.Exp.String()
}
