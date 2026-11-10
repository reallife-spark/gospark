package understand

import (
	"context"
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

func sharedScript(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "shared", "sparknet-understand.py"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shared understand script not present: %v", err)
	}
	return path
}

func TestScanUsesPythonPolicy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/app.py", "print('hello')\n")
	writeFile(t, root, "node_modules/ignored.js", "function ignored() {}\n")
	writeFile(t, root, "ignored.py", "print('ignored')\n")
	writeFile(t, root, ".understandignore", "ignored.py\n")
	result, err := Scan(context.Background(), "python3", sharedScript(t), root, nil)
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

func TestScanFailsClosedWithoutProducer(t *testing.T) {
	_, err := Scan(
		context.Background(),
		"python3",
		filepath.Join(t.TempDir(), "missing-understand.py"),
		t.TempDir(),
		nil,
	)
	if err == nil {
		t.Fatal("Scan succeeded without the Python producer")
	}
}
