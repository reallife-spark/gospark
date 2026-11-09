package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ErrVaultNotFound is returned when a vault id is not in the catalog.
var ErrVaultNotFound = errors.New("memory: vault not found")

// ErrVaultForbidden is returned when a vault path is outside every allowed root.
var ErrVaultForbidden = errors.New("memory: vault path is outside the allowed roots")

// Config configures a Manager.
type Config struct {
	// Root holds the catalog and every per-vault store. It is never a vault.
	Root string
	// NodeID namespaces vault ids so two nodes with the same path never share
	// an id by accident.
	NodeID string
	// AllowedVaultRoots bounds which directories may be registered. Empty means
	// the caller has already performed that check; the service always sets it.
	AllowedVaultRoots []string
	// Now is injectable so retention tests never sleep.
	Now func() time.Time
}

// Manager owns the vault catalog and one store per vault.
type Manager struct {
	cfg  Config
	mu   sync.RWMutex
	byID map[string]string
	open map[string]*Vault
	now  func() time.Time
}

// Vault is one vault's memory store. All reads and writes are scoped to this
// vault; there is no cross-vault query path.
type Vault struct {
	ID   string
	Path string

	mu      sync.RWMutex
	file    string
	policy  map[string]any
	records map[string]*Record
	hashes  map[string]string
	index   map[string]map[string]struct{}
}

type catalogFile struct {
	Schema string            `json:"schema"`
	Vaults map[string]string `json:"vaults"`
}

type vaultFile struct {
	Schema  string    `json:"schema"`
	VaultID string    `json:"vault_id"`
	Records []*Record `json:"records"`
}

// NewManager opens or creates the catalog. A corrupt catalog refuses startup:
// losing the mapping between a vault id and its path would make a user's
// memory silently disappear.
func NewManager(cfg Config) (*Manager, error) {
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		return nil, errors.New("memory: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("memory: root: %w", err)
	}
	cfg.Root = abs
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Join(abs, "vaults"), 0o700); err != nil {
		return nil, fmt.Errorf("memory: create root: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("memory: secure root: %w", err)
	}
	m := &Manager{
		cfg:  cfg,
		byID: map[string]string{},
		open: map[string]*Vault{},
		now:  cfg.Now,
	}
	catalogPath := filepath.Join(abs, "catalog.json")
	raw, err := os.ReadFile(catalogPath)
	switch {
	case err == nil:
		var catalog catalogFile
		if err := json.Unmarshal(raw, &catalog); err != nil {
			return nil, fmt.Errorf("memory: catalog is unreadable: %w", err)
		}
		if catalog.Schema != CatalogSchema {
			return nil, fmt.Errorf("memory: unknown catalog schema %q", catalog.Schema)
		}
		for id, path := range catalog.Vaults {
			if id == "" || !filepath.IsAbs(path) {
				return nil, fmt.Errorf("memory: catalog has an invalid vault entry %q", id)
			}
			m.byID[id] = path
		}
	case errors.Is(err, os.ErrNotExist):
		if err := m.saveCatalog(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("memory: read catalog: %w", err)
	}
	return m, nil
}

// Root returns the store root.
func (m *Manager) Root() string { return m.cfg.Root }

// VaultForPath resolves a vault path to its stable id and opens it.
func (m *Manager) VaultForPath(path string) (*Vault, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("memory: vault path: %w", err)
	}
	abs = filepath.Clean(abs)
	if !m.allowed(abs) {
		return nil, ErrVaultForbidden
	}
	id := m.vaultID(abs)

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.open[id]; ok {
		return existing, nil
	}
	if _, exists := m.byID[id]; !exists {
		m.byID[id] = abs
		if err := m.saveCatalog(); err != nil {
			delete(m.byID, id)
			return nil, err
		}
	}
	vault, err := openVault(m.cfg.Root, id, abs)
	if err != nil {
		return nil, err
	}
	m.open[id] = vault
	return vault, nil
}

