package cmd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/google/uuid"
)

func TestImageList_Columns(t *testing.T) {
	images := []model.Image{
		{ID: uuid.New(), Name: "noble-pxe", Version: "24.04", Arch: "amd64", CreatedAt: time.Now().Add(-time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(images)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "image", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "noble-pxe") {
		t.Errorf("expected image name in output, got: %q", out)
	}
	if !strings.Contains(out, "amd64") {
		t.Errorf("expected arch in output, got: %q", out)
	}
}

// docs/user/images.md and troubleshooting.md tell operators to confirm an
// image roll by comparing the pinned SHA in `slagctl image list` with the
// version directory the worker's on-disk <name> link names, so the table
// must show the digest.
func TestImageList_ShowsPinnedSHA(t *testing.T) {
	sum := "0123456789ab" + strings.Repeat("c", 52)
	images := []model.Image{
		{ID: uuid.New(), Name: "noble-pxe", Version: "24.04", Arch: "amd64", SHA256: sum, CreatedAt: time.Now().Add(-time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(images)
	})
	out, err := execCmd(t, testServer(t, mux), "image", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SHA256") {
		t.Errorf("image list has no SHA256 column:\n%s", out)
	}
	if !strings.Contains(out, sum[:12]) {
		t.Errorf("image list does not show the pinned sha256 prefix %q:\n%s", sum[:12], out)
	}
}

// The API requires a non-empty sha256 but not its length, so a hand-registered
// row can carry a short value. The table prints it whole instead of panicking
// on a fixed-width slice.
func TestImageList_ShortSHAPrintedWhole(t *testing.T) {
	images := []model.Image{
		{ID: uuid.New(), Name: "noble-pxe", Version: "24.04", Arch: "amd64", SHA256: "abc123", CreatedAt: time.Now()},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(images)
	})
	out, err := execCmd(t, testServer(t, mux), "image", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "abc123") {
		t.Errorf("image list does not show the short sha256 %q:\n%s", "abc123", out)
	}
}

func TestImageList_JSON(t *testing.T) {
	images := []model.Image{
		{ID: uuid.New(), Name: "noble-pxe", Version: "24.04", Arch: "amd64"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(images)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "image", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []model.Image
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, out)
	}
}

func TestImageCreate_RequiredFlags(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "image", "create", "--name", "noble")
	if err == nil {
		t.Fatal("expected error for missing required flags")
	}
}

func TestImageCreate_Success(t *testing.T) {
	created := model.Image{ID: uuid.New(), Name: "noble-pxe", Version: "24.04"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "image", "create",
		"--name", "noble-pxe", "--version", "24.04",
		"--sha256", "abc123", "--url", "https://example.com/noble-pxe.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "noble-pxe") {
		t.Errorf("expected image name in output, got: %q", out)
	}
}

// TestImageFollowLatest_Success drives the full happy path: a stub
// bucket serves latest-sha.txt + the per-SHA sidecar, and the stub
// API receives a PUT with URL+SHA256 derived from those two files.
// Both stubs run on the same httptest.Server so we can use --bucket-url
// to point the bucket fetch at it.
func TestImageFollowLatest_Success(t *testing.T) {
	const gitSHA = "ee0d7114d22fabe98d14e05a1f3e6ee21be1829c"
	const contentSHA = "d007fce1fbb98d8c967ad606d615eae87774cc35d1d56ca48dea1de4a1518a08"

	mux := http.NewServeMux()
	// Stub the bucket pointer + sidecar (single noble-pxe prefix).
	mux.HandleFunc("/noble-pxe/latest-sha.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(gitSHA + "\n"))
	})
	mux.HandleFunc("/noble-pxe/"+gitSHA+".tar.gz.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(contentSHA + "\n"))
	})

	// Stub the API upsert. Capture the body to assert the URL and
	// SHA256 the command sent.
	var got model.Image
	mux.HandleFunc("/api/v1/images/noble-pxe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		got.ID = uuid.New()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(got)
	})

	cl, baseURL := testServerWithURL(t, mux)
	out, err := execCmd(t, cl, "image", "follow-latest",
		"--bucket-url", baseURL)
	if err != nil {
		t.Fatal(err)
	}

	wantURL := baseURL + "/noble-pxe/" + gitSHA + ".tar.gz"
	if got.URL != wantURL {
		t.Errorf("URL: got %q, want %q", got.URL, wantURL)
	}
	if got.SHA256 != contentSHA {
		t.Errorf("SHA256: got %q, want %q", got.SHA256, contentSHA)
	}
	if got.Name != "noble-pxe" {
		t.Errorf("Name: got %q, want noble-pxe", got.Name)
	}
	if !strings.Contains(out, gitSHA) {
		t.Errorf("expected git SHA in output, got: %q", out)
	}
}

// Without --bucket-url, follow-latest follows the bucket of the image row
// already registered under the same name.
func TestImageFollowLatest_DerivesBucketFromRegisteredImage(t *testing.T) {
	const oldSHA = "1111111111111111111111111111111111111111"
	const gitSHA = "ee0d7114d22fabe98d14e05a1f3e6ee21be1829c"
	const contentSHA = "d007fce1fbb98d8c967ad606d615eae87774cc35d1d56ca48dea1de4a1518a08"

	mux := http.NewServeMux()
	mux.HandleFunc("/bucket/noble-pxe/latest-sha.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(gitSHA + "\n"))
	})
	mux.HandleFunc("/bucket/noble-pxe/"+gitSHA+".tar.gz.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(contentSHA + "\n"))
	})
	var baseURL string
	mux.HandleFunc("/api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]model.Image{
			{Name: "other", URL: baseURL + "/elsewhere/other/" + oldSHA + ".tar.gz"},
			{Name: "noble-pxe", URL: baseURL + "/bucket/noble-pxe/" + oldSHA + ".tar.gz"},
		})
	})
	var got model.Image
	mux.HandleFunc("/api/v1/images/noble-pxe", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(got)
	})

	cl, url := testServerWithURL(t, mux)
	baseURL = url
	if _, err := execCmd(t, cl, "image", "follow-latest"); err != nil {
		t.Fatal(err)
	}
	if want := baseURL + "/bucket/noble-pxe/" + gitSHA + ".tar.gz"; got.URL != want {
		t.Errorf("URL: got %q, want %q", got.URL, want)
	}
}

// With no registered image and no --bucket-url there is nothing to follow,
// and the error says which flag to pass.
func TestImageFollowLatest_NoImageNoBucketURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	cl, _ := testServerWithURL(t, mux)
	_, err := execCmd(t, cl, "image", "follow-latest")
	if err == nil || !strings.Contains(err.Error(), "--bucket-url") {
		t.Fatalf("want an error naming --bucket-url, got %v", err)
	}
}
