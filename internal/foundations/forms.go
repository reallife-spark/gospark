// Package foundations holds typed Go mirrors of the Step 89 shared contracts.
//
// The canonical byte rule is in canonical.go. These structs make the service
// boundary typed without changing the JSON shape: nullable contract fields are
// pointers, optional signature envelopes use omitempty, and payload/result
// fields stay JSON values because the contracts deliberately leave them open.
package foundations

// EventEnvelope is the canonical sparknet-event/1 control-bus message.
type EventEnvelope struct {
	SchemaVersion  string         `json:"schema_version"`
	EventID        string         `json:"event_id"`
	EventType      string         `json:"event_type"`
	Topic          string         `json:"topic"`
	SenderNodeID   string         `json:"sender_node_id"`
	CreatedAt      string         `json:"created_at"`
	ExpiresAt      string         `json:"expires_at"`
	Classification string         `json:"classification"`
	Department     *string        `json:"department"`
	VaultID        string         `json:"vault_id"`
	Scope          []string       `json:"scope"`
	Payload        map[string]any `json:"payload"`
	SignatureB64   *string        `json:"signature_b64,omitempty"`
}

// GraphNode is one page in the derived document-link graph.
type GraphNode struct {
	ID             string   `json:"id"`
	Path           string   `json:"path"`
	Title          string   `json:"title"`
	Category       string   `json:"category"`
	Tags           []string `json:"tags"`
	Classification string   `json:"classification"`
	Department     *string  `json:"department"`
	VaultID        string   `json:"vault_id"`
}

// GraphEdge is one resolved link. An unresolved link is reported separately.
type GraphEdge struct {
	Source     string         `json:"source"`
	Target     string         `json:"target"`
	Kind       string         `json:"kind"`
	Provenance map[string]any `json:"provenance"`
}

// Graph is the deterministic projection returned by a graph build.
type Graph struct {
	SchemaVersion string      `json:"schema_version"`
	Nodes         []GraphNode `json:"nodes"`
	Edges         []GraphEdge `json:"edges"`
	Unresolved    []any       `json:"unresolved"`
}

// SemanticResult is one retrieval hit in a hybrid bundle.
type SemanticResult struct {
	SchemaVersion  string         `json:"schema_version"`
	ResultID       string         `json:"result_id"`
	Source         string         `json:"source"`
	Path           string         `json:"path"`
	Score          float64        `json:"score"`
	Text           string         `json:"text"`
	Classification string         `json:"classification"`
	Provenance     map[string]any `json:"provenance"`
}

// AgenticResult is one tool result in a hybrid bundle.
type AgenticResult struct {
	SchemaVersion  string         `json:"schema_version"`
	ResultID       string         `json:"result_id"`
	Tool           string         `json:"tool"`
	Action         string         `json:"action"`
	Result         any            `json:"result"`
	Classification string         `json:"classification"`
	Provenance     map[string]any `json:"provenance"`
}

// HybridBundle carries semantic and agentic results side by side.
type HybridBundle struct {
	SchemaVersion  string           `json:"schema_version"`
	Query          string           `json:"query"`
	Semantic       []SemanticResult `json:"semantic"`
	Agentic        []AgenticResult  `json:"agentic"`
	GeneratedAt    string           `json:"generated_at"`
	Classification string           `json:"classification"`
	Department     *string          `json:"department"`
	VaultID        string           `json:"vault_id"`
}

// OperatorOS is the attested local OS identity an execution request runs under.
type OperatorOS struct {
	UID      int      `json:"uid"`
	EUID     int      `json:"euid"`
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	IsRoot   bool     `json:"is_root"`
}

// ExecRequest is the signed argv-only execution envelope.
type ExecRequest struct {
	SchemaVersion  string            `json:"schema_version"`
	RequestID      string            `json:"request_id"`
	NodeID         string            `json:"node_id"`
	UserID         string            `json:"user_id"`
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	OperatorOS     OperatorOS        `json:"operator_os"`
	Capabilities   []string          `json:"capabilities"`
	Nonce          string            `json:"nonce"`
	IssuedAt       string            `json:"issued_at"`
	ExpiresAt      string            `json:"expires_at"`
	SignatureB64   *string           `json:"signature_b64,omitempty"`
}

