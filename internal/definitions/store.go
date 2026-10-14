package definitions

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

type Kind string

const (
	KindQuery      Kind = "query"
	KindCollection Kind = "collection"
)

// ContentVersionOrderedSorts marks content canonicalized with sort
// specifications in declared key order. Content without it was canonicalized
// with every object's keys sorted alphabetically.
const ContentVersionOrderedSorts = 2

type Definition struct {
	ID             string `json:"id"`
	Type           Kind   `json:"type"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Content        string `json:"content,omitempty"`
	Format         string `json:"format"`
	SourceFilename string `json:"sourceFilename,omitempty"`
	ContentVersion int    `json:"contentVersion,omitempty"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	// SortOrderReview is computed by Get and List; it is never persisted.
	SortOrderReview *SortOrderReview `json:"sortOrderReview,omitempty"`
}

// SortOrderReviewRequired is the SortOrderReview status of a legacy query
// definition whose multi-key sort precedence must be confirmed.
const SortOrderReviewRequired = "review_required"

// SortOrderReviewErrorCode is the API error code for SortOrderReviewError.
const SortOrderReviewErrorCode = "sort_order_review_required"

// SortOrderIssue is one multi-key sort whose precedence must be confirmed.
type SortOrderIssue struct {
	QueryIndex int    `json:"queryIndex"`
	QueryName  string `json:"queryName"`
	// Path is relative to the definition content, for example
	// "queries[0].pipeline[1].$sort".
	Path       string   `json:"path"`
	StoredKeys []string `json:"storedKeys"`
	// SortSpec is the ordered specification as compact JSON, for example
	// `{"a":1,"b":-1}`. A confirmation must repeat it exactly.
	SortSpec string `json:"sortSpec"`
}

// ReviewedSort confirms the key order of one multi-key sort: Path and
// SortSpec as reported in SortOrderIssue for the content being saved.
type ReviewedSort struct {
	Path     string `json:"path"`
	SortSpec string `json:"sortSpec"`
}

// ErrSourceDefinitionChanged reports that the definition a copy was made from
// was changed or deleted after the copy was loaded.
var ErrSourceDefinitionChanged = errors.New("source definition changed")

type SortOrderReview struct {
	Status  string           `json:"status"`
	Message string           `json:"message"`
	Issues  []SortOrderIssue `json:"issues"`
}

// SortOrderReviewError blocks running or saving a legacy definition until
// every multi-key sort is explicitly reviewed.
type SortOrderReviewError struct {
	Message string
	Review  SortOrderReview
}

func (e *SortOrderReviewError) Error() string { return e.Message }

func multiKeySortIssues(queries []config.QueryDefinition) []SortOrderIssue {
	var issues []SortOrderIssue
	for i, q := range queries {
		base := fmt.Sprintf("queries[%d]", i)
		locs := append(config.MultiKeySortSpecs(q.Pipeline, base+".pipeline"), config.MultiKeySortSpecs(q.Update, base+".update")...)
		for _, loc := range locs {
			issues = append(issues, SortOrderIssue{QueryIndex: i, QueryName: q.Name, Path: loc.Path, StoredKeys: loc.Keys, SortSpec: loc.Spec})
		}
	}
	return issues
}

func needsSortOrderReview(d Definition) bool {
	return d.Type == KindQuery && d.ContentVersion < ContentVersionOrderedSorts
}

func describeIssues(issues []SortOrderIssue) string {
	parts := make([]string, len(issues))
	for i, is := range issues {
		parts[i] = fmt.Sprintf("query %d (%s) at %s, stored as [%s]", is.QueryIndex+1, is.QueryName, is.Path, strings.Join(is.StoredKeys, ", "))
	}
	return strings.Join(parts, "; ")
}

func newSortOrderReview(d Definition, issues []SortOrderIssue) SortOrderReview {
	return SortOrderReview{
		Status: SortOrderReviewRequired,
		Message: fmt.Sprintf("query definition %q was saved by an earlier version that may have stored multi-key sort keys alphabetically, so the intended sort precedence is unknown: %s. Review each sort in the editor, confirm or correct the key order, and save the definition again",
			d.Name, describeIssues(issues)),
		Issues: issues,
	}
}

// ReviewSortOrder returns the sort-order review a legacy query definition
// needs, or nil when its sort precedence is known.
func (d Definition) ReviewSortOrder(queries []config.QueryDefinition) *SortOrderReview {
	if !needsSortOrderReview(d) {
		return nil
	}
	issues := multiKeySortIssues(queries)
	if len(issues) == 0 {
		return nil
	}
	review := newSortOrderReview(d, issues)
	return &review
}

