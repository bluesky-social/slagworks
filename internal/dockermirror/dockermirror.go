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
	"os"
	"strings"

	"github.com/ory/dockertest/v3/docker"
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

// Auth returns dockertest auth creds for the ECR cache. Empty
// AuthConfiguration when SLAGWORKS_ECR_AUTH_{USERNAME,PASSWORD,SERVER}
// are not all set — fine when Image() returned a bare Docker Hub path
// (anonymous pull) but a misconfiguration when Image() returned a
// cache-prefixed path. The mismatch surfaces as an auth-required
// error from the daemon at pull time, which is at least a fast,
// informative failure.
func Auth() docker.AuthConfiguration {
	u := os.Getenv("SLAGWORKS_ECR_AUTH_USERNAME")
	p := os.Getenv("SLAGWORKS_ECR_AUTH_PASSWORD")
	s := os.Getenv("SLAGWORKS_ECR_AUTH_SERVER")
	if u == "" || p == "" || s == "" {
		return docker.AuthConfiguration{}
	}
	return docker.AuthConfiguration{
		Username:      u,
		Password:      p,
		ServerAddress: s,
	}
}
