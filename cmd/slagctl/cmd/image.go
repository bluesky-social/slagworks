package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/imagepull"
	"github.com/bluesky-social/slagworks/internal/model"
)

// imageBaseURL returns the URL prefix that holds an image's per-SHA tarballs
// and its latest-sha.txt pointer. With --bucket-url it is
// <bucket-url>/<name>. Without it, the prefix is taken from the image row
// already registered under that name: an operator names the bucket once,
// when registering the first image, and follow-latest keeps following it.
func imageBaseURL(ctx context.Context, bucketURL, imgName string) (string, error) {
	if bucketURL != "" {
		return strings.TrimRight(bucketURL, "/") + "/" + imgName, nil
	}
	var images []model.Image
	if err := apiClient.Get(ctx, "/api/v1/images", nil, &images); err != nil {
		return "", fmt.Errorf("fetching image list: %w", err)
	}
	for _, img := range images {
		if img.Name != imgName {
			continue
		}
		i := strings.LastIndex(img.URL, "/")
		isHTTP := strings.HasPrefix(img.URL, "https://") || strings.HasPrefix(img.URL, "http://")
		if !isHTTP || i < len("https://") {
			return "", fmt.Errorf("image %q is registered with URL %q, which follow-latest cannot derive a bucket from\n  pass --bucket-url <base URL that contains %s/latest-sha.txt>", imgName, img.URL, imgName)
		}
		return img.URL[:i], nil
	}
	return "", fmt.Errorf("no image named %q is registered, so there is no bucket to follow\n  pass --bucket-url <base URL that contains %s/latest-sha.txt>", imgName, imgName)
}

// gitSHAPattern matches a 40-char lowercase hex SHA1 (a slagworks repo
// commit SHA). Used to validate latest-sha.txt contents before
// constructing URLs from it — a malformed pointer would otherwise let
// us register a row pointing at a 404 or worse.
var gitSHAPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "manage images"}
	cmd.AddCommand(imageListCmd(), imageCreateCmd(), imageFollowLatestCmd(), imageSyncCmd(), imageDeleteCmd())
	return cmd
}

func imageDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "delete an image row",
		Long: `Deletes an image row by name or uuid. Site workers sync whatever image
rows exist; deleting a row pushes a snapshot to every site, and workers
stop tracking and booting that image when they apply it. Any
already-downloaded file stays on disk under the worker's local image
cache (nothing prunes it).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ident := args[0]
			var images []model.Image
			if err := apiClient.Get(ctx(cmd), "/api/v1/images", nil, &images); err != nil {
				return err
			}
			var matches []model.Image
			for _, img := range images {
				if img.ID.String() == ident || img.Name == ident {
					matches = append(matches, img)
				}
			}
			if len(matches) == 0 {
				return fmt.Errorf("no image matches %q", ident)
			}
			if len(matches) > 1 {
				return fmt.Errorf("%q matches %d images; use the uuid to disambiguate", ident, len(matches))
			}
			img := matches[0]

			if !yes {
				if jsonOut {
					return fmt.Errorf("refusing interactive confirmation with --json; pass --yes")
				}
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("refusing to delete without confirmation; pass --yes")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "name=%s sha256=%s\n", img.Name, img.SHA256)
				var confirmed bool
				if err := huh.NewConfirm().
					Title(fmt.Sprintf("Delete image %q?", img.Name)).
					Value(&confirmed).
					Run(); err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}

			// Delete by the resolved immutable UUID, not the name we
			// matched on: the row was already identified client-side, and
			// deleting by id avoids re-resolving a (mutable, reusable) name
			// at request time. DeleteImage accepts either.
			if err := apiClient.Delete(ctx(cmd), "/api/v1/images/"+img.ID.String()); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(),
					map[string]any{"deleted": true, "id": img.ID, "name": img.Name})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted image %s\n", img.Name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	return cmd
}

func imageListCmd() *cobra.Command {
	return watchable(&cobra.Command{
		Use:   "list",
		Short: "list images",
		RunE: func(cmd *cobra.Command, args []string) error {
			var images []model.Image
			if err := apiClient.Get(ctx(cmd), "/api/v1/images", nil, &images); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), images)
			}
			rows := make([][]string, len(images))
			for i, img := range images {
				rows[i] = []string{
					img.ID.String(),
					img.Name,
					img.Version,
					img.Arch,
					shortSHA256(img.SHA256),
					format.HumanDuration(img.CreatedAt),
				}
			}
			format.PrintTable(cmd.OutOrStdout(),
				[]string{"ID", "NAME", "VERSION", "ARCH", "SHA256", "CREATED"},
				rows)
			return nil
		},
	})
}

func imageCreateCmd() *cobra.Command {
	var imgName, version, arch, sha, url string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "register an image",
		RunE: func(cmd *cobra.Command, args []string) error {
			img := model.Image{
				Name:    imgName,
				Version: version,
				Arch:    arch,
				SHA256:  sha,
				URL:     url,
			}
			if err := apiClient.Post(ctx(cmd), "/api/v1/images", img, &img); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), img)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created image %s:%s (%s)\n", img.Name, img.Version, img.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&imgName, "name", "", "image name")
	cmd.Flags().StringVar(&version, "version", "", "version string")
	cmd.Flags().StringVar(&arch, "arch", "amd64", "architecture")
	cmd.Flags().StringVar(&sha, "sha256", "", "SHA256 checksum")
	cmd.Flags().StringVar(&url, "url", "", "download URL")
	cmd.MarkFlagRequired("name")
	cmd.MarkFlagRequired("version")
	cmd.MarkFlagRequired("sha256")
	cmd.MarkFlagRequired("url")
	return cmd
}

// imageFollowLatestCmd resolves the current latest <git-sha>.tar.gz
// for the requested flavor on S3 and registers (or updates) the
// matching Image row in the global controller to point at the
// immutable per-SHA URL.
//
// "Follow latest" is intentionally a CLI workflow rather than a
// runtime mode: the row in the controller always pins an immutable
// URL whose contents are content-addressed by the SHA in the path.
// imagepull.SyncImages can then verify (URL contents == row's
// SHA256) without ever needing to know what "latest" means. To pick
// up a new build, an operator re-runs this command. See
// docs/user/images.md.
func imageFollowLatestCmd() *cobra.Command {
	var version, arch, bucketURL, imgName string
	cmd := &cobra.Command{
		Use:   "follow-latest",
		Short: "register/update the PXE image row pointing at the current latest <sha>.tar.gz",
		Long: `Resolves <bucket>/<name>/latest-sha.txt to the current