// VaultByID opens a vault from the catalog.
func (m *Manager) VaultByID(id string) (*Vault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path, ok := m.byID[id]
	if !ok {
		return nil, ErrVaultNotFound
	}
	if existing := m.open[id]; existing != nil {
		return existing, nil
	}
	vault, err := openVault(m.cfg.Root, id, path)
	if err != nil {
		return nil, err
	}
	m.open[id] = vault
	return vault, nil
}

// Vaults lists the catalog as id -> path. It is metadata, never content.
func (m *Manager) Vaults() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.byID))
	for id, path := range m.byID {
		out[id] = path
	}
	return out
}

func (m *Manager) vaultID(absPath string) string {
	h := sha256.New()
	h.Write([]byte(m.cfg.NodeID))
	h.Write([]byte{0})
	h.Write([]byte(absPath))
	return hex.EncodeToString(h.Sum(nil))
}

func (m *Manager) allowed(path string) bool {
	if len(m.cfg.AllowedVaultRoots) == 0 {
		return true
	}
	for _, root := range m.cfg.AllowedVaultRoots {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return true
		}
	}
	return false
}

func (m *Manager) saveCatalog() error {
	payload, err := json.MarshalIndent(catalogFile{Schema: CatalogSchema, Vaults: m.byID}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(m.cfg.Root, "catalog.json"), append(payload, '\n'))
}

func openVault(root, id, path string) (*Vault, error) {
	dir := filepath.Join(root, "vaults", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create vault: %w", err)
	}
	v := &Vault{
		ID:      id,
		Path:    path,
		file:    filepath.Join(dir, "records.json"),
		policy:  map[string]any{},
		records: map[string]*Record{},
		hashes:  map[string]string{},
		index:   map[string]map[string]struct{}{},
	}
	if policyRaw, err := os.ReadFile(filepath.Join(dir, "policy.json")); err == nil {
		_ = json.Unmarshal(policyRaw, &v.policy)
		if v.policy == nil {
			v.policy = map[string]any{}
		}
	}
	raw, err := os.ReadFile(v.file)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: read vault: %w", err)
	}
	var document vaultFile
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("memory: vault is unreadable: %w", err)
	}
	if document.Schema != StoreSchema || document.VaultID != id {
		return nil, fmt.Errorf("memory: vault schema or id mismatch")
	}
	for _, record := range document.Records {
		if record == nil || record.ID == "" {
			return nil, errors.New("memory: vault contains a record without an id")
		}
		v.records[record.ID] = record
		if record.ContentHash != "" {
			v.hashes[record.ContentHash] = record.ID
		}
		v.indexRecord(record)
	}
	return v, nil
}

