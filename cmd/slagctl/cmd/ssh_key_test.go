package cmd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/google/uuid"
)

func TestSSHKeyCommands(t *testing.T) {
	keyID := uuid.New()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode([]model.SSHKey{
				{ID: keyID, PublicKey: "ssh-ed25519 AAAAC3test test@host", Comment: "test"},
			})
		case "POST":
			var body struct {
				PublicKey string `json:"public_key"`
				Comment   string `json:"comment"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.SSHKey{
				ID: keyID, PublicKey: body.PublicKey, Comment: body.Comment,
			})
		}
	})
	mux.HandleFunc("/api/v1/ssh-keys/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusNoContent)
		}
	})

	cl := testServer(t, mux)

	// List
	out, err := execCmd(t, cl, "ssh-key", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "test@host") {
		t.Fatalf("list output missing key comment: %s", out)
	}

	// Add
	out, err = execCmd(t, cl, "ssh-key", "add", "ssh-ed25519 AAAAC3test test@host")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, "Added") {
		t.Fatalf("add output missing confirmation: %s", out)
	}

	// Remove
	out, err = execCmd(t, cl, "ssh-key", "remove", keyID.String())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(out, "Removed") {
		t.Fatalf("remove output missing confirmation: %s", out)
	}
}
