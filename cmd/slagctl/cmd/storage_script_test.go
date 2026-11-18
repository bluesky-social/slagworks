package cmd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/google/uuid"
)

// TestStorageScriptUpdate_NoChanges verifies a byte-identical update
// short-circuits: no confirmation prompt, no PUT, exit clean.
func TestStorageScriptUpdate_NoChanges(t *testing.T) {
	content := "# slagworks:priority 100\necho hi"
	existing := model.StorageScript{ID: uuid.New(), Name: "asus-rs501a", Priority: 100, Content: content, CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.StorageScript{existing})
	})
	mux.HandleFunc("/api/v1/storage-scripts/asus-rs501a", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, content)
	out, err := execCmd(t, cl, "storage-script", "update", f, "--name", "asus-rs501a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no changes") {
		t.Errorf("expected 'no changes' output, got: %q", out)
	}
	if putCalled {
		t.Error("PUT must not be called for a byte-identical update")
	}
}

// TestStorageScriptUpdate_YesBypassesConfirm verifies --yes skips the
// prompt and applies the update, and the success message reports the
// version the store minted.
func TestStorageScriptUpdate_YesBypassesConfirm(t *testing.T) {
	id := uuid.New()
	existing := model.StorageScript{ID: id, Name: "asus-rs501a", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}
	newContent := "# slagworks:priority 200\necho new"

	var putBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.StorageScript{existing})
	})
	mux.HandleFunc("/api/v1/storage-scripts/asus-rs501a", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&putBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "asus-rs501a", "priority": 200, "content": newContent,
			"created_at": existing.CreatedAt, "version": 2,
		})
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, newContent)
	out, err := execCmd(t, cl, "storage-script", "update", f, "--name", "asus-rs501a", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if putBody["content"] != newContent {
		t.Errorf("PUT body content: got %v", putBody["content"])
	}
	if !strings.Contains(out, "version 2") {
		t.Errorf("expected version 2 in output, got: %q", out)
	}
	if !strings.Contains(out, "-echo old") || !strings.Contains(out, "+echo new") {
		t.Errorf("expected a unified diff in output, got: %q", out)
	}
}

// TestStorageScriptUpdate_JSONOutputIsPureJSON verifies --json --yes emits
// only the JSON response object on stdout, with no diff or priority text
// mixed in ahead of it (the update path used to write the human-readable
// diff unconditionally, before checking jsonOut).
func TestStorageScriptUpdate_JSONOutputIsPureJSON(t *testing.T) {
	id := uuid.New()
	existing := model.StorageScript{ID: id, Name: "asus-rs501a", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}
	newContent := "# slagworks:priority 200\necho new"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.StorageScript{existing})
	})
	mux.HandleFunc("/api/v1/storage-scripts/asus-rs501a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "asus-rs501a", "priority": 200, "content": newContent,
			"created_at": existing.CreatedAt, "version": 2,
		})
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, newContent)
	out, err := execCmd(t, cl, "storage-script", "update", f, "--name", "asus-rs501a", "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not valid JSON (diff text likely leaked onto stdout): %v\noutput: %q", err, out)
	}
	if got["version"] != float64(2) {
		t.Errorf("expected version 2 in JSON output, got: %+v", got)
	}
}

// TestStorageScriptUpdate_JSONNoChanges verifies the byte-identical
// short-circuit also respects --json: no plain-text "no changes" line.
func TestStorageScriptUpdate_JSONNoChanges(t *testing.T) {
	content := "# slagworks:priority 100\necho hi"
	existing := model.StorageScript{ID: uuid.New(), Name: "asus-rs501a", Priority: 100, Content: content, CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.StorageScript{existing})
	})
	mux.HandleFunc("/api/v1/storage-scripts/asus-rs501a", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, content)
	out, err := execCmd(t, cl, "storage-script", "update", f, "--name", "asus-rs501a", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json no-change output is not valid JSON: %v\noutput: %q", err, out)
	}
	if got["changed"] != false {
		t.Errorf("expected changed:false, got: %+v", got)
	}
	if putCalled {
		t.Error("PUT must not be called for a byte-identical update")
	}
}

