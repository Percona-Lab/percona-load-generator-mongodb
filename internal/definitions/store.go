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

type Definition struct {
	ID             string `json:"id"`
	Type           Kind   `json:"type"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Content        string `json:"content,omitempty"`
	Format         string `json:"format"`
	SourceFilename string `json:"sourceFilename,omitempty"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

type Input struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Content        string `json:"content"`
	Format         string `json:"format"`
	SourceFilename string `json:"sourceFilename"`
}

func (in *Input) UnmarshalJSON(b []byte) error {
	type alias Input
	var raw struct {
		alias
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
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
	var normalized interface{}
	if err := json.Unmarshal(raw.Content, &normalized); err != nil {
		return fmt.Errorf("content must be a JSON string, object, or array: %w", err)
	}
	canonical, err := marshalJSONNoHTMLEscape(normalized)
	if err != nil {
		return err
	}
	in.Content = string(canonical)
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
			return def, nil
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
