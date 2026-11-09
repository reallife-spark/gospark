package understand

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanFingerprintsAndExcludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/app.py", "print('hello')\n")
	writeFile(t, root, "node_modules/ignored.js", "function ignored() {}\n")
	writeFile(t, root, "ignored.py", "print('ignored')\n")
	writeFile(t, root, ".understandignore", "ignored.py\n")
	result, err := Scan(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, record := range result.Files {
		paths[record.Path] = true
	}
	if !paths["src/app.py"] {
		t.Fatalf("src/app.py missing from %v", paths)
	}
	if paths["node_modules/ignored.js"] {
		t.Fatal("default exclude node_modules was not applied")
	}
	if paths["ignored.py"] {
		t.Fatal(".understandignore pattern was not applied")
	}
	if len(result.Languages) != 1 || result.Languages[0] != "python" {
		t.Fatalf("languages = %v", result.Languages)
	}
}

func TestLanguageForAndTestPath(t *testing.T) {
	cases := map[string]string{
		"a/b.py":       "python",
		"a/b.go":       "go",
		"Dockerfile":   "dockerfile",
		"Makefile":     "makefile",
		".env":         "env",
		"a/b.unknown":  "",
		"compose.yaml": "docker-compose",
		"a/b.test.ts":  "typescript",
	}
	for path, want := range cases {
		if got := LanguageFor(path); got != want {
			t.Fatalf("LanguageFor(%q) = %q, want %q", path, got, want)
		}
	}
	for _, path := range []string{"tests/test_app.py", "src/app_test.go", "src/App.test.tsx"} {
		if !IsTestPath(path) {
			t.Fatalf("IsTestPath(%q) = false", path)
		}
	}
	if IsTestPath("src/app.py") {
		t.Fatal("src/app.py reported as a test")
	}
}
