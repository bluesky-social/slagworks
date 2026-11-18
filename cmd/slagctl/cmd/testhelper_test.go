package cmd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
)

// testServer creates a test HTTP server and returns a client pointing at it.
func testServer(t *testing.T, mux *http.ServeMux) *slclient.Client {
	cl, _ := testServerWithURL(t, mux)
	return cl
}

// testServerWithURL is testServer plus the server's base URL, for
// tests that need to also point external code (e.g. the bucket-fetch
// in `image follow-latest`) at the same stub.
func testServerWithURL(t *testing.T, mux *http.ServeMux) (*slclient.Client, string) {
	t.Helper()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	cl := slclient.New(slclient.Config{BaseURL: ts.URL, HTTPClient: ts.Client()})
	return cl, ts.URL
}

// execCmd runs the root command with the given client injected and captures stdout.
func execCmd(t *testing.T, cl *slclient.Client, args ...string) (string, error) {
	t.Helper()
	root := cmd.NewRootCmd()
	cmd.SetAPIClient(root, cl)
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(new(bytes.Buffer))
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}
