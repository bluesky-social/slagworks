package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"apache", "Apache License\nVersion 2.0, January 2004", []string{"Apache-2.0"}},
		{"mit", "Permission is hereby granted, free of charge, to any person\nobtaining a copy of this software", []string{"MIT"}},
		{"bsd3", "Redistribution and use in source and binary forms, with or without modification...\nNeither the name of the copyright holder", []string{"BSD-3-Clause"}},
		{"bsd2", "Redistribution and use in source and binary forms, with or without modification, are permitted", []string{"BSD-2-Clause"}},
		{"isc", "Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee", []string{"ISC"}},
		{"mpl names gpl as secondary", "Mozilla Public License Version 2.0\n... the GNU General Public License, Version 2.0 ...", []string{"MPL-2.0"}},
		{"gpl", "GNU GENERAL PUBLIC LICENSE\nVersion 3", []string{"GPL"}},
		{"lgpl", "GNU Lesser General Public License", []string{"LGPL"}},
		{"markdown decoration", "**Permission is hereby granted**, free of charge, to any person obtaining a copy", []string{"MIT"}},
		{"two licenses in one file", "Apache License Version 2.0\n\nRedistribution and use in source and binary forms\nNeither the name of", []string{"Apache-2.0", "BSD-3-Clause"}},
		{"bsd4 is not bsd3", "Redistribution and use in source and binary forms...\nAll advertising materials mentioning features or use of this software must display...\nNeither the name of", []string{"BSD-4-Clause"}},
		{"mpl followed by a gpl text", "Mozilla Public License Version 2.0\n\nGNU GENERAL PUBLIC LICENSE\nEveryone is permitted to copy and distribute verbatim copies of this license document", []string{"MPL-2.0", "GPL"}},
		{"unknown", "All rights reserved.", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.text); !reflect.DeepEqual(got, c.want) {
				t.Errorf("classify = %v, want %v", got, c.want)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const mitText = "Permission is hereby granted, free of charge, to any person obtaining a copy"

func TestScanRejects(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name: "disallowed license",
			files: map[string]string{
				"modules.txt":           "# example.com/a v1.0.0\n## explicit\nexample.com/a\n",
				"example.com/a/LICENSE": "GNU GENERAL PUBLIC LICENSE Version 3",
			},
			wantErr: "GPL is not an allowed license",
		},
		{
			name: "unrecognized text",
			files: map[string]string{
				"modules.txt":           "# example.com/a v1.0.0\nexample.com/a\n",
				"example.com/a/LICENSE": "All rights reserved.",
			},
			wantErr: "license text not recognized",
		},
		{
			name: "module without a license",
			files: map[string]string{
				"modules.txt":           "# example.com/a v1.0.0\nexample.com/a\n# example.com/b v1.0.0\nexample.com/b\n",
				"example.com/a/LICENSE": mitText,
				"example.com/b/b.go":    "package b\n",
			},
			wantErr: "vendor/example.com/b: module has no license file",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range c.files {
				writeFile(t, filepath.Join(dir, name), content)
			}
			_, err := scan(dir)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("scan error = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestScanAccepts(t *testing.T) {
	dir := t.TempDir()
	// example.com/repo/sub has no license of its own and is covered by the
	// one at the repository root. example.com/unused has no packages in the
	// vendor tree, so it needs none.
	writeFile(t, filepath.Join(dir, "modules.txt"),
		"# example.com/repo v1.0.0\n## explicit; go 1.21\nexample.com/repo\n"+
			"# example.com/repo/sub v1.0.0\nexample.com/repo/sub\n"+
			"# example.com/unused v1.0.0\n## explicit\n")
	writeFile(t, filepath.Join(dir, "example.com/repo/LICENSE"), mitText)
	writeFile(t, filepath.Join(dir, "example.com/repo/LICENSE.docs"), "Attribution-ShareAlike 4.0 International")
	writeFile(t, filepath.Join(dir, "example.com/repo/PATENTS"), "not a license")
	writeFile(t, filepath.Join(dir, "example.com/repo/license.go"), "package repo\n")

	got, err := scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{{path: "example.com/repo", licenses: []string{"MIT", "CC-BY-SA-4.0"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %+v, want %+v", got, want)
	}
}

// TestInventoryIsCurrent fails when a dependency change has not been followed
// by 'just gen-licenses'.
func TestInventoryIsCurrent(t *testing.T) {
	root := filepath.Join("..", "..")
	want, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, outputFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale: run 'just gen-licenses' and commit the result", outputFile)
	}
}
