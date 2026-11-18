// Command licenses writes THIRD-PARTY-LICENSES.md from the vendored module
// tree.
//
//	go run ./scripts/licenses          # rewrite the inventory
//	go run ./scripts/licenses -check   # exit 1 if the committed file is stale
//
// It reads the text of every license file under vendor/ and classifies it.
// A license it cannot classify, a license outside the allowed set, or a
// vendored module with no license file is an error, not a line in the
// output: someone has to look at it and either extend the classifier or
// drop the dependency.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const outputFile = "THIRD-PARTY-LICENSES.md"

func main() {
	check := flag.Bool("check", false, "compare with the committed file instead of writing it")
	root := flag.String("root", ".", "repository root")
	flag.Parse()

	want, err := generate(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
		os.Exit(1)
	}
	out := filepath.Join(*root, outputFile)
	if !*check {
		if err := os.WriteFile(out, want, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
			os.Exit(1)
		}
		return
	}
	got, err := os.ReadFile(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
		os.Exit(1)
	}
	if !bytes.Equal(got, want) {
		fmt.Fprintf(os.Stderr, "licenses: %s is stale: run 'just gen-licenses' and commit the result\n", outputFile)
		os.Exit(1)
	}
}

// allowed lists the licenses a vendored dependency may carry, in the order
// the inventory prints them. Anything else fails the run.
var allowed = []string{
	"Apache-2.0",
	"MIT",
	"BSD-3-Clause",
	"BSD-2-Clause",
	"ISC",
	"MPL-2.0",
	"CC-BY-SA-4.0",
}

// licenseFile matches the file names that hold license text. PATENTS and
// NOTICE files are not licenses and are left out.
var licenseFile = regexp.MustCompile(`(?i)^(licen[sc]e|copying|unlicense)([-._].*)?$`)

// skipped lists files whose name looks like a license but which do not
// license any code that reaches a slagworks binary. Each needs a reason.
var skipped = map[string]string{
	// A link to the origin of logo.png, an image no build uses.
	"modernc.org/memory/LICENSE-LOGO": "image attribution",
}

var spaces = regexp.MustCompile(`\s+`)

// classify returns the licenses whose text appears in a license file. One
// file can hold several: some projects concatenate the licenses of code they
// copied in.
func classify(text string) []string {
	t := strings.ToLower(spaces.ReplaceAllString(text, " "))
	// Markdown and comment decoration splits phrases that the checks below
	// look for.
	t = strings.NewReplacer("*", "", "#", "", "\"", "", "`", "").Replace(t)
	t = spaces.ReplaceAllString(t, " ")

	var ids []string
	add := func(id string) { ids = append(ids, id) }

	mpl := strings.Contains(t, "mozilla public license") && strings.Contains(t, "2.0")
	if mpl {
		add("MPL-2.0")
	}
	if strings.Contains(t, "apache license") && strings.Contains(t, "version 2.0") {
		add("Apache-2.0")
	}
	if strings.Contains(t, "permission is hereby granted, free of charge, to any person obtaining") {
		add("MIT")
	}
	// The four-clause BSD license adds an advertising obligation and is not
	// in the allowed set, so it must not pass as the three-clause one.
	if strings.Contains(t, "all advertising materials mentioning features or use of this software") {
		add("BSD-4-Clause")
	} else if strings.Contains(t, "redistribution and use in source and binary forms") {
		if strings.Contains(t, "neither the name") || strings.Contains(t, "may not be used to endorse") ||
			strings.Contains(t, "may be used to endorse") {
			add("BSD-3-Clause")
		} else {
			add("BSD-2-Clause")
		}
	}
	if strings.Contains(t, "permission to use, copy, modify, and/or distribute this software for any purpose") ||
		strings.Contains(t, "permission to use, copy, modify, and distribute this software for any purpose") {
		add("ISC")
	}
	if strings.Contains(t, "attribution-sharealike 4.0") {
		add("CC-BY-SA-4.0")
	}
	// The MPL names the GNU licenses as secondary licenses, so their names
	// in an MPL text do not mean the code is under them. The preamble of an
	// actual GNU license text does.
	gnuText := strings.Contains(t, "everyone is permitted to copy and distribute verbatim copies of this license document")
	if !mpl || gnuText {
		for _, gnu := range []struct{ phrase, id string }{
			{"gnu affero general public license", "AGPL"},
			{"gnu lesser general public license", "LGPL"},
			{"gnu general public license", "GPL"},
		} {
			if strings.Contains(t, gnu.phrase) {
				add(gnu.id)
				break
			}
		}
	}
	return ids
}

// entry is one directory under vendor/ that carries license files.
type entry struct {
	path     string   // import path of the directory
	licenses []string // sorted in the order of allowed
}

func (e entry) expression() string { return strings.Join(e.licenses, " + ") }

