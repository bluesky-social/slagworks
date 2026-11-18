//go:build integration

package netboxtest

import (
	"context"

	mobyclient "github.com/moby/moby/client"

	"github.com/bluesky-social/slagworks/internal/dockermirror"
)

// mirrored rewrites a Docker Hub image path through the pull-through
// cache when one is configured; see internal/dockermirror.
func mirrored(path string) string {
	return dockermirror.Image(path)
}

// pullMirrored pulls repository:tag with the ECR cache creds if the
// daemon lacks it; see dockermirror.Pull.
func pullMirrored(ctx context.Context, c *mobyclient.Client, repository, tag string) error {
	return dockermirror.Pull(ctx, c, repository, tag)
}
