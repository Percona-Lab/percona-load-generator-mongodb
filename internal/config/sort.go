package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Sort specifications keep their declared key order. In pipelines and update
// documents they are held as bson.D; every other object stays a map. A sort
// position is:
//   - the value of any "$sort" key (the $sort stage in every pipeline and
//     sub-pipeline, and the $push modifier in update documents), and
//   - the "sortBy" argument of the operators in sortByOwners.
var sortByOwners = map[string]bool{
	"$setWindowFields": true,
	"$fill":            true,
	"$top":             true,
	"$bottom":          true,
	"$topN":            true,
	"$bottomN":         true,
	"$sortArray":       true,
}

var sortMetaValues = map[string]bool{"textScore": true, "searchScore": true}

func isSortPosition(owner, key string) bool {
	return key == "$sort" || (key == "sortBy" && sortByOwners[owner])
}

// decodeOrderedJSON decodes JSON like encoding/json into interface{} (numbers
// as float64, objects as maps), except that objects at sort positions become
// bson.D in written order. Duplicate keys in a sort object are an error.
func decodeOrderedJSON(raw []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	v, err := decodeOrderedValue(dec, "", false)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("unexpected data after JSON value")
	}
	return v, nil
}

func decodeOrderedValue(dec *json.Decoder, owner string, sortPos bool) (interface{}, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		if sortPos {
			return decodeSortObject(dec, owner)
		}
		m := map[string]interface{}{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string)
			v, err := decodeOrderedValue(dec, k, isSortPosition(owner, k))
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return m, nil
	case json.Delim('['):
		out := []interface{}{}
		for dec.More() {
			v, err := decodeOrderedValue(dec, "", false)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return tok, nil
	}
}