// Remember validates, redacts and stores one record. A duplicate content hash
// returns the existing record instead of creating a near-duplicate.
func (v *Vault) Remember(record Record) (Record, bool, error) {
	if err := record.Validate(); err != nil {
		return Record{}, false, err
	}
	record.VaultID = v.ID
	record.Text = Redact(record.Text)
	record.Summary = Redact(record.Summary)
	record.ContentHash = hashRecord(&record)
	if record.RetentionUntil == "" {
		record.RetentionUntil = defaultRetention(record.Type, time.Now().UTC())
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if id, ok := v.hashes[record.ContentHash]; ok {
		if existing, ok := v.records[id]; ok && existing.RetentionState != Deleted {
			return *existing, false, nil
		}
	}
	v.records[record.ID] = &record
	v.hashes[record.ContentHash] = record.ID
	v.indexRecord(&record)
	if err := v.saveLocked(); err != nil {
		delete(v.records, record.ID)
		delete(v.hashes, record.ContentHash)
		v.unindexRecord(&record)
		return Record{}, false, err
	}
	return record, true, nil
}

// Get returns one record, including a suppressed or tombstoned one so the
// audit trail is inspectable.
func (v *Vault) Get(id string) (Record, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	record, ok := v.records[id]
	if !ok {
		return Record{}, false
	}
	return *record, true
}

// Query returns scored, scoped records. It marks returned records as used; a
// recall is evidence that a memory stayed useful.
func (v *Vault) Query(q Query) ([]Hit, error) {
	return v.query(q, true)
}

// Inspect is a read-only query for an operator view; it does not change use
// counters.
func (v *Vault) Inspect(q Query) ([]Hit, error) {
	return v.query(q, false)
}

func (v *Vault) query(q Query, markUse bool) ([]Hit, error) {
	if len(q.Types) > 0 {
		for _, t := range q.Types {
			if !ValidType(t) {
				return nil, fmt.Errorf("memory: unknown memory type %q", t)
			}
		}
	}
	topK := q.TopK
	if topK <= 0 {
		topK = 8
	}
	if topK > 50 {
		topK = 50
	}
	queryTokens := tokenize(q.Text)
	now := time.Now().UTC()

	v.mu.Lock()
	defer v.mu.Unlock()

	candidates := make(map[string]struct{}, len(v.records))
	if len(queryTokens) > 0 {
		for _, token := range queryTokens {
			for id := range v.index[token] {
				candidates[id] = struct{}{}
			}
		}
	} else {
		for id := range v.records {
			candidates[id] = struct{}{}
		}
	}

	allowedTypes := map[Type]struct{}{}
	for _, t := range q.Types {
		allowedTypes[t] = struct{}{}
	}
	var hits []Hit
	for id := range candidates {
		record := v.records[id]
		if record == nil || record.RetentionState != Active {
			continue
		}
		if len(allowedTypes) > 0 {
			if _, ok := allowedTypes[record.Type]; !ok {
				continue
			}
		}
		if !scopeMatches(record.Scope, q.Scope) {
			continue
		}
		lexical := lexicalScore(queryTokens, record)
		if len(queryTokens) > 0 && lexical == 0 {
			continue
		}
		importance := record.Importance
		if importance == 0 {
			importance = 0.5
		}
		recency := recencyScore(record.CreatedAt, now)
		score := 0.55*lexical + 0.25*importance + 0.20*recency
		hits = append(hits, Hit{
			Record: *record,
			Score:  round(score),
			Reasons: map[string]float64{
				"lexical":    round(lexical),
				"importance": round(importance),
				"recency":    round(recency),
			},
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Record.CreatedAt > hits[j].Record.CreatedAt
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	if markUse && len(hits) > 0 {
		stamp := now.Format(time.RFC3339Nano)
		for i := range hits {
			if record := v.records[hits[i].Record.ID]; record != nil {
				record.LastUsedAt = stamp
				record.UseCount++
				record.UpdatedAt = stamp
				hits[i].Record = *record
			}
		}
		_ = v.saveLocked()
	}
	return hits, nil
}

// MarkUsed records that retrieval selected these ids. It is used by the
// optional semantic path, which merges vector hits without going through Query.
func (v *Vault) MarkUsed(ids []string) {
	if len(ids) == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	changed := false
	for _, id := range ids {
		if record := v.records[id]; record != nil {
			record.LastUsedAt = stamp
			record.UseCount++
			record.UpdatedAt = stamp
			changed = true
		}
	}
	if changed {
		_ = v.saveLocked()
	}
}

// Forget suppresses or tombstones records. Suppression is the default; a
// tombstone keeps the id and reason so an audit can still resolve it.
func (v *Vault) Forget(ids []string, mode string, reason string) (int, error) {
	if mode == "" {
		mode = "suppress"
	}
	if mode != "suppress" && mode != "delete" {
		return 0, fmt.Errorf("memory: unknown forget mode %q", mode)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	changed := 0
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		record := v.records[id]
		if record == nil {
			continue
		}
		if mode == "delete" {
			record.RetentionState = Deleted
			record.Tombstone = reason
		} else {
			record.RetentionState = Suppressed
		}
		record.UpdatedAt = stamp
		changed++
	}
	if changed > 0 {
		if err := v.saveLocked(); err != nil {
			return 0, err
		}
	}
	return changed, nil
}

// Prune suppresses records whose retention deadline has passed. It never
// deletes; deletion is a user action.
func (v *Vault) Prune(now time.Time) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	changed := 0
	for _, record := range v.records {
		if record.RetentionState != Active || record.RetentionUntil == "" {
			continue
		}
		until, err := time.Parse(time.RFC3339, record.RetentionUntil)
		if err == nil && !now.Before(until) {
			record.RetentionState = Suppressed
			record.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
			changed++
		}
	}
	if changed > 0 {
		if err := v.saveLocked(); err != nil {
			return 0, err
		}
	}
	return changed, nil
}

// List returns a bounded page of records for the operator UI. Newest first.
func (v *Vault) List(limit int) []Record {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]Record, 0, len(v.records))
	for _, record := range v.records {
		out = append(out, *record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SetPolicy stores the local capture/retention/share policy for this vault.
// It is a local policy document, not authority.
func (v *Vault) SetPolicy(policy map[string]any) error {
	if policy == nil {
		policy = map[string]any{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.policy = policy
	payload, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(filepath.Dir(v.file), "policy.json"), append(payload, '\n'))
}

func (v *Vault) policyCopyLocked() map[string]any {
	out := make(map[string]any, len(v.policy))
	for key, value := range v.policy {
		out[key] = value
	}
	return out
}

// Policy returns a copy of the local policy.
func (v *Vault) Policy() map[string]any {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.policyCopyLocked()
}

// Status reports content-free counts.
func (v *Vault) Status() Status {
	v.mu.RLock()
	defer v.mu.RUnlock()
	status := Status{
		Schema:  "sparknet-memory-status/1",
		VaultID: v.ID,
		Records: len(v.records),
		ByType:  map[string]int{},
		ByState: map[string]int{},
		Policy:  v.policyCopyLocked(),
	}
	for _, record := range v.records {
		status.Bytes += int64(len(record.Text) + len(record.Summary))
		status.ByType[string(record.Type)]++
		status.ByState[string(record.RetentionState)]++
		if status.Oldest == "" || record.CreatedAt < status.Oldest {
			status.Oldest = record.CreatedAt
		}
		if record.CreatedAt > status.Newest {
			status.Newest = record.CreatedAt
		}
	}
	return status
}

// Export returns a portable bundle for one vault.
func (v *Vault) Export() (Bundle, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	bundle := Bundle{
		Schema:  "sparknet-memory-bundle/1",
		VaultID: v.ID,
		Records: make([]Record, 0, len(v.records)),
	}
	for _, record := range v.records {
		bundle.Records = append(bundle.Records, *record)
	}
	sort.Slice(bundle.Records, func(i, j int) bool {
		return bundle.Records[i].CreatedAt < bundle.Records[j].CreatedAt
	})
	return bundle, nil
}

// Bundle is the export/import shape.
type Bundle struct {
	Schema  string   `json:"schema"`
	VaultID string   `json:"vault_id"`
	Records []Record `json:"records"`
}

// Import applies a bundle. Identical records are skipped; differing records
// with the same content hash are reported as conflicts and never overwritten.
func (v *Vault) Import(bundle Bundle, replace bool) (ImportResult, error) {
	if bundle.Schema != "sparknet-memory-bundle/1" {
		return ImportResult{}, fmt.Errorf("memory: unknown bundle schema %q", bundle.Schema)
	}
	result := ImportResult{}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, record := range bundle.Records {
		record.VaultID = v.ID
		if err := record.Validate(); err != nil {
			result.Rejected++
			continue
		}
		record.Text = Redact(record.Text)
		record.Summary = Redact(record.Summary)
		record.ContentHash = hashRecord(&record)
		if existingID, ok := v.hashes[record.ContentHash]; ok {
			if existing, ok := v.records[existingID]; ok {
				if existing.SourceRef == record.SourceRef && existing.Text == record.Text {
					result.Skipped++
					continue
				}
				if !replace {
					result.Conflicts = append(result.Conflicts, record.ID)
					continue
				}
				v.unindexRecord(existing)
				delete(v.records, existingID)
				delete(v.hashes, existing.ContentHash)
				result.Replaced++
			}
		}
		v.records[record.ID] = &record
		v.hashes[record.ContentHash] = record.ID
		v.indexRecord(&record)
		result.Imported++
	}
	if err := v.saveLocked(); err != nil {
		return result, err
	}
	return result, nil
}

// ImportResult describes an import.
type ImportResult struct {
	Imported  int      `json:"imported"`
	Skipped   int      `json:"skipped"`
	Replaced  int      `json:"replaced"`
	Rejected  int      `json:"rejected"`
	Conflicts []string `json:"conflicts,omitempty"`
}

func (v *Vault) indexRecord(record *Record) {
	text := record.Text + " " + record.Summary + " " + record.SourceRef + " " + record.Subtype
	for _, token := range tokenize(text) {
		set := v.index[token]
		if set == nil {
			set = map[string]struct{}{}
			v.index[token] = set
		}
		set[record.ID] = struct{}{}
	}
}

func (v *Vault) unindexRecord(record *Record) {
	text := record.Text + " " + record.Summary + " " + record.SourceRef + " " + record.Subtype
	for _, token := range tokenize(text) {
		if set := v.index[token]; set != nil {
			delete(set, record.ID)
			if len(set) == 0 {
				delete(v.index, token)
			}
		}
	}
}

func (v *Vault) saveLocked() error {
	document := vaultFile{Schema: StoreSchema, VaultID: v.ID, Records: make([]*Record, 0, len(v.records))}
	for _, record := range v.records {
		document.Records = append(document.Records, record)
	}
	sort.Slice(document.Records, func(i, j int) bool {
		return document.Records[i].CreatedAt < document.Records[j].CreatedAt
	})
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(v.file, append(payload, '\n'))
}

func writeAtomic(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".memory-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func scopeMatches(record Scope, want Scope) bool {
	if want.UserID != "" && record.UserID != want.UserID {
		return false
	}
	if want.ProjectID != "" && record.ProjectID != want.ProjectID {
		return false
	}
	if want.ThreadID != "" && record.ThreadID != want.ThreadID {
		return false
	}
	if want.WorkflowID != "" && record.WorkflowID != want.WorkflowID {
		return false
	}
	if want.AgentID != "" && record.AgentID != want.AgentID {
		return false
	}
	return true
}

var stopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "that": {}, "this": {},
	"from": {}, "are": {}, "was": {}, "were": {}, "you": {}, "your": {},
	"have": {}, "has": {}, "had": {}, "not": {}, "but": {}, "what": {},
	"when": {}, "where": {}, "which": {}, "into": {}, "about": {}, "they": {},
}

func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
	})
	seen := map[string]struct{}{}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len(field) < 2 {
			continue
		}
		if _, stop := stopWords[field]; stop {
			continue
		}
		if _, ok := seen[field]; ok {
			continue
		}
		seen[field] = struct{}{}
		out = append(out, field)
		if len(out) >= 64 {
			break
		}
	}
	return out
}

func lexicalScore(queryTokens []string, record *Record) float64 {
	if len(queryTokens) == 0 {
		return 0
	}
	recordTokens := map[string]struct{}{}
	for _, token := range tokenize(record.Text + " " + record.Summary + " " + record.SourceRef + " " + record.Subtype) {
		recordTokens[token] = struct{}{}
	}
	matches := 0
	for _, token := range queryTokens {
		if _, ok := recordTokens[token]; ok {
			matches++
		}
	}
	return float64(matches) / float64(len(queryTokens))
}

func recencyScore(created string, now time.Time) float64 {
	when, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		when, err = time.Parse(time.RFC3339, created)
	}
	if err != nil {
		return 0.5
	}
	days := now.Sub(when).Hours() / 24
	if days < 0 {
		days = 0
	}
	return 1.0 / (1.0 + days/30.0)
}

func round(value float64) float64 {
	return math.Round(value*10000) / 10000
}

// ScopeMatches exposes the same scope predicate Vault.Query uses.
func ScopeMatches(record Scope, want Scope) bool { return scopeMatches(record, want) }
