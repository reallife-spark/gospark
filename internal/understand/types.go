// Package understand is the Go control/service half of the Understand-Anything
// graph: walking, fingerprinting, storage, query, diff, export and the MCP
// surface. It never parses a non-Go file and never writes authority; the
// Python core in the fabric's shared/ directory is the one parser.
package understand

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
	Order       int      `json:"order"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	NodeIDs     []string `json:"nodeIds"`
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

// Graph is the serialized knowledge graph.
type Graph struct {
	Version string     `json:"version"`
	Kind    string     `json:"kind,omitempty"`
	Project Project    `json:"project"`
	Nodes   []Node     `json:"nodes"`
	Edges   []Edge     `json:"edges"`
	Layers  []Layer    `json:"layers"`
	Tour    []TourStep `json:"tour"`
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

// DiffResult is the node/edge delta between two graphs.
type DiffResult struct {
	NodesAdded   []string   `json:"nodes_added"`
	NodesRemoved []string   `json:"nodes_removed"`
	NodesChanged []string   `json:"nodes_changed"`
	EdgesAdded   [][]string `json:"edges_added"`
	EdgesRemoved [][]string `json:"edges_removed"`
}