// CheckSortOrder reports a query definition saved before sort key order was
// kept: its multi-key sort specifications were stored alphabetically, so the
// declared precedence is unknown. The error is a *SortOrderReviewError.
func (d Definition) CheckSortOrder(queries []config.QueryDefinition) error {
	if review := d.ReviewSortOrder(queries); review != nil {
		return &SortOrderReviewError{Message: review.Message, Review: *review}
	}
	return nil
}

func (d Definition) withSortOrderReview() Definition {
	d.SortOrderReview = nil
	if !needsSortOrderReview(d) {
		return d
	}
	parsed, err := config.ParseQueriesBytes([]byte(d.Content))
	if err != nil {
		return d
	}
	d.SortOrderReview = d.ReviewSortOrder(parsed.Queries)
	return d
}

// checkReviewedSorts requires every multi-key sort of content, which is saved
// over or copied from source, to be confirmed in reviewed with its exact
// ordered specification when source needs review. A confirmation whose
// specification differs from the submitted one is stale and does not count.
func checkReviewedSorts(source Definition, content string, reviewed []ReviewedSort, action string) error {
	if source.withSortOrderReview().SortOrderReview == nil {
		return nil
	}
	parsed, err := config.ParseQueriesBytes([]byte(content))
	if err != nil {
		return fmt.Errorf("invalid query definition JSON: %w", err)
	}
	issues := multiKeySortIssues(parsed.Queries)
	want := make(map[string]string, len(issues))
	for _, is := range issues {
		want[is.Path] = is.SortSpec
	}
	confirmed := make(map[string]bool, len(reviewed))
	var stale []string
	for _, rs := range reviewed {
		spec, ok := want[rs.Path]
		if !ok {
			return fmt.Errorf("reviewed sort path %q is not a multi-key sort in the submitted content; reload the definition and review it again", rs.Path)
		}
		if rs.SortSpec == spec {
			confirmed[rs.Path] = true
		} else {
			stale = append(stale, fmt.Sprintf("%s was confirmed as %s but is %s", rs.Path, rs.SortSpec, spec))
		}
	}
	var missing []SortOrderIssue
	for _, is := range issues {
		if !confirmed[is.Path] {
			missing = append(missing, is)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	msg := fmt.Sprintf("query definition %q needs its sort order reviewed before %s; saving does not confirm the order by itself. Confirm or correct each multi-key sort: %s",
		source.Name, action, describeIssues(missing))
	if len(stale) > 0 {
		msg += fmt.Sprintf(". These confirmations are stale because the sort changed after it was confirmed: %s", strings.Join(stale, "; "))
	}
	return &SortOrderReviewError{Message: msg, Review: newSortOrderReview(source, issues)}
}

// CheckCopySource reports whether source is still the version a copy was
// loaded from (in.SourceUpdatedAt and in.SourceContentVersion).
func CheckCopySource(source Definition, in Input) error {
	if strings.TrimSpace(in.SourceUpdatedAt) == "" {
		return fmt.Errorf("sourceUpdatedAt is required with sourceDefinitionId: send the updatedAt of the definition that was copied")
	}
	if source.UpdatedAt != in.SourceUpdatedAt || source.ContentVersion != in.SourceContentVersion {
		return fmt.Errorf("%w: %s definition %q is now version %s (content version %d), but the copy was loaded from %s (content version %d); reload it and copy it again",
			ErrSourceDefinitionChanged, source.Type, source.Name, source.UpdatedAt, source.ContentVersion, in.SourceUpdatedAt, in.SourceContentVersion)
	}
	return nil
}

// checkCopySourceLocked validates the source a new definition was copied
// from, if the client declared one. Only that one definition is read.
func (s *FileStore) checkCopySourceLocked(kind Kind, in Input, content string) error {
	id := strings.TrimSpace(in.SourceDefinitionID)
	if id == "" {
		if strings.TrimSpace(in.SourceUpdatedAt) != "" || in.SourceContentVersion != 0 {
			return fmt.Errorf("sourceUpdatedAt and sourceContentVersion require sourceDefinitionId")
		}
		return nil
	}
	for _, d := range s.data.Definitions {
		if d.Type == kind && d.ID == id {
			if err := CheckCopySource(d, in); err != nil {
				return err
			}
			return checkReviewedSorts(d, content, in.ReviewedSorts, "a copy of it can be saved")
		}
	}
	return fmt.Errorf("%w: source %s definition %q no longer exists; reload the library and copy it again", ErrSourceDefinitionChanged, kind, id)
}

type Input struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Content        string `json:"content"`
	Format         string `json:"format"`
	SourceFilename string `json:"sourceFilename"`
	// ReviewedSorts confirms the multi-key sorts of Content. Required to
	// update, or to save a copy of, a definition that needs sort-order
	// review; ignored otherwise.
	ReviewedSorts []ReviewedSort `json:"reviewedSorts,omitempty"`
	// SourceDefinitionID, SourceUpdatedAt, and SourceContentVersion identify
	// the definition and version a new definition was copied from (Save As
	// New). Create validates them and applies the source's review. The origin
	// of content without them is unknown and is treated as new.
	SourceDefinitionID   string `json:"sourceDefinitionId,omitempty"`
	SourceUpdatedAt      string `json:"sourceUpdatedAt,omitempty"`
	SourceContentVersion int    `json:"sourceContentVersion,omitempty"`
}

func (in *Input) UnmarshalJSON(b []byte) error {
	type alias Input
	var raw struct {
		alias
		Content           json.RawMessage `json:"content"`
		ReviewedSortPaths json.RawMessage `json:"reviewedSortPaths"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw.ReviewedSortPaths) > 0 && string(raw.ReviewedSortPaths) != "null" {
		return fmt.Errorf("reviewedSortPaths is no longer accepted because it does not bind a confirmation to a key order; send reviewedSorts with the path and sortSpec of each sortOrderReview issue")
	}
	*in = Input(raw.alias)
	if len(raw.Content) == 0 || string(raw.Content) == "null" {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw.Content, &asString); err == nil {
		in.Content = asString
		return nil
	}
	// Keep the raw text: decoding into interface{} would reorder object keys.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw.Content); err != nil {
		return fmt.Errorf("content must be a JSON string, object, or array: %w", err)
	}
	if c := compact.Bytes(); len(c) == 0 || (c[0] != '{' && c[0] != '[') {
		return fmt.Errorf("content must be a JSON string, object, or array")
	}
	in.Content = compact.String()
	return nil
}

type FileStore struct {
	mu   sync.Mutex
	path string
	data persistedData
}

type persistedData struct {
	Definitions []Definition `json:"definitions"`
}

func DefaultStorePath() string {
	if override := strings.TrimSpace(os.Getenv("PLGM_DEFINITIONS_STORE")); override != "" {
		return override
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = "."
	}
	return filepath.Join(base, "plgm", "definitions.json")
}

func NewFileStore(path string) (*FileStore, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultStorePath()
	}
	s := &FileStore{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileStore) List(kind Kind) ([]Definition, error) {
	if err := validateKind(kind); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Definition
	for _, def := range s.data.Definitions {
		if def.Type == kind {
			def = def.withSortOrderReview()
			def.Content = ""
			out = append(out, def)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

func (s *FileStore) Get(kind Kind, id string) (Definition, error) {
	if err := validateKind(kind); err != nil {
		return Definition{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, def := range s.data.Definitions {
		if def.Type == kind && def.ID == id {
			return def.withSortOrderReview(), nil
		}
	}
	return Definition{}, os.ErrNotExist
}

func (s *FileStore) Create(kind Kind, in Input) (Definition, error) {
	if err := validateKind(kind); err != nil {
		return Definition{}, err
	}
	def, err := buildDefinition(kind, in)
	if err != nil {
		return Definition{}, err
	}
	def.ID = newID()
	now := time.Now().UTC().Format(time.RFC3339)
	def.CreatedAt = now
	def.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkCopySourceLocked(kind, in, def.Content); err != nil {
		return Definition{}, err
	}
	if err := s.rejectDuplicateLocked(def, ""); err != nil {
		return Definition{}, err
	}
	s.data.Definitions = append(s.data.Definitions, def)
	if err := s.saveLocked(); err != nil {
		return Definition{}, err
	}
	return def, nil
}

func (s *FileStore) Update(kind Kind, id string, in Input) (Definition, error) {
	if err := validateKind(kind); err != nil {
		return Definition{}, err
	}
	next, err := buildDefinition(kind, in)
	if err != nil {
		return Definition{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i, def := range s.data.Definitions {
		if def.Type == kind && def.ID == id {
			if err := checkReviewedSorts(def, next.Content, in.ReviewedSorts, "it can be saved"); err != nil {
				return Definition{}, err
			}
			next.ID = def.ID
			next.CreatedAt = def.CreatedAt
			next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := s.rejectDuplicateLocked(next, id); err != nil {
				return Definition{}, err
			}
			s.data.Definitions[i] = next
			if err := s.saveLocked(); err != nil {
				return Definition{}, err
			}
			return next, nil
		}
	}
	return Definition{}, os.ErrNotExist
}

func (s *FileStore) Delete(kind Kind, id string) error {
	if err := validateKind(kind); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, def := range s.data.Definitions {
		if def.Type == kind && def.ID == id {
			s.data.Definitions = append(s.data.Definitions[:i], s.data.Definitions[i+1:]...)
			return s.saveLocked()
		}
	}
	return os.ErrNotExist
}

func (s *FileStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = persistedData{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read definitions store: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		s.data = persistedData{}
		return nil
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return fmt.Errorf("parse definitions store: %w", err)
	}
	for i := range s.data.Definitions {
		s.data.Definitions[i].SortOrderReview = nil
	}
	return nil
}

func (s *FileStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create definitions store directory: %w", err)
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode definitions store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write definitions store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace definitions store: %w", err)
	}
	return nil
}

func (s *FileStore) rejectDuplicateLocked(candidate Definition, allowID string) error {
	candidateName := strings.ToLower(strings.TrimSpace(candidate.Name))
	candidateHash := contentHash(candidate.Content)
	for _, existing := range s.data.Definitions {
		if existing.Type != candidate.Type || existing.ID == allowID {
			continue
		}
		if strings.ToLower(strings.TrimSpace(existing.Name)) == candidateName {
			return fmt.Errorf("%s definition named %q already exists", candidate.Type, candidate.Name)
		}
		if contentHash(existing.Content) == candidateHash {
			return fmt.Errorf("%s definition with identical content already exists as %q", candidate.Type, existing.Name)
		}
	}
	return nil
}

func buildDefinition(kind Kind, in Input) (Definition, error) {
	if err := validateKind(kind); err != nil {
		return Definition{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.TrimSpace(in.SourceFilename)
	}
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if name == "" {
		return Definition{}, fmt.Errorf("definition name is required")
	}
	content, err := ValidateContent(kind, []byte(in.Content))
	if err != nil {
		return Definition{}, err
	}
	format := strings.ToLower(strings.TrimSpace(in.Format))
	if format == "" {
		format = "json"
	}
	if format != "json" {
		return Definition{}, fmt.Errorf("unsupported definition format %q: only json is currently supported", format)
	}
	return Definition{
		Type:           kind,
		Name:           name,
		Description:    strings.TrimSpace(in.Description),
		Content:        content,
		Format:         format,
		SourceFilename: strings.TrimSpace(in.SourceFilename),
		ContentVersion: ContentVersionOrderedSorts,
	}, nil
}

func ValidateContent(kind Kind, raw []byte) (string, error) {
	if err := validateKind(kind); err != nil {
		return "", err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", fmt.Errorf("%s definition content is empty", kind)
	}
	switch kind {
	case KindCollection:
		parsed, err := config.ParseCollectionsBytes(raw)
		if err != nil {
			return "", fmt.Errorf("invalid collection definition JSON: %w", err)
		}
		if len(parsed.Collections) == 0 {
			return "", fmt.Errorf("collection definition must include at least one collection")
		}
		if err := config.ValidateCollectionDefinitions(parsed.Collections); err != nil {
			return "", err
		}
		return marshalCanonical(parsed)
	case KindQuery:
		parsed, err := config.ParseQueriesBytes(raw)
		if err != nil {
			return "", fmt.Errorf("invalid query definition JSON: %w", err)
		}
		if len(parsed.Queries) == 0 {
			return "", fmt.Errorf("query definition must include at least one query")
		}
		if err := config.NormalizeAndValidateQueries(parsed.Queries); err != nil {
			return "", err
		}
		if err := rejectDuplicateQueryNames(parsed.Queries); err != nil {
			return "", err
		}
		return marshalCanonical(parsed)
	default:
		return "", fmt.Errorf("unsupported definition type %q", kind)
	}
}

// MarshalCanonicalJSON pretty-prints validated definition payloads for display and storage.
// Datatype placeholders such as "<string>" are kept readable instead of HTML-escaped as \u003c.
func MarshalCanonicalJSON(v interface{}) (string, error) {
	b, err := marshalJSONIndentNoHTMLEscape(v)
	if err != nil {
		return "", fmt.Errorf("canonicalize definition JSON: %w", err)
	}
	return string(b), nil
}

func marshalCanonical(v interface{}) (string, error) {
	return MarshalCanonicalJSON(v)
}

func marshalJSONNoHTMLEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func marshalJSONIndentNoHTMLEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func rejectDuplicateQueryNames(queries []config.QueryDefinition) error {
	seen := map[string]int{}
	for i, q := range queries {
		name := strings.ToLower(strings.TrimSpace(q.Name))
		if name == "" {
			continue
		}
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("duplicate query name %q at index %d (already declared at index %d)", q.Name, i, prev)
		}
		seen[name] = i
	}
	return nil
}

func validateKind(kind Kind) error {
	switch kind {
	case KindQuery, KindCollection:
		return nil
	default:
		return fmt.Errorf("unsupported definition type %q", kind)
	}
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(content)))
	return hex.EncodeToString(sum[:])
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
