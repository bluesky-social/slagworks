package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// A site config deleted while its PATCH is in flight stays deleted, and the
// PATCH reports 404 rather than re-creating the row or claiming success.
func TestUpdateSiteConfigDeletedMidRequestReturns404(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}, nil); err != nil {
		t.Fatal(err)
	}

	s := &Server{store: st, log: slog.Default(), echo: echo.New()}
	s.echo.PATCH("/api/v1/site-configs/:slug", s.updateSiteConfig)
	body := `{"promiscuous":true}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/site-configs/pop3",
		&midRequestBody{hook: func() {
			if err := st.DeleteSiteConfig("pop3"); err != nil {
				t.Errorf("mid-request delete: %v", err)
			}
		}, r: strings.NewReader(body)})
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, req)

	if _, err := st.GetSiteConfig("pop3"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("site config exists again after being deleted mid-PATCH (GetSiteConfig err=%v); want it to stay deleted", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("PATCH of a site config deleted mid-request: %d %s, want 404", rec.Code, rec.Body.String())
	}
}
