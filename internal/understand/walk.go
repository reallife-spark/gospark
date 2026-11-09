package understand

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const maxFileBytes = 2_000_000

var defaultExcludes = map[string]struct{}{
	".git": {}, ".hg": {}, ".svn": {}, ".ua": {}, ".understand-anything": {},
	".venv": {}, "venv": {}, "node_modules": {}, "__pycache__": {},
	".pytest_cache": {}, ".ruff_cache": {}, ".mypy_cache": {}, ".tox": {},
	".state": {}, ".cache": {}, ".obsidian": {}, "dist": {}, "build": {},
	"target": {}, "coverage": {}, "htmlcov": {}, "vendor": {}, "third_party": {},
	"_staging": {}, "_archives": {}, "references": {},
}

var languageBySuffix = map[string]string{
	".py": "python", ".pyi": "python", ".go": "go",
	".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".mts": "typescript", ".cts": "typescript",
	".sh": "shell", ".bash": "shell", ".zsh": "shell",
	".md": "markdown", ".markdown": "markdown",
	".json": "json", ".jsonc": "json", ".yaml": "yaml", ".yml": "yaml", ".toml": "toml",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
	".cs": "csharp", ".java": "java", ".kt": "kotlin", ".kts": "kotlin", ".rs": "rust",
	".rb": "ruby", ".php": "php", ".swift": "swift", ".scala": "scala", ".dart": "dart",
	".lua": "lua", ".sql": "sql", ".graphql": "graphql", ".gql": "graphql", ".proto": "protobuf",
	".tf": "terraform", ".tfvars": "terraform", ".xml": "xml", ".html": "html", ".htm": "html",
	".css": "css", ".scss": "css", ".sass": "css", ".csv": "csv", ".txt": "plaintext",
}

var languageByName = map[string]string{
	"Dockerfile": "dockerfile", "dockerfile": "dockerfile",
	"Makefile": "makefile", "makefile": "makefile", "GNUmakefile": "makefile",
	"Jenkinsfile": "jenkinsfile",
}

var testPathRE = regexp.MustCompile(`(^|/)(tests?|__tests__|spec)(/|$)|(^|/)test_[^/]+$|_test\.[A-Za-z0-9]+$|\.test\.[A-Za-z0-9]+$|\.spec\.[A-Za-z0-9]+$`)

// LanguageFor maps a project-relative path to the closed registry id.
func LanguageFor(rel string) string {
	name := path.Base(rel)
	if language, ok := languageByName[name]; ok {
		return language
	}
	if strings.HasPrefix(name, ".env") {
		return "env"
	}
	if strings.Contains("/"+rel, "/.github/workflows/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
		return "github-actions"
	}
	switch name {
	case "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml":
		return "docker-compose"
	}
	return languageBySuffix[strings.ToLower(path.Ext(name))]
}

// IsTestPath reports whether a path is a test file by the contract's common
// conventions. It is used by the graph linker, never to exclude a file.
func IsTestPath(rel string) bool { return testPathRE.MatchString(rel) }

func loadIgnore(root string) ([]string, error) {
	file, err := os.Open(filepath.Join(root, ".understandignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, scanner.Err()
}

func isIgnored(rel string, patterns []string) bool {
	ignored := false
	for _, pattern := range patterns {
		negated := strings.HasPrefix(pattern, "!")
		candidate := strings.TrimPrefix(pattern, "!")
		candidate = strings.TrimPrefix(candidate, "/")
		if matchPattern(rel, candidate) {
			ignored = !negated
		}
	}
	return ignored
}

func matchPattern(rel, pattern string) bool {
	if strings.HasSuffix(pattern, "/") {
		prefix := strings.TrimSuffix(pattern, "/")
		return rel == prefix || strings.HasPrefix(rel, prefix+"/")
	}
	if ok, _ := path.Match(pattern, rel); ok {
		return true
	}
	ok, _ := path.Match(pattern, path.Base(rel))
	return ok
}

// Scan walks root deterministically and fingerprints every supported file.
func Scan(root string, excludes []string) (ScanResult, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return ScanResult{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return ScanResult{}, err
	}
	if !info.IsDir() {
		return ScanResult{}, fmt.Errorf("not a directory: %s", abs)
	}
	ignore, err := loadIgnore(abs)
	if err != nil {
		return ScanResult{}, err
	}
	patterns := append(append([]string{}, excludes...), ignore...)
	result := ScanResult{Root: abs, Files: []FileRecord{}, Languages: []string{}, Errors: []ScanError{}}
	err = filepath.WalkDir(abs, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(abs, current)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if current == abs {
				return nil
			}
			_, excluded := defaultExcludes[entry.Name()]
			if excluded || isIgnored(rel, patterns) {
				return filepath.SkipDir
			}
			return nil
		}
		if isIgnored(rel, patterns) {
			return nil
		}
		language := LanguageFor(rel)
		if language == "" {
			return nil
		}
		stat, statErr := entry.Info()
		if statErr != nil {
			result.Errors = append(result.Errors, ScanError{Path: rel, Reason: "unreadable"})
			return nil
		}
		if stat.Size() > maxFileBytes {
			result.Errors = append(result.Errors, ScanError{Path: rel, Reason: "too-large"})
			return nil
		}
		records, readErr := fingerprint(abs, rel, language, stat)
		if readErr != nil {
			result.Errors = append(result.Errors, ScanError{Path: rel, Reason: readErr.Error()})
			return nil
		}
		result.Files = append(result.Files, records)
		return nil
	})
	if err != nil {
		return ScanResult{}, err
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	languages := map[string]struct{}{}
	for _, record := range result.Files {
		languages[record.Language] = struct{}{}
	}
	for language := range languages {
		result.Languages = append(result.Languages, language)
	}
	sort.Strings(result.Languages)
	return result, nil
}

func fingerprint(root, rel, language string, stat fs.FileInfo) (FileRecord, error) {
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return FileRecord{}, err
	}
	defer file.Close()
	hasher := sha256.New()
	sample := make([]byte, 4096)
	n, readErr := io.ReadFull(file, sample)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		return FileRecord{}, readErr
	}
	if strings.ContainsRune(string(sample[:n]), 0) {
		return FileRecord{}, fmt.Errorf("binary")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return FileRecord{}, err
	}
	if _, err := io.Copy(hasher, file); err != nil {
		return FileRecord{}, err
	}
	return FileRecord{
		Path:     rel,
		Language: language,
		SHA256:   hex.EncodeToString(hasher.Sum(nil)),
		Size:     stat.Size(),
		MTimeNS:  stat.ModTime().UnixNano(),
	}, nil
}

// GitCommit returns HEAD for the tree, or the empty string outside git.
func GitCommit(root string) string {
	command := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		return ""
	}
	commit := strings.TrimSpace(string(output))
	if regexp.MustCompile(`^[0-9a-f]{7,64}$`).MatchString(commit) {
		return commit
	}
	return ""
}