// ExecResponse is the daemon's signed result envelope.
type ExecResponse struct {
	SchemaVersion string  `json:"schema_version"`
	RequestID     string  `json:"request_id"`
	NodeID        string  `json:"node_id"`
	UserID        string  `json:"user_id"`
	ExitCode      int     `json:"exit_code"`
	Stdout        string  `json:"stdout"`
	Stderr        string  `json:"stderr"`
	StartedAt     string  `json:"started_at"`
	CompletedAt   string  `json:"completed_at"`
	Truncated     bool    `json:"truncated"`
	SignatureB64  *string `json:"signature_b64,omitempty"`
}

// CollabParticipant is one verified room member.
type CollabParticipant struct {
	NodeID     string  `json:"node_id"`
	UserID     string  `json:"user_id"`
	Department *string `json:"department"`
	Role       string  `json:"role"`
}

// CollabRoom is the bounded collaboration envelope.
type CollabRoom struct {
	SchemaVersion           string              `json:"schema_version"`
	RoomID                  string              `json:"room_id"`
	OwnerNodeID             string              `json:"owner_node_id"`
	Participants            []CollabParticipant `json:"participants"`
	Classification          string              `json:"classification"`
	Department              *string             `json:"department"`
	CreatedAt               string              `json:"created_at"`
	ExpiresAt               string              `json:"expires_at"`
	Status                  string              `json:"status"`
	CrossDepartmentApproved bool                `json:"cross_department_approved"`
}

// CollabMessage is one room message with provenance.
type CollabMessage struct {
	SchemaVersion  string         `json:"schema_version"`
	MessageID      string         `json:"message_id"`
	RoomID         string         `json:"room_id"`
	SenderNodeID   string         `json:"sender_node_id"`
	SenderUserID   string         `json:"sender_user_id"`
	CreatedAt      string         `json:"created_at"`
	Text           string         `json:"text"`
	Classification string         `json:"classification"`
	Department     *string        `json:"department"`
	Provenance     map[string]any `json:"provenance"`
}

// PackageArtifact is one digest-bound file in a package.
type PackageArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// PackageDependency is one closed package/version edge.
type PackageDependency struct {
	PackageID string `json:"package_id"`
	Version   string `json:"version"`
}

// PackageManifest is a signed package description. A manifest is data and
// never installs or executes itself.
type PackageManifest struct {
	SchemaVersion string              `json:"schema_version"`
	PackageID     string              `json:"package_id"`
	Version       string              `json:"version"`
	Description   string              `json:"description"`
	Artifacts     []PackageArtifact   `json:"artifacts"`
	Dependencies  []PackageDependency `json:"dependencies"`
	SignerNodeID  string              `json:"signer_node_id"`
	CreatedAt     string              `json:"created_at"`
	SignatureB64  *string             `json:"signature_b64,omitempty"`
}

// PackageIndex is the signed package index.
type PackageIndex struct {
	SchemaVersion string            `json:"schema_version"`
	UpdatedAt     string            `json:"updated_at"`
	Packages      []PackageManifest `json:"packages"`
	SignatureB64  *string           `json:"signature_b64,omitempty"`
}

// ABACResource is the resource side of an access decision.
type ABACResource struct {
	VaultID        string         `json:"vault_id"`
	Path           string         `json:"path"`
	Class          string         `json:"class"`
	Classification string         `json:"classification"`
	Department     *string        `json:"department"`
	Provenance     map[string]any `json:"provenance"`
}

// ABACDecision is the fail-closed result of an ABAC evaluation.
type ABACDecision struct {
	Allowed        bool     `json:"allowed"`
	Reason         string   `json:"reason"`
	Subject        string   `json:"subject"`
	SubjectKind    string   `json:"subject_kind"`
	Resource       string   `json:"resource"`
	VaultID        string   `json:"vault_id"`
	Classification string   `json:"classification"`
	EffectiveScope []string `json:"effective_scope"`
}
