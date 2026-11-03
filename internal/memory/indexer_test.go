package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanVaultIncludesKnowledgeAndProjectsOnly(t *testing.T) {
	root := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("05_Knowledge/concepts/a.md", "A fact")
	write("04_Evergreen/b.md", "Evergreen")
	write("06_Projects/p/PROJECT.md", "Project")
	write("02_Daily/2026-11-02.md", "Private day")
	write("00_System/user.md", "secret")
	write("05_Knowledge/ignore.txt", "not markdown")
	write("05_Knowledge/image.png", "not text")
	if err := os.Symlink(filepath.Join(root, "05_Knowledge/concepts/a.md"), filepath.Join(root, "05_Knowledge/link.md")); err != nil {
		t.Fatal(err)
	}
	docs, err := ScanVault(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("docs = %+v", docs)
	}
	seen := map[string]bool{}
	for _, doc := range docs {
		seen[doc.SourceRef] = true
	}
	for _, want := range []string{"05_Knowledge/concepts/a.md", "04_Evergreen/b.md", "06_Projects/p/PROJECT.md"} {
		if !seen[want] {
			t.Fatalf("missing %s in %+v", want, docs)
		}
	}
}
