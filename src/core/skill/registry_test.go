package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, dir, text string) string {
	t.Helper()
	base := filepath.Join(root, dir)
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "SKILL.md"), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestProgressiveDisclosure(t *testing.T) {
	root := t.TempDir()
	base := writeSkill(t, root, "folder", "---\nname: example\ndescription: Test purpose\n---\nSECRET_INSTRUCTIONS")
	if err := os.WriteFile(filepath.Join(base, "notes.md"), []byte("SECRET_REFERENCE"), 0644); err != nil {
		t.Fatal(err)
	}
	r, errs := Discover(root)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(r.List()) != 1 || r.List()[0].Name != "example" {
		t.Fatal(r.List())
	}
	catalog := r.Catalog()
	if !strings.Contains(catalog, "Test purpose") || strings.Contains(catalog, "SECRET_") {
		t.Fatal(catalog)
	}
	body, err := r.Load("example")
	if err != nil || !strings.Contains(body, "SECRET_INSTRUCTIONS") || strings.Contains(body, "SECRET_REFERENCE") {
		t.Fatalf("%q %v", body, err)
	}
	ref, err := r.ReadResource("example", "notes.md")
	if err != nil || ref != "SECRET_REFERENCE" {
		t.Fatalf("%q %v", ref, err)
	}
	if _, err := r.Load("missing"); err == nil {
		t.Fatal("unknown skill accepted")
	}
}

func TestInvalidRegistrationsAreIsolated(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "a", "---\nname: valid\ndescription: First\n---\nBody")
	writeSkill(t, root, "b", "---\nname: valid\ndescription: Duplicate\n---\nBody")
	writeSkill(t, root, "c", "no front matter")
	writeSkill(t, root, "d", "---\nname: Invalid_Name\ndescription: Bad\n---\nBody")
	writeSkill(t, root, "e", "---\nname: empty\ndescription: Bad\n---\n")
	r, errs := Discover(root)
	if len(errs) != 4 || len(r.List()) != 1 {
		t.Fatalf("skills=%v errors=%v", r.List(), errs)
	}
	meta := r.List()
	meta[0].Name = "changed"
	if r.List()[0].Name != "valid" {
		t.Fatal("metadata leaked mutable state")
	}
}

func TestResourceBoundaries(t *testing.T) {
	root := t.TempDir()
	base := writeSkill(t, root, "example", "---\nname: example\ndescription: Example\n---\nBody")
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("private"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "large.md"), []byte(strings.Repeat("x", MaxFileBytes+1)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "binary"), []byte{0xff, 0}, 0644); err != nil {
		t.Fatal(err)
	}
	r, errs := Discover(root)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, path := range []string{"", outside, "../outside.md", "escape.md", "large.md", "binary", ".", "missing.md"} {
		if _, err := r.ReadResource("example", path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
}

func TestMissingDirectoryAndCRLF(t *testing.T) {
	r, errs := Discover(filepath.Join(t.TempDir(), "missing"))
	if len(errs) != 0 || r.Catalog() != "" {
		t.Fatalf("%v %v", r, errs)
	}
	root := t.TempDir()
	writeSkill(t, root, "example", "---\r\nname: example\r\ndescription: Example\r\n---\r\nBody")
	r, errs = Discover(root)
	if len(errs) != 0 || len(r.List()) != 1 {
		t.Fatalf("%v %v", r.List(), errs)
	}
}
