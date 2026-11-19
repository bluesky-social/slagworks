package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// clientInputServer is the production server (routes and middleware) over
// a fresh database, with "op-tok" as an operator token.
func clientInputServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	s := NewServer(Config{Store: st, RequiredGroups: []string{"platform"},
		Verifier: &fakeVerifier{claims: map[string]*tokenverify.Claims{"op-tok": {Subject: "op", Groups: []string{"platform"}}}}})
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, st
}

func operatorRequest(s *Server, method, path, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer op-tok")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	s.Echo().ServeHTTP(w, req)
	return w
}

// PUT /images/:name is keyed by the path name, so an "id" in the body (as
// in a row copied from GET /images) is not the client's to choose. One
// that belongs to another row used to reach the INSERT and fail on the
// primary key, answered with a 500.
func TestPutImageIgnoresAnIDInTheBody(t *testing.T) {
	s, st := clientInputServer(t)
	existing := &model.Image{Name: "noble-pxe", Version: "v1", SHA256: "aa", URL: "https://img.example/a.tar.gz"}
	if err := st.CreateImage(existing); err != nil {
		t.Fatal(err)
	}

	w := operatorRequest(s, http.MethodPut, "/api/v1/images/other", "application/json",
		`{"id":"`+existing.ID.String()+`","version":"v2","sha256":"bb","url":"https://img.example/b.tar.gz"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /images/other with another row's id = %d %s, want 200", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var got model.Image
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "other" || got.ID == uuid.Nil || got.ID == existing.ID {
		t.Errorf("response row = name %q id %s, want name other with an id of its own (not %s)", got.Name, got.ID, existing.ID)
	}
	images, err := st.ListImages()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]model.Image{}
	for _, img := range images {
		byName[img.Name] = img
	}
	if len(images) != 2 || byName["noble-pxe"].ID != existing.ID || byName["noble-pxe"].SHA256 != "aa" || byName["other"].ID != got.ID {
		t.Errorf("image rows after the PUT = %+v, want noble-pxe unchanged and other stored as returned", images)
	}
}

// Postgres cannot store U+0000 in text, so a NUL byte in any request text
// that reaches a write or a lookup used to fail there with SQLSTATE 22021,
// answered with a 500 carrying the raw database error. It is the client's
// error: 400, naming where the byte is. Every row below returned 500
// except the nested relay_pools one, which the handler's IP validation
// already refused; it pins that the body check walks nested values.
func TestOperatorRequestWithANULByteIs400(t *testing.T) {
	s, st := clientInputServer(t)
	id := uuid.New()
	if err := st.CreateMachine(&model.Machine{ID: id, NetBoxSerial: "SN-NUL"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: "pop-a", NetBoxSite: "pop-a",
		EphemeralPoolStart: "192.0.2.10", EphemeralPoolEnd: "192.0.2.250"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateStorageScript(&model.StorageScript{Name: "raid1", Content: "#!/bin/sh\n"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateNetworkScript(&model.NetworkScript{Name: "bond0", Content: "#!/bin/sh\n"}); err != nil {
		t.Fatal(err)
	}
	const (
		jsonType = "application/json"
		formType = "application/x-www-form-urlencoded"
	)
	cases := []struct{ name, method, path, contentType, body, where string }{
		{"image field", http.MethodPut, "/api/v1/images/other", jsonType,
			`{"version":"v\u0000","sha256":"aa","url":"u"}`, `body field "version"`},
		// A later duplicate null hides the NUL from a decoded map, but the
		// bound struct keeps it; a number float64 cannot hold fails a
		// decode into any, but the struct skips the unknown field.
		{"image field before a duplicate null", http.MethodPut, "/api/v1/images/other", jsonType,
			`{"version":"v\u0000","version":null,"sha256":"aa","url":"u"}`, "the request body"},
		{"image field beside a huge number", http.MethodPut, "/api/v1/images/other", jsonType,
			`{"version":"v\u0000","sha256":"aa","url":"u","x":1e400}`, `body field "version"`},
		{"image name in the path", http.MethodPut, "/api/v1/images/a%00b", jsonType,
			`{"version":"v1","sha256":"aa","url":"u"}`, "the request path"},
		{"site worker id", http.MethodPost, "/api/v1/site-workers", jsonType,
			`{"site_slug":"pop-a","worker_id":"gw\u0000"}`, `body field "worker_id"`},
		{"site worker in the path", http.MethodDelete, "/api/v1/site-workers/g%00w", "", "", "the request path"},
		{"site config in the path", http.MethodDelete, "/api/v1/site-configs/p%00", "", "", "the request path"},
		{"desired_os", http.MethodPatch, "/api/v1/machines/" + id.String(), jsonType,
			`{"desired_os":"noble\u0000"}`, `body field "desired_os"`},
		{"desired_os, form-encoded", http.MethodPatch, "/api/v1/machines/" + id.String(), formType,
			"desired_os=noble%00", `form field "desired_os"`},
		{"desired_os, multipart", http.MethodPatch, "/api/v1/machines/" + id.String(), "multipart/form-data; boundary=b",
			"--b\r\nContent-Disposition: form-data; name=\"desired_os\"\r\n\r\nnoble\x00\r\n--b--\r\n", `form field "desired_os"`},
		{"storage script content", http.MethodPut, "/api/v1/storage-scripts/raid1", jsonType,
			`{"content":"#!/bin/sh\necho \u0000\n"}`, `body field "content"`},
		{"storage script name in the path", http.MethodPut, "/api/v1/storage-scripts/raid%001", jsonType,
			`{"content":"#!/bin/sh\n"}`, "the request path"},
		{"storage script history path", http.MethodGet, "/api/v1/storage-scripts/raid%001/history", "", "", "the request path"},
		{"network script content", http.MethodPut, "/api/v1/network-scripts/bond0", jsonType,
			`{"content":"#!/bin/sh\necho \u0000\n"}`, `body field "content"`},
		{"network script history path", http.MethodGet, "/api/v1/network-scripts/bond%000/history", "", "", "the request path"},
		{"query parameter", http.MethodGet, "/api/v1/site-configs/pop-a/dhcp-lease-history?mac=aa%00", "", "", `query parameter "mac"`},
		{"nested body field", http.MethodPatch, "/api/v1/site-configs/pop-a", jsonType,
			`{"relay_pools":[{"subnet":"192.0.2.0/24","range_start":"192.0.2.10","range_end":"192.0.2.20","gateway":"192.0.2.1\u0000"}]}`, `body field "relay_pools"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := operatorRequest(s, tc.method, tc.path, tc.contentType, tc.body)
			var resp struct{ Message string }
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if w.Code != http.StatusBadRequest || !strings.HasPrefix(resp.Message, tc.where+" contains a NUL byte") {
				t.Errorf("%s %s = %d %s, want 400 naming %s", tc.method, tc.path, w.Code, strings.TrimSpace(w.Body.String()), tc.where)
			}
		})
	}

	// The check reads the body and puts it back: a clean request still
	// reaches the handler with its body intact, in each encoding.
	for _, enc := range []struct{ contentType, body, want string }{
		{jsonType, `{"desired_os":"noble"}`, "noble"},
		{formType, "desired_os=jammy", "jammy"},
		{"multipart/form-data; boundary=b", "--b\r\nContent-Disposition: form-data; name=\"desired_os\"\r\n\r\nplucky\r\n--b--\r\n", "plucky"},
	} {
		w := operatorRequest(s, http.MethodPatch, "/api/v1/machines/"+id.String(), enc.contentType, enc.body)
		m, err := st.GetMachine(id)
		if err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || m.DesiredOS != enc.want {
			t.Errorf("clean %s PATCH = %d %s, desired_os %q; want 200 and %q", enc.contentType, w.Code, strings.TrimSpace(w.Body.String()), m.DesiredOS, enc.want)
		}
	}
}
