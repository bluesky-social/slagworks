// Package authfake is test-only machinery for slagctl's authentication
// lifecycle: an in-process OIDC provider that rotates refresh tokens, and a
// fake slagworks API that enforces access-token expiry exactly, both driven
// by one virtual clock so a test can cross many token lifetimes in
// milliseconds. Nothing outside *_test.go files imports it.
//
// Time model: the provider and the API read Clock.Now(), which is the real
// time plus a virtual offset that Advance moves forward. The client side
// (golang.org/x/oauth2, and clientauth reading a JWT's exp) reads the real
// time, so a test that advances the clock must also move every expiry the
// client holds back by the same amount: the cached token's Expiry, and the
// exp claim inside the cached access token, which Registry.Shift rewrites.
//
// Access tokens are unsigned JWT-shaped strings so slagctl's whoami decoding
// and clientauth's exp fallback work on them; the API trusts the shared
// Registry, not the signature. A token's exp claim is its expiry as the
// client's clock sees it; the Registry holds the expiry the API enforces.
package authfake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Clock is the virtual clock shared by providers and APIs.
type Clock struct {
	mu  sync.Mutex
	adv time.Duration
}

// Now is the real time plus the virtual offset.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.adv)
}

// Advance moves virtual time forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.adv += d
	c.mu.Unlock()
}

// AccessInfo is what the registry knows about an issued access token.
type AccessInfo struct {
	Issuer string
	Family int
	Gen    int
	Issued time.Time // virtual time
	Exp    time.Time // virtual time
}

// Registry maps every access token any provider issued to its facts, and
// hands out family numbers that are unique across providers.
type Registry struct {
	mu         sync.Mutex
	tokens     map[string]AccessInfo
	families   int
	famIssuer  map[int]string
	rtIssuance map[string]int // refresh token -> family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tokens: map[string]AccessInfo{}, famIssuer: map[int]string{}, rtIssuance: map[string]int{}}
}

// Lookup returns the facts for an access token.
func (r *Registry) Lookup(tok string) (AccessInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, ok := r.tokens[tok]
	return i, ok
}

// FamilyIssuer names the provider that minted a family.
func (r *Registry) FamilyIssuer(fam int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.famIssuer[fam]
}

// FamilyOfRefresh returns the family a refresh token belongs to (0 if unknown).
func (r *Registry) FamilyOfRefresh(rt string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rtIssuance[rt]
}

// Shift is the client half of advancing the clock by d for one access token:
// it returns the same token with the exp claim moved back by d, which the
// APIs accept exactly as they accept tok. A string that is not one of the
// registry's tokens is returned unchanged.
func (r *Registry) Shift(tok string, d time.Duration) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.tokens[tok]
	parts := strings.Split(tok, ".")
	if !ok || len(parts) != 3 {
		return tok
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tok
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return tok
	}
	exp, _ := claims["exp"].(float64)
	claims["exp"] = int64(exp) - int64(d/time.Second)
	payload, _ = json.Marshal(claims)
	shifted := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
	r.tokens[shifted] = info
	return shifted
}

// Provider is a fake OIDC provider with refresh-token rotation. It serves
// discovery, the authorization-code grant (its authorize endpoint redirects
// straight back with a code, as a browser that completes the login would),
// the device grant (approved at once) and the refresh grant.
type Provider struct {
	Clock    *Clock
	Reg      *Registry
	Lifetime time.Duration
	srv      *httptest.Server

	mu            sync.Mutex
	consumed      map[string]bool
	revoked       map[int]bool
	current       map[int]int // family -> latest generation
	reuses        []string
	foreign       []string // refresh tokens minted by another provider
	refreshes     int
	down          bool
	delay         time.Duration
	omitExpiresIn bool
	codes         map[string]bool // authorization codes handed out
}

