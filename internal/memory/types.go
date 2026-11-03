// Package memory implements Sparknet's per-vault typed memory store.
//
// The store is deliberately local and dependency-free: one JSON document per
// vault, an in-memory inverted index, and no model, database server or signing
// key. It indexes knowledge the node already owns; it never becomes a second
// vault and never grants authority.
package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Schema is the record schema version.
const Schema = "sparknet-memory/1"

// StoreSchema is the on-disk vault schema version.
const StoreSchema = "sparknet-memory-store/1"

// CatalogSchema is the on-disk vault catalog schema version.
const CatalogSchema = "sparknet-memory-catalog/1"

// Type is the closed memory taxonomy.
type Type string

const (
	Episodic   Type = "episodic"
	Semantic   Type = "semantic"
	Procedural Type = "procedural"
	ShortTerm  Type = "short_term"
	Shared     Type = "shared"
)

// Types is the closed vocabulary in display order.
var Types = []Type{Episodic, Semantic, Procedural, ShortTerm, Shared}

// ValidType reports whether t is in the closed vocabulary.
func ValidType(t Type) bool {
	for _, candidate := range Types {
		if t == candidate {
			return true
		}
	}
	return false
}

// RetentionState is the lifecycle of one record. Suppression is reversible;
// deletion is a tombstone and keeps the audit trail.
type RetentionState string

const (
	Active     RetentionState = "active"
	Suppressed RetentionState = "suppressed"
	Deleted    RetentionState = "deleted"
)

// SourceKind names where a record came from. It is descriptive provenance,
// not a permission.
type SourceKind string

const (
	SourceSession  SourceKind = "session"
	SourceVault    SourceKind = "vault_page"
	SourceTool     SourceKind = "tool_run"
	SourceDelegate SourceKind = "delegate"
	SourceShare    SourceKind = "share"
	SourceRFI      SourceKind = "rfi"
	SourceUser     SourceKind = "user_note"
	SourceDaily    SourceKind = "daily_note"
)

// Scope is the exact tenant boundary. Empty fields do not filter.
type Scope struct {
	UserID     string `json:"user_id,omitempty"`
	ProjectID  string `json:"project_id,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
}

// Provenance records who wrote a record and from what. It is evidence, not
// authority; the signed envelope remains the authorization artifact.
type Provenance struct {
	OwnerID   string   `json:"owner_id,omitempty"`
	OwnerNode string   `json:"owner_node,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Model     string   `json:"model,omitempty"`
	ShareIDs  []string `json:"share_ids,omitempty"`
	LedgerRef string   `json:"ledger_ref,omitempty"`
}

// Consent is the user's capture/share/uplift posture for this record.
type Consent struct {
	Capture bool `json:"capture"`
	Share   bool `json:"share"`
	Uplift  bool `json:"uplift"`
}

// Record is one memory entry.
type Record struct {
	Schema         string         `json:"schema"`
	ID             string         `json:"memory_id"`
	VaultID        string         `json:"vault_id"`
	Type           Type           `json:"memory_type"`
	Subtype        string         `json:"subtype,omitempty"`
	Scope          Scope          `json:"scope,omitempty"`
	SourceKind     SourceKind     `json:"source_kind"`
	SourceRef      string         `json:"source_ref,omitempty"`
	SourceDigest   string         `json:"source_digest,omitempty"`
	Text           string         `json:"text,omitempty"`
	Summary        string         `json:"summary,omitempty"`
	Importance     float64        `json:"importance"`
	RetentionState RetentionState `json:"retention_state"`
	RetentionUntil string         `json:"retention_until,omitempty"`
	Consent        Consent        `json:"consent"`
	Provenance     Provenance     `json:"provenance,omitempty"`
	ContentHash    string         `json:"content_hash"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
	LastUsedAt     string         `json:"last_used_at,omitempty"`
	UseCount       int            `json:"use_count"`
	Supersedes     []string       `json:"supersedes,omitempty"`
	ConflictOf     string         `json:"conflict_of,omitempty"`
	Tombstone      string         `json:"tombstone,omitempty"`
}

// Query asks for a scoped set of records. Text is optional; an empty text
// returns recent records instead of a lexical match.
type Query struct {
	VaultID string `json:"vault_id"`
	Text    string `json:"query"`
	Types   []Type `json:"memory_types,omitempty"`
	Scope   Scope  `json:"scope,omitempty"`
	TopK    int    `json:"top_k"`
}

// Hit is one scored result.
type Hit struct {
	Record  Record             `json:"record"`
	Score   float64            `json:"score"`
	Reasons map[string]float64 `json:"reasons,omitempty"`
}

// Status is a content-free summary of one vault.
type Status struct {
	Schema  string         `json:"schema"`
	VaultID string         `json:"vault_id"`
	Records int            `json:"records"`
	Bytes   int64          `json:"bytes"`
	ByType  map[string]int `json:"by_type"`
	ByState map[string]int `json:"by_state"`
	Oldest  string         `json:"oldest,omitempty"`
	Newest  string         `json:"newest,omitempty"`
	Policy  map[string]any `json:"policy,omitempty"`
}

// Validate checks the record against the closed vocabulary and safe source
// shapes. It mutates only the fields that can be defaulted.
func (r *Record) Validate() error {
	if r.Schema == "" {
		r.Schema = Schema
	}
	if r.Schema != Schema {
		return fmt.Errorf("memory: unknown record schema %q", r.Schema)
	}
	if !ValidType(r.Type) {
		return fmt.Errorf("memory: unknown memory type %q", r.Type)
	}
	if r.Text == "" && r.Summary == "" {
		return errors.New("memory: a record needs text or summary")
	}
	if len(r.Text) > 64<<10 {
		return errors.New("memory: text exceeds 64 KiB")
	}
	if len(r.Summary) > 8<<10 {
		return errors.New("memory: summary exceeds 8 KiB")
	}
	if r.SourceRef != "" && !safeSourceRef(r.SourceRef) {
		return fmt.Errorf("memory: unsafe source_ref %q", r.SourceRef)
	}
	if r.Importance < 0 {
		r.Importance = 0
	}
	if r.Importance > 1 {
		r.Importance = 1
	}
	if r.RetentionState == "" {
		r.RetentionState = Active
	}
	switch r.RetentionState {
	case Active, Suppressed, Deleted:
	default:
		return fmt.Errorf("memory: unknown retention state %q", r.RetentionState)
	}
	if r.ID == "" {
		r.ID = newID()
	}
	if r.ContentHash == "" {
		r.ContentHash = hashRecord(r)
	}
	if r.CreatedAt == "" {
		r.CreatedAt = nowISO()
	}
	r.UpdatedAt = nowISO()
	return nil
}

func safeSourceRef(ref string) bool {
	if strings.ContainsRune(ref, 0) {
		return false
	}
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "\\") {
		return false
	}
	cleaned := path.Clean(strings.ReplaceAll(ref, "\\", "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return !strings.Contains(cleaned, "/../")
}

func hashRecord(r *Record) string {
	h := sha256.New()
	parts := []string{
		r.VaultID, string(r.Type), string(r.SourceKind), r.SourceRef,
		r.Scope.UserID, r.Scope.ProjectID, r.Scope.ThreadID,
		r.Scope.WorkflowID, r.Scope.AgentID, r.Text, r.Summary,
	}
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("mem-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
