// Package main_test asserts the contract between the published
// single PXE image tarball (noble-pxe) and the URLs the slagworks agent
// (+ BOOT hook) fetches at runtime.
//
// Iteration cost is sub-second: this is a static check against the
// build.sh source plus the assets/initramfs/scripts/slagworks BOOT
// hook source. It catches "agent fetches a path the build doesn't
// publish" drift in milliseconds rather than 12 minutes of QEMU
// integration.
//
// Running: `go test -tags manifestcheck ./scripts/image-build/...`.
// Build tag avoids pulling this into the production tree.
//go:build manifestcheck

package main_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// expectedImageFiles is the canonical list of files the image-build
// pipeline must publish under <image>/. These are the entries the single
// noble-pxe.tar.gz must contain.
//
// Source: every distinct `files/images/<image>/<NAME>` URL grepped
// out of cmd/slagworks-agent and the slagworks BOOT hook. If you add a
// new download URL, add the filename here AND update build.sh's
// customize-hook to emit it AND add it to imageFiles in
// internal/imagepull, which refuses a tarball that lacks one.
var expectedImageFiles = map[string]string{
	"vmlinuz":       "PXE-bootable kernel; downloaded by iPXE during deploy and copied to ESP at install time",
	"initrd":        "stock Ubuntu initrd carrying the slagworks BOOT hook (/scripts/slagworks); iPXE chains into it, stock /init sources the hook for boot=slagworks",
	"initrd-local":  "stock Ubuntu initrd for ESP/local boot; the post-deploy boot uses this so root=UUID= works without a network round-trip",
	"root.squashfs": "deploy environment root filesystem; fetched by the BOOT hook and mounted via overlayfs",
}

// TestAgentURLsMatchPublishedManifest greps the agent + BOOT hook
// for every files/images/<image>/<NAME> URL and confirms every NAME
// is in expectedImageFiles. Prevents the post-redesign class of bug
// where the agent fetched initrd-local but build.sh stopped emitting
// it.
func TestAgentURLsMatchPublishedManifest(t *testing.T) {
	urls := collectImageURLs(t)
	if len(urls) == 0 {
		t.Fatal("no files/images/<image>/<NAME> URLs found; grep regex needs work")
	}

	missing := map[string][]string{} // file -> source files where it's referenced
	for _, ref := range urls {
		if _, ok := expectedImageFiles[ref.name]; !ok {
			missing[ref.name] = append(missing[ref.name], ref.source)
		}
	}
	if len(missing) > 0 {
		var names []string
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("agent code references files not in expectedImageFiles map:")
		for _, n := range names {
			t.Errorf("  %s — referenced by %v", n, missing[n])
		}
		t.Errorf("either add to expectedImageFiles (and ensure build.sh emits it) " +
			"or remove the reference from agent/overlay code")
	}
}

// TestBuildScriptSanityCheckCoversAllExpectedFiles asserts that
// build.sh's "Sanity-check artifacts" loop verifies the existence of
// every file in expectedImageFiles. That loop is the explicit
// "what the tarball ships" contract; if a name is missing from it,
// the build won't fail on a missing file and we'd silently publish
// a tarball that's missing artifacts the agent expects.
//
// The check is deliberately conservative: we look for the literal
// filename inside the sanity-check `for f in ... ; do` loop body
// rather than try to parse it. Adding to the list there is the
// concrete fix for missing files.
func TestBuildScriptSanityCheckCoversAllExpectedFiles(t *testing.T) {
	build, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	src := string(build)

	// Locate the sanity-check loop. Anchor on the comment and the
	// for-loop header so we can isolate the expected-name list.
	loopStart := strings.Index(src, "# Sanity-check artifacts")
	if loopStart < 0 {
		t.Fatal("build.sh: 'Sanity-check artifacts' comment missing; refactor moved the loop?")
	}
	loopEnd := strings.Index(src[loopStart:], "done")
	if loopEnd < 0 {
		t.Fatal("build.sh: no 'done' after sanity-check comment; can't bound the loop")
	}
	loopBody := src[loopStart : loopStart+loopEnd]

	// The sanity-check loop names files in a shell `for f in ...; do`
	// — bordered by space or list-terminating punctuation. Use a
	// word-boundary check that handles all of those forms.
	wordBoundaryRE := regexp.MustCompile(`[A-Za-z0-9_.\-]+`)
	loopWords := map[string]struct{}{}
	for _, w := range wordBoundaryRE.FindAllString(loopBody, -1) {
		loopWords[w] = struct{}{}
	}

	for name, why := range expectedImageFiles {
		if _, ok := loopWords[name]; !ok {
			t.Errorf("build.sh sanity-check loop doesn't list %q (purpose: %s)",
				name, why)
		}
	}
}

type urlRef struct {
	name   string
	source string
}

// collectImageURLs greps the agent + BOOT hook source for any
// `files/images/<image>/<NAME>` URL pattern and extracts NAME.
//
// The agent uses Go fmt strings (`files/images/%s/%s`); the BOOT hook
// shell uses `$SLAG_IMAGE/` interpolation. Both forms get a single
// regex because we only care about the literal trailing component.
func collectImageURLs(t *testing.T) []urlRef {
	t.Helper()
	sources := []string{
		"../../cmd/slagworks-agent/main.go",
		"../../assets/initramfs/scripts/slagworks",
	}

	// The path component after `images/<image>/` — match shell var
	// `$SLAG_IMAGE/`, Go fmt `%s/`, and any literal directory.
	re := regexp.MustCompile(`files/images/(?:[A-Za-z0-9_$%{}]+)/([A-Za-z0-9_.\-]+)`)

	var out []urlRef
	for _, path := range sources {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		matches := re.FindAllStringSubmatch(string(body), -1)
		for _, m := range matches {
			out = append(out, urlRef{name: m[1], source: path})
		}
	}
	return out
}
