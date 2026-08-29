package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robertpitt/git3/internal/locator"
)

// Format and document-size limits for the remote protocol.
const (
	FormatVersion  = 1
	MaxHead        = 2 << 20
	MaxTransaction = 64 << 20
	MaxLogPage     = 128 << 20
	MaxSnapshot    = 128 << 20
	MaxPackset     = 64 << 20
	MaxRefs        = 100000
)

var hexLower = regexp.MustCompile(`^[0-9a-f]+$`)

// ObjectRef identifies an immutable managed object and its integrity metadata.
type ObjectRef struct {
	Key    string `json:"key"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

// StoragePolicy records the server-side encryption requirements for a repository.
type StoragePolicy struct {
	ServerSideEncryption string  `json:"serverSideEncryption"`
	KMSKeyID             *string `json:"kmsKeyId"`
	BucketKeyEnabled     *bool   `json:"bucketKeyEnabled"`
}

// SnapshotPointer identifies the published ref snapshot.
type SnapshotPointer struct {
	SnapshotID    string    `json:"snapshotId"`
	Generation    uint64    `json:"generation"`
	TransactionID *string   `json:"transactionId"`
	Object        ObjectRef `json:"object"`
}

// PacksetPointer identifies the published packset manifest.
type PacksetPointer struct {
	PacksetID     string    `json:"packsetId"`
	Generation    uint64    `json:"generation"`
	TransactionID *string   `json:"transactionId"`
	Object        ObjectRef `json:"object"`
}

// PagePointer identifies the newest immutable transaction-log page.
type PagePointer struct {
	PageID          string    `json:"pageId"`
	FirstGeneration uint64    `json:"firstGeneration"`
	LastGeneration  uint64    `json:"lastGeneration"`
	Object          ObjectRef `json:"object"`
}

// Log describes the live transaction-log floor, pages, and inline tail.
type Log struct {
	FloorGeneration    uint64       `json:"floorGeneration"`
	FloorTransactionID *string      `json:"floorTransactionId"`
	TipPage            *PagePointer `json:"tipPage"`
	Tail               []Envelope   `json:"tail"`
}

// GCBarrier prevents writers from publishing while garbage collection is active.
type GCBarrier struct {
	PlanID    string    `json:"planId"`
	CreatedAt string    `json:"createdAt"`
	Plan      ObjectRef `json:"plan"`
}

// Head is the single mutable publication point for a repository.
type Head struct {
	FormatVersion     int                        `json:"formatVersion"`
	RequiredFeatures  []string                   `json:"requiredFeatures"`
	RepositoryID      string                     `json:"repositoryId"`
	ObjectFormat      string                     `json:"objectFormat"`
	LogicalGeneration uint64                     `json:"logicalGeneration"`
	TransactionID     *string                    `json:"transactionId"`
	ManifestRevision  uint64                     `json:"manifestRevision"`
	PublicationID     string                     `json:"publicationId"`
	HeadSymref        *string                    `json:"headSymref"`
	StoragePolicy     StoragePolicy              `json:"storagePolicy"`
	RefSnapshot       SnapshotPointer            `json:"refSnapshot"`
	Packset           PacksetPointer             `json:"packset"`
	Log               Log                        `json:"log"`
	GCBarrier         *GCBarrier                 `json:"gcBarrier"`
	Extra             map[string]json.RawMessage `json:"-"`
}

// Update describes one ref transition in a transaction.
type Update struct {
	Ref  string  `json:"ref"`
	Old  *string `json:"old"`
	New  *string `json:"new"`
	Kind string  `json:"kind"`
}

// ObjectData describes a transaction's immutable thin WAL pack.
type ObjectData struct {
	Object            ObjectRef `json:"object"`
	GitPackChecksum   string    `json:"gitPackChecksum"`
	Thin              bool      `json:"thin"`
	BaseGeneration    uint64    `json:"baseGeneration"`
	BaseTransactionID *string   `json:"baseTransactionId"`
}

// ObjectFormatGuess infers the Git hash format from the pack checksum length.
func (d ObjectData) ObjectFormatGuess() string {
	if len(d.GitPackChecksum) == 64 {
		return "sha256"
	}
	return "sha1"
}

// Transaction is an ordered atomic set of ref updates.
type Transaction struct {
	FormatVersion       int         `json:"formatVersion"`
	RepositoryID        string      `json:"repositoryId"`
	ObjectFormat        string      `json:"objectFormat"`
	Generation          uint64      `json:"generation"`
	TransactionID       string      `json:"transactionId"`
	ParentGeneration    uint64      `json:"parentGeneration"`
	ParentTransactionID *string     `json:"parentTransactionId"`
	CreatedAt           string      `json:"createdAt"`
	WriterVersion       string      `json:"writerVersion"`
	Updates             []Update    `json:"updates"`
	ObjectData          *ObjectData `json:"objectData"`
}

// Envelope pairs a transaction with its immutable descriptor object.
type Envelope struct {
	Descriptor  ObjectRef   `json:"descriptor"`
	Transaction Transaction `json:"transaction"`
}

// PagePrevious links a log page to its predecessor.
type PagePrevious struct {
	PageID          string    `json:"pageId"`
	FirstGeneration uint64    `json:"firstGeneration"`
	LastGeneration  uint64    `json:"lastGeneration"`
	Object          ObjectRef `json:"object"`
}

// LogPage stores a bounded segment of transaction history.
type LogPage struct {
	FormatVersion     int           `json:"formatVersion"`
	RepositoryID      string        `json:"repositoryId"`
	PageID            string        `json:"pageId"`
	Previous          *PagePrevious `json:"previous"`
	BaseGeneration    uint64        `json:"baseGeneration"`
	BaseTransactionID *string       `json:"baseTransactionId"`
	FirstGeneration   uint64        `json:"firstGeneration"`
	LastGeneration    uint64        `json:"lastGeneration"`
	Transactions      []Envelope    `json:"transactions"`
}

// PackEntry identifies one published Git pack and index.
type PackEntry struct {
	GitPackChecksum string    `json:"gitPackChecksum"`
	ObjectCount     uint64    `json:"objectCount"`
	Pack            ObjectRef `json:"pack"`
	Index           ObjectRef `json:"index"`
}

// PackLevel groups packs by geometric compaction level.
type PackLevel struct {
	Level uint64      `json:"level"`
	Packs []PackEntry `json:"packs"`
}

// Packset is the complete published base object set for a generation.
type Packset struct {
	FormatVersion int         `json:"formatVersion"`
	RepositoryID  string      `json:"repositoryId"`
	ObjectFormat  string      `json:"objectFormat"`
	PacksetID     string      `json:"packsetId"`
	Generation    uint64      `json:"generation"`
	TransactionID *string     `json:"transactionId"`
	Levels        []PackLevel `json:"levels"`
}

// Snapshot is the canonical materialized ref state at a generation.
type Snapshot struct {
	RepositoryID, ObjectFormat string
	Generation                 uint64
	TransactionID              *string
	Refs                       map[string]string
}

// Cursor records the locally cached remote publication position.
type Cursor struct {
	FormatVersion     int     `json:"formatVersion"`
	RepositoryID      string  `json:"repositoryId"`
	ObjectFormat      string  `json:"objectFormat"`
	LogicalGeneration uint64  `json:"logicalGeneration"`
	TransactionID     *string `json:"transactionId"`
	LastPublicationID string  `json:"lastPublicationId"`
	LastHeadETag      string  `json:"lastHeadEtag"`
	CachedRefsSHA256  string  `json:"cachedRefsSha256"`
}

// GCCandidate identifies an unreachable object eligible for conditional deletion.
type GCCandidate struct {
	Key          string `json:"key"`
	Size         uint64 `json:"size"`
	ETag         string `json:"etag"`
	LastModified string `json:"lastModified"`
	Category     string `json:"category,omitempty"`
}

// GCPlan records a deterministic, resumable garbage-collection candidate set.
type GCPlan struct {
	FormatVersion       int           `json:"formatVersion"`
	PlanID              string        `json:"planId"`
	RepositoryID        string        `json:"repositoryId"`
	SourcePublicationID string        `json:"sourcePublicationId"`
	SourceETag          string        `json:"sourceEtag"`
	Cutoff              string        `json:"cutoff"`
	Candidates          []GCCandidate `json:"candidates"`
}

// Validate checks the GC plan's structure, ordering, and managed-key containment.
func (p *GCPlan) Validate() error {
	if p.FormatVersion != 1 || !validUUID(p.PlanID) || !validUUID(p.RepositoryID) || !validUUID(p.SourcePublicationID) || p.SourceETag == "" || !validTime(p.Cutoff) || len(p.Candidates) > MaxRefs {
		return fmt.Errorf("invalid GC plan header")
	}
	last := ""
	for _, c := range p.Candidates {
		if c.Key <= last || c.ETag == "" || !validTime(c.LastModified) {
			return fmt.Errorf("invalid GC candidate")
		}
		if err := locator.ValidateManagedKey(c.Key); err != nil {
			return err
		}
		last = c.Key
	}
	return nil
}

func ptrEqual(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func validUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// ValidOID reports whether s is a canonical object ID for format.
func ValidOID(s, format string) bool {
	n := 40
	if format == "sha256" {
		n = 64
	} else if format != "sha1" {
		return false
	}
	return len(s) == n && hexLower.MatchString(s)
}
func validateObject(r ObjectRef) error {
	if err := locator.ValidateManagedKey(r.Key); err != nil {
		return err
	}
	if len(r.SHA256) != 64 || !hexLower.MatchString(r.SHA256) {
		return fmt.Errorf("bad SHA-256 for %s", r.Key)
	}
	return nil
}

// Validate checks the object's managed key and SHA-256 digest.
func (r ObjectRef) Validate() error { return validateObject(r) }
func validTime(s string) bool {
	t, e := time.Parse("2006-01-02T15:04:05Z", s)
	return e == nil && t.UTC().Format("2006-01-02T15:04:05Z") == s
}
func validRefBasic(s string) bool {
	if !strings.HasPrefix(s, "refs/") || len(s) <= 5 || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.Contains(s, "//") || strings.Contains(s, "..") || strings.Contains(s, "@{") {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	return true
}

// Validate checks HEAD invariants without fetching referenced objects.
func (h *Head) Validate() error {
	if h.FormatVersion != 1 {
		return fmt.Errorf("unsupported formatVersion %d", h.FormatVersion)
	}
	if h.RequiredFeatures == nil {
		return fmt.Errorf("requiredFeatures must not be null")
	}
	if len(h.RequiredFeatures) > 0 {
		return fmt.Errorf("unsupported required feature %q", h.RequiredFeatures[0])
	}
	if !validUUID(h.RepositoryID) || !validUUID(h.PublicationID) {
		return fmt.Errorf("invalid repository/publication UUID")
	}
	if h.ObjectFormat != "sha1" && h.ObjectFormat != "sha256" {
		return fmt.Errorf("unsupported object format")
	}
	if h.ManifestRevision < 1 {
		return fmt.Errorf("manifest revision must be positive")
	}
	if h.LogicalGeneration == 0 {
		if h.TransactionID != nil || h.HeadSymref != nil || h.RefSnapshot.TransactionID != nil || h.Packset.TransactionID != nil || h.Log.FloorTransactionID != nil {
			return fmt.Errorf("generation-zero transaction fields must be null")
		}
	} else if h.TransactionID == nil || !validUUID(*h.TransactionID) {
		return fmt.Errorf("invalid tip transaction")
	}
	if !validUUID(h.RefSnapshot.SnapshotID) || !validUUID(h.Packset.PacksetID) {
		return fmt.Errorf("invalid manifest UUID")
	}
	if h.RefSnapshot.Generation == 0 && h.RefSnapshot.TransactionID != nil || h.RefSnapshot.Generation > 0 && (h.RefSnapshot.TransactionID == nil || !validUUID(*h.RefSnapshot.TransactionID)) {
		return fmt.Errorf("invalid snapshot transaction")
	}
	if h.Packset.Generation == 0 && h.Packset.TransactionID != nil || h.Packset.Generation > 0 && (h.Packset.TransactionID == nil || !validUUID(*h.Packset.TransactionID)) {
		return fmt.Errorf("invalid packset transaction")
	}
	if h.Log.FloorGeneration == 0 && h.Log.FloorTransactionID != nil || h.Log.FloorGeneration > 0 && (h.Log.FloorTransactionID == nil || !validUUID(*h.Log.FloorTransactionID)) {
		return fmt.Errorf("invalid log floor transaction")
	}
	if err := validateObject(h.RefSnapshot.Object); err != nil {
		return err
	}
	if err := validateObject(h.Packset.Object); err != nil {
		return err
	}
	if h.Packset.Generation != h.Log.FloorGeneration || !ptrEqual(h.Packset.TransactionID, h.Log.FloorTransactionID) {
		return fmt.Errorf("packset/log floor mismatch")
	}
	if h.RefSnapshot.Generation < h.Log.FloorGeneration || h.RefSnapshot.Generation > h.LogicalGeneration {
		return fmt.Errorf("snapshot generation outside live range")
	}
	if h.HeadSymref != nil && !strings.HasPrefix(*h.HeadSymref, "refs/heads/") {
		return fmt.Errorf("invalid HEAD symref")
	}
	if h.Log.TipPage != nil {
		if !validUUID(h.Log.TipPage.PageID) || h.Log.TipPage.FirstGeneration > h.Log.TipPage.LastGeneration || h.Log.TipPage.LastGeneration > h.LogicalGeneration {
			return fmt.Errorf("invalid tip page")
		}
		if err := validateObject(h.Log.TipPage.Object); err != nil {
			return err
		}
	}
	if len(h.Log.Tail) > 32 {
		return fmt.Errorf("log tail exceeds 32 records")
	}
	if h.Log.Tail == nil {
		return fmt.Errorf("log tail must not be null")
	}
	if b, e := json.Marshal(h.Log.Tail); e != nil || len(b) > 1<<20 {
		return fmt.Errorf("log tail exceeds 1 MiB")
	}
	prevGen := h.Log.FloorGeneration
	prevID := h.Log.FloorTransactionID
	if h.Log.TipPage != nil {
		if err := validateObject(h.Log.TipPage.Object); err != nil {
			return err
		}
		prevGen = h.Log.TipPage.LastGeneration /* page linkage verified during traversal */
	}
	for i := range h.Log.Tail {
		tx := &h.Log.Tail[i].Transaction
		if err := tx.Validate(); err != nil {
			return err
		}
		if tx.Generation != prevGen+1 || (!(h.Log.TipPage != nil && i == 0) && !ptrEqual(tx.ParentTransactionID, prevID)) {
			return fmt.Errorf("tail chain gap")
		}
		prevGen = tx.Generation
		id := tx.TransactionID
		prevID = &id
	}
	if h.Log.TipPage == nil && (prevGen != h.LogicalGeneration || !ptrEqual(prevID, h.TransactionID)) {
		return fmt.Errorf("log does not end at HEAD tip")
	}
	if h.Log.TipPage != nil && len(h.Log.Tail) == 0 && h.Log.TipPage.LastGeneration != h.LogicalGeneration {
		return fmt.Errorf("tip page does not reach HEAD")
	}
	if h.Log.TipPage != nil && len(h.Log.Tail) > 0 && !ptrEqual(prevID, h.TransactionID) {
		return fmt.Errorf("tail does not end at HEAD")
	}
	if h.GCBarrier != nil {
		if !validUUID(h.GCBarrier.PlanID) || !validTime(h.GCBarrier.CreatedAt) {
			return fmt.Errorf("invalid GC barrier")
		}
		if err := validateObject(h.GCBarrier.Plan); err != nil {
			return err
		}
	}
	return h.StoragePolicy.Validate()
}

// Validate checks the server-side encryption policy.
func (p StoragePolicy) Validate() error {
	switch p.ServerSideEncryption {
	case "inherit", "AES256":
		if p.KMSKeyID != nil || p.BucketKeyEnabled != nil {
			return fmt.Errorf("KMS fields require aws:kms")
		}
	case "aws:kms":
	default:
		return fmt.Errorf("invalid encryption policy")
	}
	return nil
}

// Validate checks transaction ordering, identity, refs, and object metadata.
func (t *Transaction) Validate() error {
	if t.FormatVersion != 1 || !validUUID(t.RepositoryID) || !validUUID(t.TransactionID) || (t.ObjectFormat != "sha1" && t.ObjectFormat != "sha256") || t.Generation != t.ParentGeneration+1 || !validTime(t.CreatedAt) || len(t.Updates) == 0 || len(t.Updates) > MaxRefs {
		return fmt.Errorf("invalid transaction header")
	}
	if t.Generation == 1 && t.ParentTransactionID != nil {
		return fmt.Errorf("generation one parent must be null")
	}
	if t.Generation > 1 && (t.ParentTransactionID == nil || !validUUID(*t.ParentTransactionID)) {
		return fmt.Errorf("invalid parent transaction")
	}
	last := ""
	for _, u := range t.Updates {
		if !validRefBasic(u.Ref) || u.Ref <= last {
			return fmt.Errorf("invalid or unsorted ref %q", u.Ref)
		}
		last = u.Ref
		if u.Old != nil && !ValidOID(*u.Old, t.ObjectFormat) {
			return fmt.Errorf("invalid old oid")
		}
		if u.New != nil && !ValidOID(*u.New, t.ObjectFormat) {
			return fmt.Errorf("invalid new oid")
		}
		switch u.Kind {
		case "create":
			if u.Old != nil || u.New == nil {
				return fmt.Errorf("invalid create")
			}
		case "delete":
			if u.Old == nil || u.New != nil {
				return fmt.Errorf("invalid delete")
			}
		case "fast-forward", "force":
			if u.Old == nil || u.New == nil {
				return fmt.Errorf("invalid update")
			}
		default:
			return fmt.Errorf("invalid update kind")
		}
	}
	if t.ObjectData != nil {
		if err := validateObject(t.ObjectData.Object); err != nil {
			return err
		}
		if !ValidOID(t.ObjectData.GitPackChecksum, t.ObjectFormat) || !t.ObjectData.Thin {
			return fmt.Errorf("invalid WAL pack")
		}
		if t.ObjectData.BaseGeneration != t.ParentGeneration || !ptrEqual(t.ObjectData.BaseTransactionID, t.ParentTransactionID) {
			return fmt.Errorf("invalid WAL base")
		}
	}
	return nil
}

// Validate checks packset ordering, identity, and referenced object metadata.
func (p *Packset) Validate() error {
	if p.FormatVersion != 1 || !validUUID(p.RepositoryID) || !validUUID(p.PacksetID) || (p.ObjectFormat != "sha1" && p.ObjectFormat != "sha256") {
		return fmt.Errorf("invalid packset header")
	}
	if p.Generation == 0 && p.TransactionID != nil || p.Generation > 0 && (p.TransactionID == nil || !validUUID(*p.TransactionID)) {
		return fmt.Errorf("invalid packset transaction")
	}
	if p.Levels == nil {
		return fmt.Errorf("null packset levels")
	}
	var lastLevel int64 = -1
	for _, l := range p.Levels {
		if int64(l.Level) <= lastLevel {
			return fmt.Errorf("unsorted levels")
		}
		lastLevel = int64(l.Level)
		last := ""
		for _, e := range l.Packs {
			if !ValidOID(e.GitPackChecksum, p.ObjectFormat) || e.GitPackChecksum <= last {
				return fmt.Errorf("invalid pack checksum order")
			}
			last = e.GitPackChecksum
			if err := validateObject(e.Pack); err != nil {
				return err
			}
			if err := validateObject(e.Index); err != nil {
				return err
			}
			base := ".git/git3/packs/pack-" + e.GitPackChecksum
			if e.Pack.Key != base+".pack" || e.Index.Key != base+".idx" {
				return fmt.Errorf("pack key/checksum mismatch")
			}
		}
	}
	return nil
}

// Apply applies transactions to refs after checking every old value.
func Apply(refs map[string]string, txs []Transaction) error {
	for _, tx := range txs {
		for _, u := range tx.Updates {
			old, ok := refs[u.Ref]
			if (u.Old == nil && ok) || (u.Old != nil && (!ok || old != *u.Old)) {
				return fmt.Errorf("old value mismatch for %s", u.Ref)
			}
			if u.New == nil {
				delete(refs, u.Ref)
			} else {
				refs[u.Ref] = *u.New
			}
		}
	}
	return nil
}

// SortedRefs returns ref names in lexical order.
func SortedRefs(refs map[string]string) []string {
	out := make([]string, 0, len(refs))
	for k := range refs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