// scan classifies every license file under vendorDir and checks that every
// module in modules.txt is covered by one.
func scan(vendorDir string) ([]entry, error) {
	found := map[string]map[string]bool{}
	var problems []string

	// Reads go through a filesystem rooted at vendorDir, so a symlink in the
	// tree cannot lead the walk outside it.
	vendor := os.DirFS(vendorDir)
	err := fs.WalkDir(vendor, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !licenseFile.MatchString(d.Name()) || strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if _, ok := skipped[rel]; ok {
			return nil
		}
		text, err := fs.ReadFile(vendor, rel)
		if err != nil {
			return err
		}
		ids := classify(string(text))
		if len(ids) == 0 {
			problems = append(problems, fmt.Sprintf("vendor/%s: license text not recognized", rel))
			return nil
		}
		dir := path.Dir(rel)
		if found[dir] == nil {
			found[dir] = map[string]bool{}
		}
		for _, id := range ids {
			if !isAllowed(id) {
				problems = append(problems, fmt.Sprintf("vendor/%s: %s is not an allowed license", rel, id))
				continue
			}
			found[dir][id] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	modules, err := vendoredModules(filepath.Join(vendorDir, "modules.txt"))
	if err != nil {
		return nil, err
	}
	for _, m := range modules {
		if !covered(found, m) {
			problems = append(problems, fmt.Sprintf("vendor/%s: module has no license file", m))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, errors.New("cannot build the inventory:\n  " + strings.Join(problems, "\n  ") +
			"\nextend the classifier in scripts/licenses/main.go, or remove the dependency")
	}

	entries := make([]entry, 0, len(found))
	for dir, set := range found {
		e := entry{path: dir}
		for _, id := range allowed {
			if set[id] {
				e.licenses = append(e.licenses, id)
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].path) < strings.ToLower(entries[j].path)
	})
	return entries, nil
}

func isAllowed(id string) bool {
	for _, a := range allowed {
		if a == id {
			return true
		}
	}
	return false
}

// covered reports whether module, or a directory above it, has a license
// file. A repository that holds several modules often keeps one license at
// its root.
func covered(found map[string]map[string]bool, module string) bool {
	for dir := module; dir != "." && dir != "/" && dir != ""; dir = parent(dir) {
		if len(found[dir]) > 0 {
			return true
		}
	}
	return false
}

func parent(dir string) string {
	i := strings.LastIndex(dir, "/")
	if i < 0 {
		return ""
	}
	return dir[:i]
}

// vendoredModules returns the module paths in vendor/modules.txt that have
// packages in the vendor tree.
func vendoredModules(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var modules []string
	var current string
	hasPackages := false
	flush := func() {
		if current != "" && hasPackages {
			modules = append(modules, current)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "# "):
			flush()
			fields := strings.Fields(line)
			current, hasPackages = fields[1], false
		case strings.HasPrefix(line, "##"), strings.TrimSpace(line) == "":
		default:
			hasPackages = true
		}
	}
	flush()
	return modules, nil
}

func generate(root string) ([]byte, error) {
	entries, err := scan(filepath.Join(root, "vendor"))
	if err != nil {
		return nil, err
	}
	ref, err := os.ReadFile(filepath.Join(root, "assets", "ipxe", "UPSTREAM-REF"))
	if err != nil {
		return nil, err
	}
	ipxeRef := strings.TrimSpace(string(ref))

	groups := map[string][]entry{}
	for _, e := range entries {
		groups[e.expression()] = append(groups[e.expression()], e)
	}
	exprs := make([]string, 0, len(groups))
	for x := range groups {
		exprs = append(exprs, x)
	}
	sort.Slice(exprs, func(i, j int) bool {
		if len(groups[exprs[i]]) != len(groups[exprs[j]]) {
			return len(groups[exprs[i]]) > len(groups[exprs[j]])
		}
		return exprs[i] < exprs[j]
	})

	var b bytes.Buffer
	b.WriteString(`# Third-party licenses

<!-- Generated by scripts/licenses. Do not edit: run 'just gen-licenses'. -->

Slagworks' own code is licensed under MIT OR Apache-2.0 (see ` + "`LICENSE.txt`" + `).
This file lists the licenses of the third-party software it is built from or
ships with: the Go modules vendored under ` + "`vendor/`" + `, and the prebuilt boot
binaries in ` + "`assets/`" + `.

## Go modules

Each entry is a directory under ` + "`vendor/`" + ` that carries license text, which is
where the full text of that license is. An entry with more than one license
has more than one license file: either the code is offered under a choice of
them, or parts of the directory are under different ones.

| License | Entries |
|---------|--------:|
`)
	for _, x := range exprs {
		fmt.Fprintf(&b, "| %s | %d |\n", x, len(groups[x]))
	}
	for _, x := range exprs {
		fmt.Fprintf(&b, "\n### %s\n\n", x)
		for _, e := range groups[x] {
			fmt.Fprintf(&b, "- %s\n", e.path)
		}
	}

	b.WriteString(`
### Notes

- MPL-2.0 is copyleft at the level of a file: changes to the licensed files
  themselves must be shared. Slagworks uses these modules unmodified.
- CC-BY-SA-4.0 covers a specification document that ships next to a module's
  code, not the code.

## Prebuilt binaries in assets/

### iPXE (assets/ipxe/snponly.efi, assets/ipxe/undionly.kpxe)

Built from upstream iPXE at **` + ipxeRef + `** (` + "`assets/ipxe/UPSTREAM-REF`" + `) by the
` + "`build-ipxe.yaml`" + ` workflow.

iPXE is licensed under GPL-2.0, with the iPXE permissive exception for some
files. The binaries are separate executables that a machine runs at boot.
They are not linked into any slagworks Go binary.

The corresponding source is upstream iPXE at that tag:
https://github.com/ipxe/ipxe (tag ` + ipxeRef + `). The build configuration is in
` + "`.github/workflows/build-ipxe.yaml`" + `.
`)
	return b.Bytes(), nil
}
