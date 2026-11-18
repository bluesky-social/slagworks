// Package dockermirror centralizes the Docker Hub → ECR pull-through
// cache rewriting used by tests that pull container images via
// dockertest. CI runners are configured to set
// SLAGWORKS_DOCKER_HUB_MIRROR (the cache prefix) and the
// SLAGWORKS_ECR_AUTH_* trio (creds for the cache); the helpers here
// turn those into a rewritten image path + auth tuple.
//
// Why centralized: previously every caller (internal/testpg,
// internal/netboxtest) duplicated the same env-var check + fall-through
// logic, and the fall-through silently let pulls hit registry-1.docker.io
// when env was unset — fine until the anonymous quota drained, then
// every PR exploded with 429s. The centralized helpers fail-fast in CI
// when env is missing instead of silently routing to docker.io.
package dockermirror

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/registry"
	mobyclient "github.com/moby/moby/client"
)

// envMirror is the env var holding a Docker Hub pull-through cache
// prefix (e.g. "123456789012.dkr.ecr.us-east-2.amazonaws.com/docker-hub-cache").
// Optional.
const envMirror = "SLAGWORKS_DOCKER_HUB_MIRROR"

// Image rewrites a Docker Hub image path through the configured
// mirror. Accepts both forms:
//
//	"library/postgres"       official image
//	"netboxcommunity/netbox" community image
//
// With the mirror set it returns "<prefix>/<path>". Unset, it returns
// the path for Docker Hub itself, with "library/" stripped to match
// dockertest's addressing of official images. Anonymous Docker Hub pulls
// are rate limited, so a CI system that runs these suites often should
// set the mirror.
func Image(path string) string {
	if prefix := os.Getenv(envMirror); prefix != "" {
		return prefix + "/" + path
	}
	return strings.TrimPrefix(path, "library/")
}

// Auth returns registry auth creds for the ECR cache. Empty
// AuthConfig when SLAGWORKS_ECR_AUTH_{USERNAME,PASSWORD,SERVER}
// are not all set — fine when Image() returned a bare Docker Hub path
// (anonymous pull) but a misconfiguration when Image() returned a
// cache-prefixed path. The mismatch surfaces as an auth-required
// error from the daemon at pull time, which is at least a fast,
// informative failure.
func Auth() registry.AuthConfig {
	u := os.Getenv("SLAGWORKS_ECR_AUTH_USERNAME")
	p := os.Getenv("SLAGWORKS_ECR_AUTH_PASSWORD")
	s := os.Getenv("SLAGWORKS_ECR_AUTH_SERVER")
	if u == "" || p == "" || s == "" {
		return registry.AuthConfig{}
	}
	return registry.AuthConfig{
		Username:      u,
		Password:      p,
		ServerAddress: s,
	}
}

// Pull makes repository:tag present on the daemon behind c, pulling it
// with Auth's creds when it is missing. dockertest v4 pulls a missing
// image without credentials, so callers pull through here before
// handing the image to it; dockertest then finds the image and skips
// its own pull.
func Pull(ctx context.Context, c *mobyclient.Client, repository, tag string) error {
	ref := repository + ":" + tag
	if _, err := c.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %s: %w", ref, err)
	}
	encoded, err := authconfig.Encode(Auth())
	if err != nil {
		return fmt.Errorf("encode registry auth for %s: %w", ref, err)
	}
	resp, err := c.ImagePull(ctx, ref, mobyclient.ImagePullOptions{RegistryAuth: encoded})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", ref, err)
	}
	defer func() { _ = resp.Close() }()
	// Wait drains the progress stream and returns any error the daemon
	// reported inside it (auth failure, unknown tag).
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull image %s: %w", ref, err)
	}
	return nil
}
