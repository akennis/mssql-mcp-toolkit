package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materialises a map of relative path -> file body under a fresh
// temp directory and returns that directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

// A root file that only lists includes pulls the tools in from per-group
// directories, one tool per file, and each tool inherits its group from the
// directory it sits in.
func TestQueryToolsIncludeTree(t *testing.T) {
	root := writeTree(t, map[string]string{
		"index.yaml": `
groups:
  reference:
    label: things-reference
    port: 9201
    description: Lookups.
  directory:
    label: things-directory
    port: 9202
    description: People.
include:
  - tools/reference
  - tools/directory
`,
		"tools/reference/_notes.yaml": "this file is skipped because it starts with an underscore\n",
		"tools/reference/school_years.yaml": `
name: school_years
description: Every school year.
query: SELECT id, name FROM dbo.SchoolYear
outputFormat: csv
columns: [id, name]
`,
		"tools/reference/buildings.yaml": `
name: buildings
description: Every building.
query: SELECT id, name FROM dbo.Building
outputFormat: csv
columns: [id, name]
`,
		"tools/directory/find_person.yaml": `
name: find_person
group: directory
description: Find a person by name.
query: SELECT id, name FROM dbo.Person WHERE name LIKE @q
parameters:
  - name: q
    description: name fragment
outputFormat: csv
columns: [id, name]
`,
	})

	f, err := parseQueryToolsFile(filepath.Join(root, "index.yaml"), builtinToolNames("things"))
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}

	if len(f.Specs) != 3 {
		t.Fatalf("got %d specs, want 3", len(f.Specs))
	}
	// Directory include order in the root, then filename order within each dir.
	want := []struct{ name, group string }{
		{"buildings", "reference"},
		{"school_years", "reference"},
		{"find_person", "directory"},
	}
	for i, w := range want {
		if f.Specs[i].Name != w.name || f.Specs[i].Group != w.group {
			t.Errorf("spec %d = %s/%s, want %s/%s", i, f.Specs[i].Name, f.Specs[i].Group, w.name, w.group)
		}
	}

	if len(f.Groups) != 2 || f.Groups[0].Name != "reference" || f.Groups[1].Name != "directory" {
		t.Fatalf("groups = %+v, want [reference directory]", f.Groups)
	}
	if f.Groups[0].Port != 9201 || f.Groups[0].Label != "things-reference" {
		t.Errorf("reference group = %+v, want port 9201 label things-reference", f.Groups[0])
	}
}

// A tool whose explicit group contradicts the directory it lives in is a
// startup error, not a silent reassignment.
func TestQueryToolsIncludeGroupMismatch(t *testing.T) {
	root := writeTree(t, map[string]string{
		"index.yaml": "include: [tools/reference]\n",
		"tools/reference/oops.yaml": `
name: oops
group: directory
description: Mislabelled.
query: SELECT 1 AS a
outputFormat: scalar
`,
	})

	_, err := parseQueryToolsFile(filepath.Join(root, "index.yaml"), builtinToolNames("things"))
	if err == nil || !strings.Contains(err.Error(), `sets group "directory" but sits under directory "reference"`) {
		t.Fatalf("error = %v, want a group/directory mismatch complaint", err)
	}
}

// include: may also name a single file, and a cycle is reported rather than
// looping forever.
func TestQueryToolsIncludeCycle(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.yaml": "include: [b.yaml]\n",
		"b.yaml": "include: [a.yaml]\n",
	})
	_, err := parseQueryToolsFile(filepath.Join(root, "a.yaml"), nil)
	if err == nil || !strings.Contains(err.Error(), "include cycle") {
		t.Fatalf("error = %v, want an include-cycle error", err)
	}
}

// A duplicate tool name across two included files is caught, and the message
// still names the tool.
func TestQueryToolsIncludeDuplicateAcrossFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"index.yaml": "include: [tools/reference]\n",
		"tools/reference/one.yaml": `
name: dup
description: First.
query: SELECT 1 AS a
outputFormat: scalar
`,
		"tools/reference/two.yaml": `
name: dup
description: Second.
query: SELECT 2 AS a
outputFormat: scalar
`,
	})
	_, err := parseQueryToolsFile(filepath.Join(root, "index.yaml"), nil)
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("error = %v, want a duplicate-name error", err)
	}
}