// NewProvider starts a provider on a loopback port.
func NewProvider(clock *Clock, reg *Registry, lifetime time.Duration) *Provider {
	p := &Provider{Clock: clock, Reg: reg, Lifetime: lifetime,
		consumed: map[string]bool{}, revoked: map[int]bool{}, current: map[int]int{}, codes: map[string]bool{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	return p
}

// Close stops the provider.
func (p *Provider) Close() { p.srv.Close() }

// Issuer is the provider's issuer URL (discovery lives under it).
func (p *Provider) Issuer() string { return p.srv.URL }

// SetDown makes the token endpoint answer 503 (and discovery 503) while set.
func (p *Provider) SetDown(down bool) { p.mu.Lock(); p.down = down; p.mu.Unlock() }

// SetDelay makes the token endpoint answer slowly.
func (p *Provider) SetDelay(d time.Duration) { p.mu.Lock(); p.delay = d; p.mu.Unlock() }

// SetOmitExpiresIn drops expires_in from token responses (RFC 6749 makes it
// RECOMMENDED, not required).
func (p *Provider) SetOmitExpiresIn(v bool) { p.mu.Lock(); p.omitExpiresIn = v; p.mu.Unlock() }

// Revoke ends a family: its refresh tokens are refused (not a reuse).
func (p *Provider) Revoke(fam int) { p.mu.Lock(); p.revoked[fam] = true; p.mu.Unlock() }

// Reuses lists refresh tokens presented after they were spent.
func (p *Provider) Reuses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reuses...)
}

// Foreign lists refresh tokens presented here that another provider minted.
func (p *Provider) Foreign() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.foreign...)
}

// Refreshes counts successful rotations.
func (p *Provider) Refreshes() int { p.mu.Lock(); defer p.mu.Unlock(); return p.refreshes }

// Mint issues a fresh family (a login) and returns the token as the client
// would hold it: Expiry relative to the client's real clock.
func (p *Provider) Mint() *oauth2.Token {
	p.Reg.mu.Lock()
	p.Reg.families++
	fam := p.Reg.families
	p.Reg.famIssuer[fam] = p.Issuer()
	p.Reg.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	tok, _ := p.mintLocked(fam, 0)
	return tok
}

func (p *Provider) mintLocked(fam, gen int) (*oauth2.Token, int64) {
	p.current[fam] = gen
	now := p.Clock.Now()
	exp := now.Add(p.Lifetime)
	claims, _ := json.Marshal(map[string]any{
		"iss": p.Issuer(), "sub": "operator-sub", "preferred_username": "operator",
		"exp": time.Now().Add(p.Lifetime).Unix(), "fam": fam, "gen": gen,
	})
	at := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".x"
	rt := fmt.Sprintf("rt-%d-%d", fam, gen)
	p.Reg.mu.Lock()
	p.Reg.tokens[at] = AccessInfo{Issuer: p.Issuer(), Family: fam, Gen: gen, Issued: now, Exp: exp}
	p.Reg.rtIssuance[rt] = fam
	p.Reg.mu.Unlock()
	return &oauth2.Token{AccessToken: at, RefreshToken: rt, TokenType: "Bearer",
		Expiry: time.Now().Add(p.Lifetime)}, int64(p.Lifetime / time.Second)
}