// decodeSortObject reads a sort object whose opening brace was consumed.
// Values are decoded generically; validateSortSpec checks them.
func decodeSortObject(dec *json.Decoder, owner string) (bson.D, error) {
	d := bson.D{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k := kt.(string)
		for _, e := range d {
			if e.Key == k {
				return nil, fmt.Errorf("sort key %q is declared more than once, so its precedence is ambiguous", k)
			}
		}
		v, err := decodeOrderedValue(dec, k, false)
		if err != nil {
			return nil, err
		}
		d = append(d, bson.E{Key: k, Value: v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return d, nil
}

// normalizeSortSpecs validates every sort position in v and returns v with
// each sort specification as a bson.D of normalized values. A multi-key sort
// held in a map has already lost its declared order and is rejected.
func normalizeSortSpecs(v interface{}, owner, path string) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, k := range sortedMapKeys(t) {
			child := t[k]
			p := joinSortPath(path, k)
			var err error
			if isSortPosition(owner, k) {
				child, err = normalizeSortSpec(child, scalarSortAllowed(owner, k), p)
			} else {
				child, err = normalizeSortSpecs(child, k, p)
			}
			if err != nil {
				return nil, err
			}
			t[k] = child
		}
		return t, nil
	case bson.M:
		_, err := normalizeSortSpecs(map[string]interface{}(t), owner, path)
		return t, err
	case bson.D:
		for i, e := range t {
			p := joinSortPath(path, e.Key)
			var err error
			if isSortPosition(owner, e.Key) {
				t[i].Value, err = normalizeSortSpec(e.Value, scalarSortAllowed(owner, e.Key), p)
			} else {
				t[i].Value, err = normalizeSortSpecs(e.Value, e.Key, p)
			}
			if err != nil {
				return nil, err
			}
		}
		return t, nil
	case []interface{}:
		for i := range t {
			nv, err := normalizeSortSpecs(t[i], "", fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	default:
		return v, nil
	}
}

// scalarSortAllowed reports whether a sort position also accepts a bare 1 or
// -1: the $push $sort modifier (a "$sort" key below a field) and $sortArray
// sortBy. A $sort stage sits directly in a stage object, whose owner is "".
func scalarSortAllowed(owner, key string) bool {
	return (key == "$sort" && owner != "") || (key == "sortBy" && owner == "$sortArray")
}

// normalizeSortSpec validates one sort position.
func normalizeSortSpec(v interface{}, allowScalar bool, path string) (interface{}, error) {
	switch t := v.(type) {
	case bson.D:
		return validateSortSpec(t, path)
	case map[string]interface{}:
		if len(t) > 1 {
			return nil, fmt.Errorf("%s: sort specification with %d keys is an unordered map, so its key precedence is ambiguous; define it in JSON with the keys in the intended order", path, len(t))
		}
		d := make(bson.D, 0, len(t))
		for k, x := range t {
			d = append(d, bson.E{Key: k, Value: x})
		}
		return validateSortSpec(d, path)
	case bson.M:
		return normalizeSortSpec(map[string]interface{}(t), allowScalar, path)
	default:
		if dir, ok := sortDirection(v); ok && allowScalar {
			return dir, nil
		}
		return nil, fmt.Errorf("%s: sort specification must be an object such as {\"field\": 1}, got %v", path, v)
	}
}

func validateSortSpec(d bson.D, path string) (bson.D, error) {
	if len(d) == 0 {
		return nil, fmt.Errorf("%s: sort specification must have at least one key", path)
	}
	out := make(bson.D, 0, len(d))
	seen := make(map[string]bool, len(d))
	for _, e := range d {
		if strings.TrimSpace(e.Key) == "" {
			return nil, fmt.Errorf("%s: sort key names cannot be empty", path)
		}
		if strings.HasPrefix(e.Key, "$") {
			return nil, fmt.Errorf("%s: sort key %q cannot start with '$'", path, e.Key)
		}
		if seen[e.Key] {
			return nil, fmt.Errorf("%s: sort key %q is declared more than once, so its precedence is ambiguous", path, e.Key)
		}
		seen[e.Key] = true
		val, err := normalizeSortValue(e.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: sort key %q: %w", path, e.Key, err)
		}
		out = append(out, bson.E{Key: e.Key, Value: val})
	}
	return out, nil
}

// normalizeSortValue accepts 1 or -1 (any numeric type, returned as int32) or
// {"$meta": "textScore"|"searchScore"} (returned as bson.D).
func normalizeSortValue(v interface{}) (interface{}, error) {
	if dir, ok := sortDirection(v); ok {
		return dir, nil
	}
	var meta interface{}
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 1 {
			meta = t["$meta"]
		}
	case bson.D:
		if len(t) == 1 && t[0].Key == "$meta" {
			meta = t[0].Value
		}
	}
	if s, ok := meta.(string); ok && sortMetaValues[s] {
		return bson.D{{Key: "$meta", Value: s}}, nil
	}
	return nil, fmt.Errorf("unsupported sort value %v; use 1, -1, or {\"$meta\": \"textScore\"|\"searchScore\"}", v)
}

func sortDirection(v interface{}) (int32, bool) {
	var f float64
	switch n := v.(type) {
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
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if f == 1 || f == -1 {
		return int32(f), true
	}
	return 0, false
}

func joinSortPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func sortedMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// orderedForJSON returns a copy of v in which bson.D values encode as JSON
// objects in their order instead of as Key/Value arrays.
func orderedForJSON(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, x := range t {
			out[k] = orderedForJSON(x)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, x := range t {
			out[i] = orderedForJSON(x)
		}
		return out
	case bson.D:
		out := make(orderedJSONObject, len(t))
		for i, e := range t {
			out[i] = bson.E{Key: e.Key, Value: orderedForJSON(e.Value)}
		}
		return out
	default:
		return v
	}
}

type orderedJSONObject []bson.E

func (o orderedJSONObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := marshalJSONNoEscape(e.Key)
		if err != nil {
			return nil, err
		}
		v, err := marshalJSONNoEscape(e.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshalJSONNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// HasMultiKeySortSpec reports whether v holds a sort specification with more
// than one key at any sort position.
func HasMultiKeySortSpec(v interface{}) bool {
	return len(MultiKeySortSpecs(v, "")) > 0
}

// SortSpecLocation is a sort specification with more than one key.
type SortSpecLocation struct {
	// Path uses the same notation as sort validation errors, for example
	// "pipeline[1].$facet.byDay[0].$sort".
	Path string `json:"path"`
	// Keys are in held order: declared order for bson.D, alphabetical for a
	// map (whose order is already lost).
	Keys []string `json:"keys"`
	// Spec is the specification as compact JSON in held order, for example
	// `{"b":-1,"a":1}`. Numeric directions render the same whether or not
	// the queries were normalized.
	Spec string `json:"spec"`
}

// MultiKeySortSpecs lists every sort position in v that holds more than one
// key, in document order (map keys alphabetically), with paths below path.
func MultiKeySortSpecs(v interface{}, path string) []SortSpecLocation {
	var out []SortSpecLocation
	collectMultiKeySorts(v, "", path, &out)
	return out
}

func collectMultiKeySorts(v interface{}, owner, path string, out *[]SortSpecLocation) {
	visit := func(k string, x interface{}) {
		p := joinSortPath(path, k)
		if isSortPosition(owner, k) {
			var keys []string
			switch s := x.(type) {
			case bson.D:
				for _, e := range s {
					keys = append(keys, e.Key)
				}
			case map[string]interface{}:
				keys = sortedMapKeys(s)
			case bson.M:
				keys = sortedMapKeys(map[string]interface{}(s))
			}
			if len(keys) > 1 {
				spec, _ := marshalJSONNoEscape(orderedForJSON(x))
				*out = append(*out, SortSpecLocation{Path: p, Keys: keys, Spec: string(spec)})
			}
		}
		collectMultiKeySorts(x, k, p, out)
	}
	switch t := v.(type) {
	case map[string]interface{}:
		for _, k := range sortedMapKeys(t) {
			visit(k, t[k])
		}
	case bson.M:
		collectMultiKeySorts(map[string]interface{}(t), owner, path, out)
	case bson.D:
		for _, e := range t {
			visit(e.Key, e.Value)
		}
	case []interface{}:
		for i, x := range t {
			collectMultiKeySorts(x, "", fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}
