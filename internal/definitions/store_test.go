package definitions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validCollections = `{"collections":[{"database":"shop","collection":"orders","fields":{"email":{"type":"string"}},"indexes":[{"keys":{"email":1}}]}]}`
const validQueries = `{"queries":[{"name":"find_orders","database":"shop","collection":"orders","operation":"find","filter":{"email":"user@example.com"}}]}`

func TestFileStoreCreateListGetUpdateDelete(t *testing.T) {
	store := newTestStore(t)

	created, err := store.Create(KindQuery, Input{
		Name:           "Orders Queries",
		Description:    "demo queries",
		Content:        validQueries,
		SourceFilename: "orders.json",
	})
	if err != nil {
		t.Fatalf("create definition: %v", err)
	}
	if created.ID == "" || created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("expected metadata to be populated, got %+v", created)
	}
	if !strings.Contains(created.Content, `"queries"`) {
		t.Fatalf("expected canonical content, got %s", created.Content)
	}

	list, err := store.List(KindQuery)
	if err != nil {
		t.Fatalf("list definitions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 definition, got %+v", list)
	}
	if list[0].Content != "" {
		t.Fatalf("list response should omit content")
	}

	got, err := store.Get(KindQuery, created.ID)
	if err != nil {
		t.Fatalf("get definition: %v", err)
	}
	if got.Content == "" || got.Name != "Orders Queries" {
		t.Fatalf("unexpected get payload: %+v", got)
	}

	updated, err := store.Update(KindQuery, created.ID, Input{
		Name:        "Orders Queries v2",
		Description: "edited",
		Content:     validQueries,
	})
	if err != nil {
		t.Fatalf("update definition: %v", err)
	}
	if updated.Name != "Orders Queries v2" || updated.CreatedAt != created.CreatedAt {
		t.Fatalf("unexpected updated metadata: %+v", updated)
	}

	if err := store.Delete(KindQuery, created.ID); err != nil {
		t.Fatalf("delete definition: %v", err)
	}
	if _, err := store.Get(KindQuery, created.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestFileStoreRejectsInvalidAndDuplicateDefinitions(t *testing.T) {
	store := newTestStore(t)

	if _, err := store.Create(KindCollection, Input{Name: "empty", Content: ""}); err == nil {
		t.Fatalf("expected empty content to fail")
	}
	if _, err := store.Create(KindCollection, Input{Name: "bad", Content: `{"collections":[{"databaseName":"shop"}]}`}); err == nil {
		t.Fatalf("expected invalid collection definition to fail")
	}
	if _, err := store.Create(KindQuery, Input{Name: "bad", Content: `{"queries":[{"name":"q","collection":"orders","filter":{}}]}`}); err == nil {
		t.Fatalf("expected invalid query definition to fail")
	}
	if _, err := store.Create(KindQuery, Input{Name: "dupe-names", Content: `{"queries":[{"name":"q","collection":"orders","operation":"find","filter":{}},{"name":"q","collection":"orders","operation":"find","filter":{}}]}`}); err == nil {
		t.Fatalf("expected duplicate query names to fail")
	}

	if _, err := store.Create(KindCollection, Input{Name: "shop", Content: validCollections}); err != nil {
		t.Fatalf("create first collection definition: %v", err)
	}
	if _, err := store.Create(KindCollection, Input{Name: "shop", Content: `{"collections":[{"database":"shop","collection":"customers","fields":{}}]}`}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate name error, got %v", err)
	}
	if _, err := store.Create(KindCollection, Input{Name: "shop-copy", Content: validCollections}); err == nil || !strings.Contains(err.Error(), "identical content") {
		t.Fatalf("expected duplicate content error, got %v", err)
	}
}

func TestFileStorePersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "definitions.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	created, err := store.Create(KindQuery, Input{Name: "persisted", Content: validQueries})
	if err != nil {
		t.Fatalf("create definition: %v", err)
	}

	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, err := reopened.Get(KindQuery, created.ID)
	if err != nil {
		t.Fatalf("get persisted definition: %v", err)
	}
	if got.Name != "persisted" || got.Content == "" {
		t.Fatalf("unexpected persisted definition: %+v", got)
	}
}

func TestValidateContentPreservesDatatypePlaceholders(t *testing.T) {
	raw := `{"queries":[{"name":"typed_filters","database":"shop","collection":"orders","operation":"find","filter":{
		"field_string":"<string>",
		"field_int":"<int>",
		"field_bool":"<boolean>",
		"field_array":"<array>",
		"field_object":"<object>",
		"field_null":"<null>",
		"literal_value":"open"
	}}]}`

	content, err := ValidateContent(KindQuery, []byte(raw))
	if err != nil {
		t.Fatalf("validate query content: %v", err)
	}
	for _, placeholder := range []string{"<string>", "<int>", "<boolean>", "<array>", "<object>", "<null>"} {
		if !strings.Contains(content, `"`+placeholder+`"`) {
			t.Fatalf("expected readable placeholder %q in content, got:\n%s", placeholder, content)
		}
	}
	if strings.Contains(content, `\u003c`) {
		t.Fatalf("expected no HTML-escaped angle brackets in content, got:\n%s", content)
	}
	if !strings.Contains(content, `"literal_value": "open"`) {
		t.Fatalf("expected user-provided filter values to be preserved, got:\n%s", content)
	}
}

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(filepath.Join(t.TempDir(), "definitions.json"))
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	return store
}
