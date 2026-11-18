package cliupdate

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// resolveToken returns a GitHub token or ("", false) if none is available.
//
// Resolution order:
//  1. GITHUB_TOKEN env var.
//  2. `gh auth token` (exec, 5s timeout). Uses the gh CLI's stored credentials,
//     which covers keychain-backed tokens on macOS and Linux.
//
// go-gh (github.com/cli/go-gh/v2) was considered for step 2 because it reads
// the keychain without shelling out. It was rejected: it pulls in go-gh's
// auth package and its dependencies, while `exec gh auth token` does the same
// in ~10 lines. The requirement is only "resolve a token noninteractively".
func resolveToken() (string, bool) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", false
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", false
	}
	return tok, true
}