// TestStorageScriptUpdate_JSONRefusesInteractiveConfirm verifies --json
// without --yes refuses before ever reaching the interactive huh prompt
// (which would otherwise write TUI/plain-text output onto the JSON stream),
// even if the test's own non-tty-ness didn't already catch it.
func TestStorageScriptUpdate_JSONRefusesInteractiveConfirm(t *testing.T) {
	existing := model.StorageScript{ID: uuid.New(), Name: "asus-rs501a", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.StorageScript{existing})
	})
	mux.HandleFunc("/api/v1/storage-scripts/asus-rs501a", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, "# slagworks:priority 200\necho new")
	_, err := execCmd(t, cl, "storage-script", "update", f, "--name", "asus-rs501a", "--json")
	if err == nil {
		t.Fatal("expected an error refusing interactive confirmation under --json")
	}
	if !strings.Contains(err.Error(), "--json") || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error should mention both --json and --yes, got: %v", err)
	}
	if putCalled {
		t.Error("PUT must not be called when confirmation is refused")
	}
}

// TestStorageScriptUpdate_RefusesWithoutYesNonTTY verifies that without
// --yes and without a tty (the case in any non-interactive invocation,
// including this test binary), update fails closed rather than prompting.
func TestStorageScriptUpdate_RefusesWithoutYesNonTTY(t *testing.T) {
	existing := model.StorageScript{ID: uuid.New(), Name: "asus-rs501a", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.StorageScript{existing})
	})
	mux.HandleFunc("/api/v1/storage-scripts/asus-rs501a", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, "# slagworks:priority 200\necho new")
	_, err := execCmd(t, cl, "storage-script", "update", f, "--name", "asus-rs501a")
	if err == nil {
		t.Fatal("expected an error refusing to update without --yes on a non-tty")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error should mention --yes, got: %v", err)
	}
	if putCalled {
		t.Error("PUT must not be called when confirmation is refused")
	}
}

// TestStorageScriptUpdate_NotFound verifies update on an unknown name
// errors rather than silently creating anything.
func TestStorageScriptUpdate_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.StorageScript{})
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, "# slagworks:priority 100\necho hi")
	_, err := execCmd(t, cl, "storage-script", "update", f, "--name", "does-not-exist", "--yes")
	if err == nil {
		t.Fatal("expected error for update of unknown script name")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the script, got: %v", err)
	}
}

// TestStorageScriptHistory_Columns verifies the history table renders
// version/head/priority/created columns and omits full content, while
// --json includes it.
func TestStorageScriptHistory_Columns(t *testing.T) {
	versions := []model.ScriptVersion{
		{Version: 1, Priority: 100, Content: "echo v1", CreatedAt: time.Now().Add(-time.Hour), Head: false},
		{Version: 2, Priority: 200, Content: "echo v2", CreatedAt: time.Now(), Head: true},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-scripts/asus-rs501a/history", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(versions)
	})
	cl := testServer(t, mux)

	out, err := execCmd(t, cl, "storage-script", "history", "asus-rs501a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "VERSION") || !strings.Contains(out, "HEAD") {
		t.Errorf("expected history table headers, got: %q", out)
	}
	if strings.Contains(out, "echo v1") || strings.Contains(out, "echo v2") {
		t.Errorf("table output must not include full content, got: %q", out)
	}

	jsonOut, err := execCmd(t, cl, "storage-script", "history", "asus-rs501a", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []model.ScriptVersion
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, jsonOut)
	}
	if len(got) != 2 || got[0].Content != "echo v1" {
		t.Errorf("expected full content in --json output, got: %+v", got)
	}
}

// writeTempScript writes content to a temp file and returns its path.
func writeTempScript(t *testing.T, content string) string {
	t.Helper()
	f := t.TempDir() + "/script.sh"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp script: %v", err)
	}
	return f
}
