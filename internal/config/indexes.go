package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// IndexKey is one field of an index or shard key pattern.
type IndexKey struct {
	Field string
	// Value is int32 1 or -1, or one of the index type strings in indexKeyTypes.
	Value interface{}
}

// IndexKeys is a key pattern in declared order. It decodes from a JSON object
// (fields in the order written) or from an array of single-field objects, and
// encodes back to a JSON object in the same order.
type IndexKeys []IndexKey

var indexKeyTypes = map[string]bool{"hashed": true, "text": true, "2dsphere": true, "2d": true}

var supportedIndexOptions = []string{"keys", "name", "unique", "sparse", "hidden", "expireAfterSeconds", "partialFilterExpression"}

// IndexDefinition is a workload index. Keys keep their declared order; the
// remaining fields are the supported createIndexes options.
type IndexDefinition struct {
	Keys                    IndexKeys       `json:"keys"`
	Name                    string          `json:"name,omitempty"`
	Unique                  bool            `json:"unique,omitempty"`
	Sparse                  bool            `json:"sparse,omitempty"`
	Hidden                  bool            `json:"hidden,omitempty"`
	ExpireAfterSeconds      *int32          `json:"expireAfterSeconds,omitempty"`
	PartialFilterExpression json.RawMessage `json:"partialFilterExpression,omitempty"`
}

type indexDefinitionAlias IndexDefinition

func (d *IndexDefinition) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return fmt.Errorf("index definition must be a JSON object: %w", err)
	}
	var unknown []string
	for k := range fields {
		if !containsString(supportedIndexOptions, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unsupported index option(s) %s (supported: %s)", strings.Join(unknown, ", "), strings.Join(supportedIndexOptions, ", "))
	}
	var a indexDefinitionAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*d = IndexDefinition(a)
	return nil
}

func (k *IndexKeys) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	var out IndexKeys
	switch tok {
	case json.Delim('{'):
		if out, err = decodeKeyObject(dec, out, false); err != nil {
			return err
		}
	case json.Delim('['):
		for dec.More() {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if t != json.Delim('{') {
				return fmt.Errorf("index key array entries must be single-field objects such as {\"field\": 1}")
			}
			if out, err = decodeKeyObject(dec, out, true); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("index keys must be a JSON object or an array of single-field objects")
	}
	*k = out
	return nil
}

// decodeKeyObject reads the fields of an object whose opening brace was
// already consumed, appending them to out in order.
func decodeKeyObject(dec *json.Decoder, out IndexKeys, single bool) (IndexKeys, error) {
	n := 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		field := t.(string)
		var raw interface{}
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		v, err := normalizeIndexKeyValue(raw)
		if err != nil {
			return nil, fmt.Errorf("index key %q: %w", field, err)
		}
		for _, e := range out {
			if e.Field == field {
				return nil, fmt.Errorf("index key %q is declared more than once", field)
			}
		}
		out = append(out, IndexKey{Field: field, Value: v})
		n++
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if single && n != 1 {
		return nil, fmt.Errorf("index key array entries must contain exactly one field, got %d", n)
	}
	return out, nil
}

