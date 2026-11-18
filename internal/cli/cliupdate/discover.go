package cliupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// githubAPIBase is the GitHub REST API base. Overridable in tests to point at
// an httptest server.
var githubAPIBase = "https://api.github.com"

// discoverLatest returns the latest release tag for cfg's repo (e.g. "v0.3.0")
// and whether one was found. It performs a single GET against the GitHub
// Releases API — no download, no binary mutation.
//
// A token, when non-empty, is sent as "Authorization: token <tok>" — required
// for private repos, opportunistic (rate limits) for public ones. A 404 (no
// published release, or a private repo this token can't see) yields
// found=false with no error: the caller records it as indeterminate.
func discoverLatest(ctx context.Context, cfg Config, token string) (latest string, found bool, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest",
		strings.TrimRight(githubAPIBase, "/"), cfg.Owner, cfg.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to decode
	case http.StatusNotFound:
		// No published release, or (private repo) no access with this token.
		// Both are "nothing to report," not an error the caller should log.
		return "", false, nil
	default:
		return "", false, fmt.Errorf("github releases/latest: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false, fmt.Errorf("decoding releases/latest: %w", err)
	}
	tag := strings.TrimSpace(body.TagName)
	if tag == "" {
		return "", false, nil
	}
	return tag, true, nil
}

// discoverTimeout bounds a single releases-API GET. Short: this rides the
// detached child, and a hung GitHub shouldn't keep the child alive.
const discoverTimeout = 5 * time.Second