git SHA, fetches the matching <sha>.tar.gz.sha256 sidecar to learn
the content sha256, and creates-or-updates the single Image row to
pin URL=<...>/<sha>.tar.gz with SHA256=<content-sha>.

There is ONE image: deploy and commission boot the same artifact in
different modes (the slagworks.mode= kernel cmdline), so there is no
--kind. The row always points at the immutable per-SHA URL; the worker
never fetches latest-sha.txt directly. To pick up a new build, re-run
this command (or run it on a cron).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := imageBaseURL(ctx(cmd), bucketURL, imgName)
			if err != nil {
				return err
			}
			pointerURL := base + "/latest-sha.txt"

			gitSHA, err := fetchHexLine(cmd.Context(), pointerURL)
			if err != nil {
				return fmt.Errorf("fetching %s: %w", pointerURL, err)
			}
			if !gitSHAPattern.MatchString(gitSHA) {
				return fmt.Errorf("%s returned %q which is not a 40-char hex SHA",
					pointerURL, gitSHA)
			}

			tarballURL := base + "/" + gitSHA + ".tar.gz"
			sidecarURL := tarballURL + ".sha256"

			contentSHA, err := fetchHexLine(cmd.Context(), sidecarURL)
			if err != nil {
				return fmt.Errorf("fetching %s: %w", sidecarURL, err)
			}
			if !sha256Pattern.MatchString(contentSHA) {
				return fmt.Errorf("%s returned %q which is not a 64-char hex sha256",
					sidecarURL, contentSHA)
			}

			img := model.Image{
				Name:    imgName,
				Version: version,
				Arch:    arch,
				SHA256:  contentSHA,
				URL:     tarballURL,
			}
			path := "/api/v1/images/" + imgName
			if err := apiClient.Put(ctx(cmd), path, img, &img); err != nil {
				return fmt.Errorf("upserting image row: %w", err)
			}

			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), img)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Pinned %s -> %s (sha256=%s)\n",
				img.Name, gitSHA, contentSHA[:12])
			return nil
		},
	}
	cmd.Flags().StringVar(&imgName, "name", "noble-pxe", "image row name / S3 prefix")
	cmd.Flags().StringVar(&version, "version", "24.04", "version string for the row")
	cmd.Flags().StringVar(&arch, "arch", "amd64", "architecture for the row")
	cmd.Flags().StringVar(&bucketURL, "bucket-url", "",
		"base URL of the image bucket (default: the bucket of the image row already registered under --name)")
	return cmd
}

// sha256Pattern matches a 64-char lowercase hex sha256 digest.
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// shortSHA256 returns the first 12 characters of an image row's sha256, the
// prefix operators compare against the version directory a worker's on-disk
// <name> link names (<name>@<first 16 hex digits>).
// Rows are normally written with a 64-char hex digest, but the API checks
// only that sha256 is non-empty, so a hand-registered value can be shorter;
// that is returned whole rather than panicking on the slice.
func shortSHA256(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// fetchHexLine GETs the URL and returns the trimmed body. Used for
// both latest-sha.txt and <sha>.tar.gz.sha256 — both files are
// hex-only-with-trailing-newline by convention. Caps the read at 256
// bytes so a misconfigured/oversized object doesn't pull a multi-MB
// payload through this code path.
func fetchHexLine(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func imageSyncCmd() *cobra.Command {
	var dir, imagesJSON string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "download images to a local directory (same code path as production site workers)",
		Long: `Downloads any missing or outdated images to the local directory.
This is the same code production site workers use (SyncImages).

Image metadata comes from the global API (default) or from --images-json
(for hosts that can't reach the API directly).

Initrds are stock Ubuntu initrds with the slagworks BOOT hook baked in at
build time (boot=slagworks sources it); there is no download-time repack.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var images []model.Image
			if imagesJSON != "" {
				if err := json.Unmarshal([]byte(imagesJSON), &images); err != nil {
					return fmt.Errorf("parsing --images-json: %w", err)
				}
			} else {
				if err := apiClient.Get(ctx(cmd), "/api/v1/images", nil, &images); err != nil {
					return fmt.Errorf("fetching image list: %w", err)
				}
			}
			if len(images) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No images registered")
				return nil
			}

			imgMap := make(map[string]*model.Image, len(images))
			for i := range images {
				imgMap[images[i].Name] = &images[i]
				fmt.Fprintf(cmd.OutOrStdout(), "  %s sha256=%s\n", images[i].Name, shortSHA256(images[i].SHA256))
			}

			log := slog.Default()
			if err := imagepull.SyncImages(log, dir, imgMap); err != nil {
				return fmt.Errorf("image sync: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Images synced to %s\n", dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "local directory for images (required)")
	cmd.Flags().StringVar(&imagesJSON, "images-json", "", "image metadata as JSON (alternative to API fetch)")
	cmd.MarkFlagRequired("dir")
	return cmd
}
