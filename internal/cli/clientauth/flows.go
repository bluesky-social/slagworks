package clientauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oauthConfig builds the oauth2.Config from OIDC discovery. It sets the device
// endpoint manually: go-oidc's provider.Endpoint() only fills AuthURL/TokenURL,
// NOT the device endpoint, which must be pulled from the discovery doc via
// provider.Claims.
func oauthConfig(ctx context.Context, cfg Config) (*oauth2.Config, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery against %s: %w", cfg.Issuer, err)
	}
	ep := provider.Endpoint()
	var extra struct {
		DeviceAuthURL string `json:"device_authorization_endpoint"`
	}
	if err := provider.Claims(&extra); err != nil {
		return nil, fmt.Errorf("reading discovery claims: %w", err)
	}
	ep.DeviceAuthURL = extra.DeviceAuthURL
	return &oauth2.Config{
		ClientID: cfg.ClientID,
		Endpoint: ep,
		Scopes:   cfg.scopes(),
	}, nil
}

// Login runs the interactive flow (loopback+PKCE by default, device if
// cfg.Device) and returns the token. It does NOT write the cache — callers do.
func Login(ctx context.Context, cfg Config) (*oauth2.Token, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	oc, err := oauthConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Device {
		return loginDevice(ctx, cfg, oc)
	}
	return loginLoopback(ctx, cfg, oc)
}

// loginLoopback runs the authorization-code + PKCE flow: bind the fixed
// loopback callback, open the browser to the authorize URL, catch the code,
// exchange it with the PKCE verifier.
func loginLoopback(ctx context.Context, cfg Config, oc *oauth2.Config) (*oauth2.Token, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.LoopbackPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("binding loopback %s (is another %s login running?): %w\n  retry with --device for the device-code flow", addr, cfg.AppName, err)
	}
	defer func() { _ = ln.Close() }()

	oc.RedirectURL = fmt.Sprintf("http://%s/callback", addr)
	verifier := oauth2.GenerateVerifier()
	state, err := randState()
	if err != nil {
		return nil, err
	}

	authURL := oc.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
	)

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	// deliver does a non-blocking send: only the first callback hit lands a
	// result; later hits (browser refresh, double-fire, a stray request to the
	// local port) are dropped instead of blocking the handler goroutine forever
	// on the cap-1 channel.
	deliver := func(r result) {
		select {
		case resCh <- r:
		default:
		}
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Validate `state` FIRST and bail without delivering on mismatch. The
		// loopback port can receive stray requests (browser extensions, port
		// scanners, other local apps); none of them know our random `state`,
		// so anything that doesn't match is rejected with a 400 and the server
		// keeps waiting for the legitimate callback. This means a stray request
		// can't abort a real login — and a genuine IdP error (which echoes our
		// `state`) is still surfaced. The 5-minute timeout is the backstop.
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			_, _ = fmt.Fprintf(w, "Login failed: %s. You can close this tab.", e)
			deliver(result{err: fmt.Errorf("authorize error: %s", e)})
			return
		}
		_, _ = fmt.Fprintf(w, "%s: authenticated. You can close this tab.", cfg.AppName)
		deliver(result{code: q.Get("code")})
	})
	srv.Handler = mux
	go func() { _ = srv.Serve(ln) }()
	// Graceful shutdown (not Close): the main goroutine returns the instant the
	// callback delivers a result, which fires this defer; Close() would cut the
	// connection before the "you can close this tab" response flushed, leaving
	// the browser on a connection-reset error. Shutdown drains the in-flight
	// response first. Bounded so a wedged connection can't hang the CLI exit.
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	_, _ = fmt.Fprintln(cfg.stderr(), "Opening browser to authenticate with your identity provider...")
	_, _ = fmt.Fprintln(cfg.stderr(), "If it doesn't open, visit:\n  "+authURL)
	_ = openBrowser(authURL)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		tok, err := oc.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchanging code: %w", err)
		}
		return tok, nil
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for browser authentication")
	}
}

// loginDevice runs the device-authorization grant: request a code, check the
// verification page exists, print the URL + user code, poll until approved.
//
// Some providers serve the device grant only when it is explicitly enabled
// (Authentik needs a brand-level device-code flow). Without it the device
// endpoint may refuse the request, or it may issue a code while the
// verification page it names answers 404 (Authentik without the brand flow,
// or a proxy in front of the provider that does not route the page). Polling
// in the second case can only end when the code expires, so the page is
// checked first. Every such failure names loopback, the working default.
func loginDevice(ctx context.Context, cfg Config, oc *oauth2.Config) (*oauth2.Token, error) {
	wayOut := fmt.Sprintf("log in without --device (`%s auth login`)", cfg.AppName)
	if oc.Endpoint.DeviceAuthURL == "" {
		return nil, fmt.Errorf("the identity provider %s advertises no device-code endpoint; %s", cfg.Issuer, wayOut)
	}
	resp, err := oc.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("device authorization request to %s: %w\n  the device-code flow may not be enabled on the identity provider; %s", oc.Endpoint.DeviceAuthURL, err, wayOut)
	}
	if status := devicePageMissing(ctx, resp.VerificationURI); status != "" {
		return nil, fmt.Errorf("the identity provider %s issued a device code, but its device page %s answers %s, so the code cannot be entered there; %s, or ask the identity provider's administrator to serve that page", cfg.Issuer, resp.VerificationURI, status, wayOut)
	}
	if resp.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(cfg.stderr(), "To authenticate, open:\n  %s\n", resp.VerificationURIComplete)
	} else {
		_, _ = fmt.Fprintf(cfg.stderr(), "To authenticate, open:\n  %s\nand enter code: %s\n", resp.VerificationURI, resp.UserCode)
	}
	tok, err := oc.DeviceAccessToken(ctx, resp)
	if err != nil {
		return nil, fmt.Errorf("waiting for device approval: %w", err)
	}
	return tok, nil
}

// devicePageTimeout bounds the check that the device page exists.
const devicePageTimeout = 10 * time.Second

// devicePageMissing GETs the device page the provider named, following
// redirects, and returns the final status line when it is 404 or 410: the
// page does not exist, so no operator can enter the code. Every other outcome
// returns "" and the flow goes ahead: a page that turns away scripted clients
// (403, a bot challenge) or that this host cannot reach may still open in the
// operator's browser on another device. It checks verification_uri, not
// verification_uri_complete, so the probe does not carry the user code.
func devicePageMissing(ctx context.Context, page string) string {
	if page == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, devicePageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return resp.Status
	}
	return ""
}

func randState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func openBrowser(u string) error {
	if _, err := url.Parse(u); err != nil {
		return err
	}
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	default: // linux and friends
		cmd = "xdg-open"
	}
	return exec.Command(cmd, u).Start()
}
