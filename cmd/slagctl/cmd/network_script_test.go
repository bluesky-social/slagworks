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

// TestNetworkScriptUpdate_NoChanges is the network-script twin of
// TestStorageScriptUpdate_NoChanges.
func TestNetworkScriptUpdate_NoChanges(t *testing.T) {
	content := "# slagworks:priority 100\necho hi"
	existing := model.NetworkScript{ID: uuid.New(), Name: "pop1-l3-kworker", Priority: 100, Content: content, CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.NetworkScript{existing})
	})
	mux.HandleFunc("/api/v1/network-scripts/pop1-l3-kworker", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, content)
	out, err := execCmd(t, cl, "network-script", "update", f, "--name", "pop1-l3-kworker")
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

// TestNetworkScriptUpdate_YesBypassesConfirm is the network-script twin of
// TestStorageScriptUpdate_YesBypassesConfirm.
func TestNetworkScriptUpdate_YesBypassesConfirm(t *testing.T) {
	id := uuid.New()
	existing := model.NetworkScript{ID: id, Name: "pop1-l3-kworker", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}
	newContent := "# slagworks:priority 50\necho new"

	var putBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.NetworkScript{existing})
	})
	mux.HandleFunc("/api/v1/network-scripts/pop1-l3-kworker", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&putBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "pop1-l3-kworker", "priority": 50, "content": newContent,
			"created_at": existing.CreatedAt, "version": 2,
		})
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, newContent)
	out, err := execCmd(t, cl, "network-script", "update", f, "--name", "pop1-l3-kworker", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if putBody["content"] != newContent {
		t.Errorf("PUT body content: got %v", putBody["content"])
	}
	if !strings.Contains(out, "version 2") {
		t.Errorf("expected version 2 in output, got: %q", out)
	}
}

// TestNetworkScriptUpdate_JSONOutputIsPureJSON is the network-script twin of
// TestStorageScriptUpdate_JSONOutputIsPureJSON.
func TestNetworkScriptUpdate_JSONOutputIsPureJSON(t *testing.T) {
	id := uuid.New()
	existing := model.NetworkScript{ID: id, Name: "pop1-l3-kworker", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}
	newContent := "# slagworks:priority 50\necho new"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.NetworkScript{existing})
	})
	mux.HandleFunc("/api/v1/network-scripts/pop1-l3-kworker", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "pop1-l3-kworker", "priority": 50, "content": newContent,
			"created_at": existing.CreatedAt, "version": 2,
		})
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, newContent)
	out, err := execCmd(t, cl, "network-script", "update", f, "--name", "pop1-l3-kworker", "--yes", "--json")
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

// TestNetworkScriptUpdate_JSONNoChanges is the network-script twin of
// TestStorageScriptUpdate_JSONNoChanges.
func TestNetworkScriptUpdate_JSONNoChanges(t *testing.T) {
	content := "# slagworks:priority 100\necho hi"
	existing := model.NetworkScript{ID: uuid.New(), Name: "pop1-l3-kworker", Priority: 100, Content: content, CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.NetworkScript{existing})
	})
	mux.HandleFunc("/api/v1/network-scripts/pop1-l3-kworker", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, content)
	out, err := execCmd(t, cl, "network-script", "update", f, "--name", "pop1-l3-kworker", "--json")
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

// TestNetworkScriptUpdate_JSONRefusesInteractiveConfirm is the
// network-script twin of TestStorageScriptUpdate_JSONRefusesInteractiveConfirm.
func TestNetworkScriptUpdate_JSONRefusesInteractiveConfirm(t *testing.T) {
	existing := model.NetworkScript{ID: uuid.New(), Name: "pop1-l3-kworker", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.NetworkScript{existing})
	})
	mux.HandleFunc("/api/v1/network-scripts/pop1-l3-kworker", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, "# slagworks:priority 50\necho new")
	_, err := execCmd(t, cl, "network-script", "update", f, "--name", "pop1-l3-kworker", "--json")
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

// TestNetworkScriptUpdate_RefusesWithoutYesNonTTY is the network-script
// twin of TestStorageScriptUpdate_RefusesWithoutYesNonTTY.
func TestNetworkScriptUpdate_RefusesWithoutYesNonTTY(t *testing.T) {
	existing := model.NetworkScript{ID: uuid.New(), Name: "pop1-l3-kworker", Priority: 100, Content: "# slagworks:priority 100\necho old", CreatedAt: time.Now()}

	putCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.NetworkScript{existing})
	})
	mux.HandleFunc("/api/v1/network-scripts/pop1-l3-kworker", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, "# slagworks:priority 50\necho new")
	_, err := execCmd(t, cl, "network-script", "update", f, "--name", "pop1-l3-kworker")
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

// TestNetworkScriptUpdate_NotFound is the network-script twin of
// TestStorageScriptUpdate_NotFound.
func TestNetworkScriptUpdate_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.NetworkScript{})
	})
	cl := testServer(t, mux)

	f := writeTempScript(t, "# slagworks:priority 100\necho hi")
	_, err := execCmd(t, cl, "network-script", "update", f, "--name", "does-not-exist", "--yes")
	if err == nil {
		t.Fatal("expected error for update of unknown script name")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the script, got: %v", err)
	}
}

// TestNetworkScriptHistory_Columns is the network-script twin of
// TestStorageScriptHistory_Columns.
func TestNetworkScriptHistory_Columns(t *testing.T) {
	versions := []model.ScriptVersion{
		{Version: 1, Priority: 100, Content: "echo v1", CreatedAt: time.Now().Add(-time.Hour), Head: false},
		{Version: 2, Priority: 50, Content: "echo v2", CreatedAt: time.Now(), Head: true},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/network-scripts/pop1-l3-kworker/history", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(versions)
	})
	cl := testServer(t, mux)

	out, err := execCmd(t, cl, "network-script", "history", "pop1-l3-kworker")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "VERSION") || !strings.Contains(out, "HEAD") {
		t.Errorf("expected history table headers, got: %q", out)
	}
	if strings.Contains(out, "echo v1") || strings.Contains(out, "echo v2") {
		t.Errorf("table output must not include full content, got: %q", out)
	}

	jsonOut, err := execCmd(t, cl, "network-script", "history", "pop1-l3-kworker", "--json")
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