func (p *Provider) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	down, delay := p.down, p.delay
	p.mu.Unlock()
	if down {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.Issuer(),
			"authorization_endpoint":                p.Issuer() + "/authorize",
			"token_endpoint":                        p.Issuer() + "/token",
			"device_authorization_endpoint":         p.Issuer() + "/device",
			"jwks_uri":                              p.Issuer() + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	case "/device":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-" + uuid.NewString(), "user_code": "ABCD-EFGH",
			"verification_uri": p.Issuer() + "/device-page", "expires_in": 3, "interval": 1,
		})
	case "/device-page":
		_, _ = w.Write([]byte("enter your code"))
	case "/authorize":
		// A browser that completes the login: redirect to the client's
		// loopback callback with a code.
		code := "code-" + uuid.NewString()
		p.mu.Lock()
		p.codes[code] = true
		p.mu.Unlock()
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound) //nolint:gosec // test fake: the loopback callback is the redirect target by design
	case "/token":
		if delay > 0 {
			time.Sleep(delay)
		}
		p.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func oauthErr(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	defer p.mu.Unlock()
	var tok *oauth2.Token
	var expiresIn int64
	switch r.Form.Get("grant_type") {
	case "refresh_token":
		rt := r.Form.Get("refresh_token")
		fam := p.Reg.FamilyOfRefresh(rt)
		if fam == 0 {
			oauthErr(w, "invalid_grant")
			return
		}
		if p.Reg.FamilyIssuer(fam) != p.Issuer() {
			p.foreign = append(p.foreign, rt)
			oauthErr(w, "invalid_grant")
			return
		}
		if p.revoked[fam] {
			oauthErr(w, "invalid_grant")
			return
		}
		if p.consumed[rt] {
			// Reuse detection: a real provider revokes the whole family.
			p.reuses = append(p.reuses, rt)
			p.revoked[fam] = true
			oauthErr(w, "invalid_grant")
			return
		}
		p.consumed[rt] = true
		p.refreshes++
		tok, expiresIn = p.mintLocked(fam, p.current[fam]+1)
	case "urn:ietf:params:oauth:grant-type:device_code":
		tok, expiresIn = p.newFamilyLocked()
	case "authorization_code":
		if !p.codes[r.Form.Get("code")] {
			oauthErr(w, "invalid_grant")
			return
		}
		delete(p.codes, r.Form.Get("code"))
		tok, expiresIn = p.newFamilyLocked()
	default:
		oauthErr(w, "unsupported_grant_type")
		return
	}
	body := map[string]any{"access_token": tok.AccessToken, "refresh_token": tok.RefreshToken, "token_type": "Bearer"}
	if !p.omitExpiresIn {
		body["expires_in"] = expiresIn
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (p *Provider) newFamilyLocked() (*oauth2.Token, int64) {
	p.Reg.mu.Lock()
	p.Reg.families++
	fam := p.Reg.families
	p.Reg.famIssuer[fam] = p.Issuer()
	p.Reg.mu.Unlock()
	return p.mintLocked(fam, 0)
}

// API is a fake slagworks global: client-config, a few read routes, an
// operation the test completes, and the logs stream. Every route but
// client-config enforces the bearer exactly: an unknown token, a token from
// another issuer (recorded as a leak), one issued before the last RotateKeys,
// or Clock.Now()+Skew past its exp answers 401 with the server's "invalid
// token" body.
type API struct {
	Issuer   string
	ClientID string
	Clock    *Clock
	Reg      *Registry
	srv      *httptest.Server

	mu       sync.Mutex
	skew     time.Duration
	keysFrom time.Time // tokens issued before this are refused
	leaks    []string
	ops      map[string]model.OperationStatus
	machine  uuid.UUID
	conns    map[*websocket.Conn]context.CancelFunc
	rejected int
}

// NewAPI starts an API whose operators sign in at issuer.
func NewAPI(clock *Clock, reg *Registry, issuer string) *API {
	a := &API{Issuer: issuer, ClientID: "slagctl-test", Clock: clock, Reg: reg,
		ops: map[string]model.OperationStatus{}, machine: uuid.New(), conns: map[*websocket.Conn]context.CancelFunc{}}
	a.srv = httptest.NewServer(http.HandlerFunc(a.handle))
	return a
}

// URL is the API base URL.
func (a *API) URL() string { return a.srv.URL }

// Close stops the API.
func (a *API) Close() { a.DropStreams(); a.srv.Close() }

// SetSkew sets how far the server's clock runs ahead of the client's.
func (a *API) SetSkew(d time.Duration) { a.mu.Lock(); a.skew = d; a.mu.Unlock() }

// RotateKeys makes the API refuse every access token issued so far, whatever
// its expiry, as it does once the identity provider has retired the key that
// signed them. Tokens issued afterwards are accepted.
func (a *API) RotateKeys() { a.mu.Lock(); a.keysFrom = a.Clock.Now(); a.mu.Unlock() }

// MachineID is the one machine the API serves.
func (a *API) MachineID() string { return a.machine.String() }

// OpenStreams counts logs streams that are subscribed and open now.
func (a *API) OpenStreams() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.conns) }

// Leaks lists access tokens this API received that another issuer minted.
func (a *API) Leaks() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.leaks...)
}

// Rejected counts 401 answers.
func (a *API) Rejected() int { a.mu.Lock(); defer a.mu.Unlock(); return a.rejected }

// AddOp registers a running operation and returns its id.
func (a *API) AddOp() string {
	id := uuid.NewString()
	a.mu.Lock()
	a.ops[id] = model.OpRunning
	a.mu.Unlock()
	return id
}

// CompleteOp marks an operation completed.
func (a *API) CompleteOp(id string) { a.mu.Lock(); a.ops[id] = model.OpCompleted; a.mu.Unlock() }