func (k IndexKeys) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range k {
		if i > 0 {
			buf.WriteByte(',')
		}
		f, err := json.Marshal(e.Field)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(e.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(f)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// normalizeIndexKeyValue accepts 1, -1 (any numeric type) or an index type
// string and returns int32 or the string.
func normalizeIndexKeyValue(v interface{}) (interface{}, error) {
	var f float64
	switch n := v.(type) {
	case string:
		if indexKeyTypes[n] {
			return n, nil
		}
		return nil, fmt.Errorf("unsupported index type %q (supported: 1, -1, \"hashed\", \"text\", \"2dsphere\", \"2d\")", n)
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return nil, fmt.Errorf("invalid direction %q", n.String())
		}
		f = parsed
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return nil, fmt.Errorf("unsupported key value %v (%T); use 1, -1, or an index type string", v, v)
	}
	if f == 1 || f == -1 {
		return int32(f), nil
	}
	return nil, fmt.Errorf("unsupported direction %v; use 1 or -1", v)
}

// BSON returns the key pattern as an ordered document.
func (k IndexKeys) BSON() bson.D {
	d := make(bson.D, 0, len(k))
	for _, e := range k {
		v := e.Value
		if n, err := normalizeIndexKeyValue(v); err == nil {
			v = n
		}
		d = append(d, bson.E{Key: e.Field, Value: v})
	}
	return d
}

// Fields returns the field names in declared order.
func (k IndexKeys) Fields() []string {
	out := make([]string, len(k))
	for i, e := range k {
		out[i] = e.Field
	}
	return out
}

// Signature is a canonical, order-preserving string form of the key pattern.
func (k IndexKeys) Signature() string {
	parts := make([]string, len(k))
	for i, e := range k {
		parts[i] = fmt.Sprintf("%s:%v", e.Field, e.Value)
	}
	return strings.Join(parts, ",")
}

// GeneratedName is the default index name, built the same way as the MongoDB
// driver and shell: field_value pairs joined by underscores.
func (k IndexKeys) GeneratedName() string {
	parts := make([]string, 0, 2*len(k))
	for _, e := range k {
		parts = append(parts, e.Field, fmt.Sprintf("%v", e.Value))
	}
	return strings.Join(parts, "_")
}

// EffectiveName is the declared name, or the generated name when none is set.
func (d IndexDefinition) EffectiveName() string {
	if strings.TrimSpace(d.Name) != "" {
		return d.Name
	}
	return d.Keys.GeneratedName()
}

// PartialFilterDoc parses partialFilterExpression (Extended JSON allowed),
// preserving field order. It returns nil when no filter is set.
func (d IndexDefinition) PartialFilterDoc() (bson.D, error) {
	if len(bytes.TrimSpace(d.PartialFilterExpression)) == 0 {
		return nil, nil
	}
	var doc bson.D
	if err := bson.UnmarshalExtJSON(d.PartialFilterExpression, false, &doc); err != nil {
		return nil, fmt.Errorf("partialFilterExpression must be a JSON object: %w", err)
	}
	if len(doc) == 0 {
		return nil, fmt.Errorf("partialFilterExpression must not be empty")
	}
	return doc, nil
}

func validateIndexKeys(k IndexKeys) error {
	if len(k) == 0 {
		return fmt.Errorf("index keys cannot be empty")
	}
	seen := make(map[string]bool, len(k))
	for i, e := range k {
		if strings.TrimSpace(e.Field) == "" {
			return fmt.Errorf("index key at position %d has an empty field name", i)
		}
		if strings.HasPrefix(e.Field, "$") && e.Field != "$**" {
			return fmt.Errorf("index key %q: field names cannot start with '$'", e.Field)
		}
		if seen[e.Field] {
			return fmt.Errorf("index key %q is declared more than once", e.Field)
		}
		seen[e.Field] = true
		v, err := normalizeIndexKeyValue(e.Value)
		if err != nil {
			return fmt.Errorf("index key %q: %w", e.Field, err)
		}
		k[i].Value = v
	}
	return nil
}

// validateIndexes checks each index and rejects definitions in one collection
// that would map to the same key pattern or the same name.
func validateIndexes(indexes []IndexDefinition) error {
	byKeys := make(map[string]int, len(indexes))
	byName := make(map[string]int, len(indexes))
	for i := range indexes {
		idx := &indexes[i]
		if err := validateIndexKeys(idx.Keys); err != nil {
			return fmt.Errorf("invalid index at position %d: %w", i, err)
		}
		if idx.ExpireAfterSeconds != nil && *idx.ExpireAfterSeconds < 0 {
			return fmt.Errorf("invalid index at position %d: expireAfterSeconds must be >= 0", i)
		}
		if _, err := idx.PartialFilterDoc(); err != nil {
			return fmt.Errorf("invalid index at position %d: %w", i, err)
		}
		sig := idx.Keys.Signature()
		if prev, ok := byKeys[sig]; ok {
			return fmt.Errorf("index at position %d has the same keys {%s} as the index at position %d; declare each key pattern once", i, sig, prev)
		}
		byKeys[sig] = i
		name := idx.EffectiveName()
		if prev, ok := byName[name]; ok {
			return fmt.Errorf("index at position %d has the same name %q as the index at position %d", i, name, prev)
		}
		byName[name] = i
	}
	return nil
}

// validateShardKey allows 1 and "hashed" values, with at most one hashed field.
func validateShardKey(k IndexKeys) error {
	if err := validateIndexKeys(k); err != nil {
		return err
	}
	hashed := 0
	for _, e := range k {
		switch e.Value {
		case int32(1):
		case "hashed":
			hashed++
		default:
			return fmt.Errorf("shard key %q: value must be 1 or \"hashed\", got %v", e.Field, e.Value)
		}
	}
	if hashed > 1 {
		return fmt.Errorf("shard key can contain at most one hashed field")
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
