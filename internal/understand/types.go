// Package understand is the Go consumer half of the Understand-Anything graph:
// storage, query, export and the MCP surface over an existing graph. Scan and
// diff are adapters over the Python producer; gofrag keeps the native Go
// parser. It never writes authority and never opens a network port.
package understand

import "encoding/json"

// Node is one graph node. The field set mirrors
// contracts/understand-schema.json; optional fields are omitted when empty.
type Node struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Name          string         `json:"name"`
	FilePath      string         `json:"filePath,omitempty"`
	LineRange     []int          `json:"lineRange,omitempty"`
	Summary       string         `json:"summary"`
	Tags          []string       `json:"tags"`
	Complexity    string         `json:"complexity"`
	LanguageNotes string         `json:"languageNotes,omitempty"`
	DomainMeta    map[string]any `json:"domainMeta,omitempty"`
	KnowledgeMeta map[string]any `json:"knowledgeMeta,omitempty"`
	FigmaMeta     map[string]any `json:"figmaMeta,omitempty"`
}

// Edge is one directed graph edge.
type Edge struct {
	Source      string  `json:"source"`
	Target      string  `json:"target"`
	Type        string  `json:"type"`
	Direction   string  `json:"direction"`
	Description string  `json:"description,omitempty"`
	Weight      float64 `json:"weight"`
}

// Layer groups nodes by top-level directory.
type Layer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	NodeIDs     []string `json:"nodeIds"`
}

// TourStep is one ordered stop in the generated tour.
type TourStep struct {
	Order          int      `json:"order"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	NodeIDs        []string `json:"nodeIds"`
	LanguageLesson string   `json:"languageLesson,omitempty"`
}

// Project is the graph's project metadata.
type Project struct {
	Name          string   `json:"name"`
	Languages     []string `json:"languages"`
	Frameworks    []string `json:"frameworks"`
	Description   string   `json:"description"`
	AnalyzedAt    string   `json:"analyzedAt"`
	GitCommitHash string   `json:"gitCommitHash"`
}

// Graph is the serialized knowledge graph. Raw retains the original JSON so
// export and save do not silently drop fields this typed view does not name.
type Graph struct {
	Version string     `json:"version"`
	Kind    string     `json:"kind,omitempty"`
	Project Project    `json:"project"`
	Nodes   []Node     `json:"nodes"`
	Edges   []Edge     `json:"edges"`
	Layers  []Layer    `json:"layers"`
	Tour    []TourStep `json:"tour"`
	raw     json.RawMessage
}

// UnmarshalJSON loads the typed view and keeps the original bytes for export.
func (g *Graph) UnmarshalJSON(data []byte) error {
	type graphAlias Graph
	var value graphAlias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*g = Graph(value)
	g.raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON returns the original graph bytes when the graph was loaded. A
// graph constructed in Go is marshalled from its typed fields.
func (g Graph) MarshalJSON() ([]byte, error) {
	if len(g.raw) > 0 {
		return g.raw, nil
	}
	type graphAlias Graph
	return json.Marshal(graphAlias(g))
}

// Fragment is one file's contribution to a graph.
type Fragment struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// ScanError records a file the walker could not fingerprint.
type ScanError struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// FileRecord is one fingerprinted file.
type FileRecord struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	MTimeNS  int64  `json:"mtime_ns"`
}

// ScanResult is the deterministic walk output.
type ScanResult struct {
	Root      string       `json:"root"`
	Files     []FileRecord `json:"files"`
	Languages []string     `json:"languages"`
	Errors    []ScanError  `json:"errors"`
}