// DropStreams closes every open WebSocket (a global restart, a proxy idle
// timeout, a laptop that slept).
func (a *API) DropStreams() {
	a.mu.Lock()
	conns := a.conns
	a.conns = map[*websocket.Conn]context.CancelFunc{}
	a.mu.Unlock()
	for c, cancel := range conns {
		cancel()
		_ = c.CloseNow()
	}
}

// Check reports whether tok is a bearer the API accepts right now, and why not.
func (a *API) Check(tok string) (bool, string) {
	info, ok := a.Reg.Lookup(tok)
	if !ok {
		return false, "unknown token"
	}
	if info.Issuer != a.Issuer {
		a.mu.Lock()
		a.leaks = append(a.leaks, tok)
		a.mu.Unlock()
		return false, "token from another issuer"
	}
	a.mu.Lock()
	skew, keysFrom := a.skew, a.keysFrom
	a.mu.Unlock()
	if info.Issued.Before(keysFrom) {
		return false, "signed with a retired key"
	}
	if !a.Clock.Now().Add(skew).Before(info.Exp) {
		return false, "expired"
	}
	return true, ""
}

func (a *API) auth(w http.ResponseWriter, r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(h, "Bearer ")
	ok := false
	if strings.HasPrefix(h, "Bearer ") {
		ok, _ = a.Check(tok)
	}
	if !ok {
		a.mu.Lock()
		a.rejected++
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid token"}`))
	}
	return ok
}

func (a *API) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/api/v1/client-config" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"oidc_issuer": a.Issuer, "oidc_client_id": a.ClientID})
		return
	}
	if !a.auth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case path == "/api/v1/machines":
		_, _ = w.Write([]byte("[]"))
	case strings.HasPrefix(path, "/api/v1/machines/"):
		_ = json.NewEncoder(w).Encode(model.Machine{ID: a.machine, Hostname: "host-under-test"})
	case strings.HasPrefix(path, "/api/v1/operations/"):
		id := strings.TrimPrefix(path, "/api/v1/operations/")
		a.mu.Lock()
		st, ok := a.ops[id]
		a.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(model.Operation{ID: uuid.MustParse(id), MachineID: a.machine,
			Type: model.OpDeploy, Status: st, CreatedAt: time.Now().Add(-time.Minute)})
	case path == "/api/v1/logs/stream":
		a.stream(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}
}

// stream accepts a logs subscription, acknowledges it and, until dropped,
// sends the global terminal envelope for a subscribed op that completes.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer func() {
		a.mu.Lock()
		delete(a.conns, c)
		a.mu.Unlock()
		cancel()
		_ = c.CloseNow()
	}()
	_, raw, err := c.Read(ctx)
	if err != nil {
		return
	}
	var env ws.Envelope
	var sub ws.LogStreamSubscribeMsg
	if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Data, &sub) != nil {
		return
	}
	ack, _ := ws.MarshalEnvelope(ws.TypeSubscribed, "", ws.SubscribedMsg{})
	if c.Write(ctx, websocket.MessageText, ack) != nil {
		return
	}
	a.mu.Lock()
	a.conns[c] = cancel
	a.mu.Unlock()
	var id int64
	for {
		a.mu.Lock()
		st := a.ops[sub.OpID]
		a.mu.Unlock()
		if st == model.OpCompleted {
			id++
			opID, _ := uuid.Parse(sub.OpID)
			e := slogkit.Envelope{TS: time.Now(), Source: slogkit.SourceGlobal, OpID: opID, Msg: "operation completed",
				Extra: map[string]any{"status": string(model.OpCompleted)}}
			buf, _ := ws.MarshalEnvelope(ws.TypeLogStreamEnvelope, "", ws.LogStreamEnvelopeMsg{ID: id, Envelope: e})
			_ = c.Write(ctx, websocket.MessageText, buf)
			<-ctx.Done()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ParseGen extracts the family and generation from a refresh token.
func ParseGen(rt string) (fam, gen int, ok bool) {
	parts := strings.Split(rt, "-")
	if len(parts) != 3 || parts[0] != "rt" {
		return 0, 0, false
	}
	f, err1 := strconv.Atoi(parts[1])
	g, err2 := strconv.Atoi(parts[2])
	return f, g, err1 == nil && err2 == nil
}
