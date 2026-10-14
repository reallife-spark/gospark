// Package apiv1 holds the on-wire messages shared by the goSpark control and
// data planes.
//
// These are hand-written Go structs mirroring proto/sparknet/v1/*.proto. They
// are the types currently compiled; generating the Go from the .proto files is
// a deliberate follow-up (see docs/adr/0001-go-control-plane.md). Keeping the
// structs here means the build has no protoc dependency while the contract is
// still written down in one place per message.
package apiv1

// NodeHealth is telemetry reported by an agent to the control plane.
type NodeHealth struct {
	NodeID          string   `json:"node_id"`
	Fingerprint     string   `json:"fingerprint"`
	OverlayAddress  string   `json:"overlay_address"`
	WireGuardPubKey string   `json:"wireguard_pubkey,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	LoadPermille    uint32   `json:"load_permille"`
	KVCacheBytes    uint64   `json:"kv_cache_bytes,omitempty"`
	Serving         bool     `json:"serving"`
	ReportedAtUnix  int64    `json:"reported_at_unix"`
}

// EscalationTarget is where a prompt should be resolved.
type EscalationTarget string

const (
	TargetUnspecified EscalationTarget = "unspecified"
	TargetEdge        EscalationTarget = "edge"
	TargetDGX         EscalationTarget = "dgx"
	TargetCluster     EscalationTarget = "cluster"
)

// EscalationDecision is the router's advisory answer. It never blocks a caller.
type EscalationDecision struct {
	Target                EscalationTarget `json:"target"`
	Reason                string           `json:"reason"`
	EstimatedPromptTokens uint32           `json:"estimated_prompt_tokens"`
	AsyncRequired         bool             `json:"async_required"`
}

// RegisterNode is the agent -> control handshake.
type RegisterNode struct {
	Health         *NodeHealth `json:"health"`
	RegistryDigest string      `json:"registry_digest,omitempty"`
}

// NodeAuthorization is one Master-signed entry in the node registry: which node
// this is, what it may hold, and whether it is revoked.
//
// It mirrors the fabric's registry entry (contracts/identity-schema.json,
// "grant"/registry section; see peer-node/manifests/node-authorizations.json).
// The registry, not the node, is the authority: a peer cannot widen itself, and
// `revoked` is inside the signed registry so only the Master can flip it.
type NodeAuthorization struct {
	NodeID        string   `json:"node_id"`
	Fingerprint   string   `json:"fingerprint"`
	PublicKeyB64  string   `json:"public_key_b64"`
	OverlayIPv4   string   `json:"overlay_ipv4,omitempty"`
	Capabilities  []string `json:"capabilities"`
	IssuedAtUnix  int64    `json:"issued_at_unix"`
	ExpiresAtUnix int64    `json:"expires_at_unix,omitempty"`
	Revoked       bool     `json:"revoked"`
}

// NodeRegistry is the Master-signed set of node authorizations, distributed to
// peers over the bus. It is fail-closed: an empty registry authorizes nobody.
//
// The whole document is signed by the Master operational key, so an attacker
// with storage write access can neither widen an entry nor reinstate a revoked
// one — both would need a fresh signature. IssuedAtUnix increases on every
// re-sign, which lets a peer refuse a rolled-back (older) registry.
type NodeRegistry struct {
	SchemaVersion      string              `json:"schema_version"`
	MasterPublicKeyB64 string              `json:"master_public_key_b64"`
	IssuedAtUnix       int64               `json:"issued_at_unix"`
	ExpiresAtUnix      int64               `json:"expires_at_unix,omitempty"`
	Authorizations     []NodeAuthorization `json:"authorizations"`
	SignatureB64       string              `json:"signature_b64,omitempty"`
}

// RegistrySchemaVersion is the only accepted registry schema version.
const RegistrySchemaVersion = "1.0"

// TaskEnvelope is a signed compute task (see compute.proto).
type TaskEnvelope struct {
	Fingerprint        string   `json:"fingerprint"`
	Capabilities       []string `json:"capabilities"`
	TaskID             string   `json:"task_id"`
	Operation          string   `json:"operation"`
	ParamsJSON         string   `json:"params_json"`
	ContentInSignature bool     `json:"content_in_signature"`
	Nonce              string   `json:"nonce"`
	IssuedAtUnix       int64    `json:"issued_at_unix"`
	ExpiresAtUnix      int64    `json:"expires_at_unix"`
	SignatureB64       string   `json:"signature_b64"`
}

// ResultEnvelope is an attested compute result (see compute.proto).
type ResultEnvelope struct {
	TaskID        string `json:"task_id"`
	NodeID        string `json:"node_id"`
	Fingerprint   string `json:"fingerprint"`
	Operation     string `json:"operation"`
	Result        string `json:"result,omitempty"`
	ResultDigest  string `json:"result_digest,omitempty"`
	CompletedUnix int64  `json:"completed_at_unix"`
	SignatureB64  string `json:"signature_b64"`
}

// ContextSpec names what a cluster job retrieves.
type ContextSpec struct {
	Source string `json:"source"`
	Query  string `json:"query"`
	TopK   uint32 `json:"top_k"`
}

// Budget bounds a cluster job.
type Budget struct {
	MaxNodes      uint32 `json:"max_nodes"`
	MaxRounds     uint32 `json:"max_rounds"`
	TokensPerNode uint32 `json:"tokens_per_node"`
}

// SubTask is one participant's view of a cluster job.
type SubTask struct {
	NodeID      string   `json:"node_id"`
	SubtaskID   string   `json:"subtask_id"`
	Instruction string   `json:"instruction"`
	ContextRefs []string `json:"context_refs,omitempty"`
}

// ClusterJob is the signed cluster job (see cluster.proto).
type ClusterJob struct {
	JobID           string       `json:"job_id"`
	ConversationID  string       `json:"conversation_id,omitempty"`
	InitiatorNodeID string       `json:"initiator_node_id"`
	CreatedAtUnix   int64        `json:"created_at_unix"`
	ExpiresAtUnix   int64        `json:"expires_at_unix"`
	Mode            string       `json:"mode"`
	Prompt          string       `json:"prompt"`
	RunningSummary  string       `json:"running_summary,omitempty"`
	Context         *ContextSpec `json:"context,omitempty"`
	Budget          *Budget      `json:"budget,omitempty"`
	Subtasks        []SubTask    `json:"subtasks"`
	SignatureB64    string       `json:"signature_b64"`
	RatificationB64 string       `json:"ratification_b64,omitempty"`
}

// Citation attributes a piece of an answer to a page on a node.
type Citation struct {
	Page   string `json:"page"`
	NodeID string `json:"node_id"`
}

// ClusterResult is one participant's attested partial (see cluster.proto).
type ClusterResult struct {
	JobID         string     `json:"job_id"`
	SubtaskID     string     `json:"subtask_id"`
	NodeID        string     `json:"node_id"`
	Fingerprint   string     `json:"fingerprint"`
	Answer        string     `json:"answer"`
	Citations     []Citation `json:"citations,omitempty"`
	Confidence    float64    `json:"confidence"`
	Model         string     `json:"model,omitempty"`
	Tokens        uint32     `json:"tokens"`
	CompletedUnix int64      `json:"completed_at_unix"`
	ResultDigest  string     `json:"result_digest,omitempty"`
	SignatureB64  string     `json:"signature_b64"`
}

// BlockHeader heads a streamed NVFP4 block (see kvtransfer.proto).
type BlockHeader struct {
	BlockID       string  `json:"block_id"`
	OriginNodeID  string  `json:"origin_node_id"`
	Digest        string  `json:"digest"`
	NumElements   uint32  `json:"num_elements"`
	NumBlocks     uint32  `json:"num_blocks"`
	GlobalScale   float32 `json:"global_scale"`
	DType         string  `json:"dtype"`
	CreatedAtUnix int64   `json:"created_at_unix"`
}

// SemanticDelta advertises mirrored hashes for delta sync.
type SemanticDelta struct {
	Namespace   string   `json:"namespace"`
	HaveDigests []string `json:"have_digests,omitempty"`
	WantDigests []string `json:"want_digests,omitempty"`
}
