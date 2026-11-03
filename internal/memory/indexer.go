package memory

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Document is one vault page selected for semantic memory.
type Document struct {
	SourceRef string
	Text      string
	Summary   string
}

var indexRoots = map[string]struct{}{
	"04_Evergreen": {},
	"05_Knowledge": {},
	"06_Projects":  {},
}

var indexSkip = map[string]struct{}{
	".git": {}, ".opencode": {}, ".state": {}, ".cache": {}, "__pycache__": {},
	"node_modules": {}, "dist": {}, "_raw": {}, "_staging": {}, "01_Inbox": {},
	"00_System": {}, "02_Daily": {}, "03_Sources": {}, "07_Archives": {},
	"sessions": {}, "journal": {}, "references": {},
}

// ScanVault returns bounded markdown documents from the allowed vault
// directories. It never follows a symlink and never reads a key or state file.
func ScanVault(root string, maxBytes int64) ([]Document, error) {
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var documents []Document
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil || rel == "." {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 0 {
			return nil
		}
		if entry.IsDir() {
			name := parts[len(parts)-1]
			if _, skip := indexSkip[name]; skip {
				return filepath.SkipDir
			}
			if len(parts) == 1 {
				if _, ok := indexRoots[name]; !ok {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".markdown" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxBytes {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(raw)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if len(text) > 64<<10 {
			text = text[:64<<10]
		}
		source := filepath.ToSlash(rel)
		documents = append(documents, Document{
			SourceRef: source,
			Text:      text,
			Summary:   summarize(text),
		})
		if len(documents) >= 5000 {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return documents, nil
}

func summarize(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) <= 800 {
		return flat
	}
	return flat[:800]
}
