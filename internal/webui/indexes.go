package webui

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// listNamespaceIndexesFn reads deployed indexes once per namespace. Tests
// replace it. An empty URI does not dial; the inventory is unknown.
var listNamespaceIndexesFn = listNamespaceIndexes

func listNamespaceIndexes(ctx context.Context, cfg *config.AppConfig, namespaces []string) stats.IndexInventoryView {
	if len(namespaces) == 0 {
		view := stats.CollectIndexInventory(ctx, nil, nil)
		return view
	}
	if cfg == nil || strings.TrimSpace(cfg.URI) == "" {
		return stats.UnknownIndexInventory(namespaces, "connection_not_configured")
	}
	conns := map[string]*mongo.Client{}
	defer func() {
		for _, c := range conns {
			_ = c.Disconnect(context.Background())
		}
	}()
	return stats.CollectIndexInventory(ctx, namespaces, func(ctx context.Context, namespace string) stats.NamespaceIndexInventory {
		dbName, coll, ok := stats.SplitNamespace(namespace)
		if !ok {
			return stats.NamespaceIndexInventory{Status: "unknown", Reason: "invalid_namespace"}
		}
		client := conns[dbName]
		if client == nil {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			conn, err := connectFn(cctx, cfg, dbName)
			cancel()
			if err != nil || conn == nil || conn.Client == nil {
				reason := "connect_failed"
				if err != nil {
					reason = indexListReason(err)
				}
				return stats.NamespaceIndexInventory{Status: "unknown", Reason: reason}
			}
			client = conn.Client
			conns[dbName] = client
		}
		start := time.Now()
		inv := listCollectionIndexes(ctx, client, dbName, coll, namespace)
		inv.ElapsedMS = int(time.Since(start) / time.Millisecond)
		return inv
	})
}

