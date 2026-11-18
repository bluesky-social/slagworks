//go:build integration

package netboxtest

import (
	"github.com/ory/dockertest/v3/docker"

	"github.com/bluesky-social/slagworks/internal/dockermirror"
)

// mirrored rewrites a Docker Hub image path through the pull-through
// cache when one is configured; see internal/dockermirror.
func mirrored(path string) string {
	return dockermirror.Image(path)
}

// registryAuth returns dockertest auth creds for the ECR cache.
func registryAuth() docker.AuthConfiguration {
	return dockermirror.Auth()
}