func listCollectionIndexes(ctx context.Context, client *mongo.Client, database, collection, namespace string) stats.NamespaceIndexInventory {
	if client == nil {
		return stats.NamespaceIndexInventory{Namespace: namespace, Status: "unknown", Reason: "connect_failed"}
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cur, err := client.Database(database).Collection(collection).Indexes().List(lctx)
	if err != nil {
		return stats.NamespaceIndexInventory{Namespace: namespace, Status: "unknown", Reason: indexListReason(err)}
	}
	defer cur.Close(lctx)
	var raws []bson.Raw
	if err := cur.All(lctx, &raws); err != nil {
		return stats.NamespaceIndexInventory{Namespace: namespace, Status: "unknown", Reason: indexListReason(err)}
	}
	indexes := make([]stats.DeployedIndex, 0, len(raws))
	for _, raw := range raws {
		idx, err := decodeListedIndex(raw)
		if err != nil {
			return stats.NamespaceIndexInventory{Namespace: namespace, Status: "unknown", Reason: "index_decode_failed"}
		}
		indexes = append(indexes, idx)
	}
	return stats.NamespaceIndexInventory{Namespace: namespace, Status: "fetched", Source: "listIndexes", Indexes: indexes}
}

func indexListReason(err error) string {
	var cmd mongo.CommandError
	if errors.As(err, &cmd) && (cmd.Code == 13 || strings.EqualFold(cmd.Name, "Unauthorized")) {
		return "permission_denied"
	}
	return stats.ClassifyIndexListFailure(err)
}

func decodeListedIndex(raw bson.Raw) (stats.DeployedIndex, error) {
	var doc struct {
		Name      string   `bson:"name"`
		Key       bson.Raw `bson:"key"`
		Unique    *bool    `bson:"unique"`
		Sparse    *bool    `bson:"sparse"`
		Hidden    *bool    `bson:"hidden"`
		Partial   bson.Raw `bson:"partialFilterExpression"`
		Collation bson.Raw `bson:"collation"`
		Multikey  *bool    `bson:"multikey"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return stats.DeployedIndex{}, err
	}
	keys, err := decodeIndexKeys(doc.Key)
	if err != nil {
		return stats.DeployedIndex{}, err
	}
	idx := stats.DeployedIndex{
		Name:     doc.Name,
		Keys:     keys,
		Type:     deployedIndexType(keys),
		Multikey: doc.Multikey,
	}
	if doc.Unique != nil {
		idx.Unique = *doc.Unique
	}
	if doc.Sparse != nil {
		idx.Sparse = *doc.Sparse
	}
	if doc.Hidden != nil {
		idx.Hidden = *doc.Hidden
	}
	if len(doc.Partial) > 0 {
		idx.Partial = true
		preds, summary := decodePartialFilter(doc.Partial)
		idx.PartialPredicates = preds
		idx.PartialSummary = summary
	}
	if len(doc.Collation) > 0 {
		var coll struct {
			Locale string `bson:"locale"`
		}
		if err := bson.Unmarshal(doc.Collation, &coll); err == nil {
			idx.Collation = coll.Locale
		}
	}
	return idx, nil
}

func decodeIndexKeys(raw bson.Raw) ([]stats.IndexKeySpec, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	els, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	keys := make([]stats.IndexKeySpec, 0, len(els))
	for _, el := range els {
		keys = append(keys, keyFromRaw(el.Key(), el.Value()))
	}
	return keys, nil
}

func keyFromRaw(field string, v bson.RawValue) stats.IndexKeySpec {
	switch v.Type {
	case bson.TypeInt32:
		return dirKey(field, int(v.Int32()))
	case bson.TypeInt64:
		return dirKey(field, int(v.Int64()))
	case bson.TypeDouble:
		return dirKey(field, int(v.Double()))
	case bson.TypeString:
		return stats.IndexKeySpec{Field: field, Kind: v.StringValue()}
	default:
		return stats.IndexKeySpec{Field: field, Kind: "unknown"}
	}
}

func dirKey(field string, dir int) stats.IndexKeySpec {
	kind := "unknown"
	switch dir {
	case 1:
		kind = "asc"
	case -1:
		kind = "desc"
	}
	return stats.IndexKeySpec{Field: field, Direction: dir, Kind: kind}
}

func deployedIndexType(keys []stats.IndexKeySpec) string {
	special := make([]string, 0)
	for _, k := range keys {
		if k.Kind != "asc" && k.Kind != "desc" {
			special = append(special, k.Kind)
		}
	}
	switch len(special) {
	case 0:
		return "btree"
	case 1:
		if len(keys) == 1 {
			return special[0]
		}
		return "compound_special"
	default:
		return "compound_special"
	}
}

func decodePartialFilter(raw bson.Raw) ([]stats.PartialPredicate, string) {
	els, err := raw.Elements()
	if err != nil {
		return nil, ""
	}
	preds := make([]stats.PartialPredicate, 0)
	parts := make([]string, 0)
	for _, el := range els {
		field := el.Key()
		if strings.HasPrefix(field, "$") {
			continue
		}
		val := el.Value()
		if val.Type != bson.TypeEmbeddedDocument {
			token, ok := canonicalRaw(val)
			if !ok {
				continue
			}
			preds = append(preds, stats.PartialPredicate{Field: field, Op: "eq", Value: token})
			parts = append(parts, field+":$eq")
			continue
		}
		sub, err := val.Document().Elements()
		if err != nil {
			continue
		}
		for _, op := range sub {
			if !strings.HasPrefix(op.Key(), "$") {
				continue
			}
			token, _ := canonicalRaw(op.Value())
			preds = append(preds, stats.PartialPredicate{Field: field, Op: op.Key(), Value: token})
			parts = append(parts, field+":"+op.Key())
		}
	}
	return preds, strings.Join(parts, ",")
}

func canonicalRaw(v bson.RawValue) (string, bool) {
	switch v.Type {
	case bson.TypeString:
		return stats.CanonicalPredicateValue(v.StringValue())
	case bson.TypeBoolean:
		return stats.CanonicalPredicateValue(v.Boolean())
	case bson.TypeInt32:
		return stats.CanonicalPredicateValue(int32(v.Int32()))
	case bson.TypeInt64:
		return stats.CanonicalPredicateValue(v.Int64())
	case bson.TypeDouble:
		return stats.CanonicalPredicateValue(v.Double())
	default:
		return "", false
	}
}
