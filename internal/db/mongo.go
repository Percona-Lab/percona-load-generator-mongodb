package db

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type Connection struct {
	Client   *mongo.Client
	Database *mongo.Database
}

func BuildMongoURI(cfg *config.AppConfig) (string, error) {
	u, err := url.Parse(cfg.URI)
	if err != nil {
		return "", fmt.Errorf("invalid base URI: %w", err)
	}

	if u.Path == "" {
		u.Path = "/"
	}

	// --- Inject Credentials if provided separately ---
	// This overrides any credentials present in the base URI string.
	if cfg.ConnectionParams.Username != "" {
		if cfg.ConnectionParams.Password != "" {
			u.User = url.UserPassword(cfg.ConnectionParams.Username, cfg.ConnectionParams.Password)
		} else {
			u.User = url.User(cfg.ConnectionParams.Username)
		}
	}

	q := u.Query()

	// --- connection params ---
	if cfg.ConnectionParams.AuthSource != "" {
		q.Set("authSource", cfg.ConnectionParams.AuthSource)
	}

	if cfg.ConnectionParams.ReadPreference != "" {
		q.Set("readPreference", cfg.ConnectionParams.ReadPreference)
	}

	// If a Replica Set name is provided, we assume we are targeting that specific set
	// and force a direct connection to the node provided in the URI.
	// If Replica Set is EMPTY (e.g. connecting to mongos), we skip both.
	// This allows the driver to Auto-Discover the mongos topology.
	if cfg.ConnectionParams.ReplicaSetName != "" {
		q.Set("replicaSet", cfg.ConnectionParams.ReplicaSetName)
		// Next we check if direct connection has been disabled by the user
		// This could happen in cases where the user is trying to connect to multiple replicaset hosts
		// so this would be set to false. Default is true so this will always be set, unless explicitly overwritten
		if cfg.ConnectionParams.DirectConnection {
			q.Set("directConnection", "true")
		}
	}

	// --- custom params ---
	for key, val := range cfg.CustomParamsMap {
		q.Set(key, fmt.Sprintf("%v", val))
	}

	// Default compressor if user did NOT provide any
	if _, exists := cfg.CustomParamsMap["compressors"]; !exists {
		q.Set("compressors", "zstd")
	}

	u.RawQuery = q.Encode()

	return u.String(), nil
}

// ---------------------------------------------------------
// Connect sets driver options + optional debug logging
// ---------------------------------------------------------
// ---------------------------------------------------------
// Connect sets driver options + optional debug logging
// ---------------------------------------------------------
func Connect(ctx context.Context, cfg *config.AppConfig, dbName string) (*Connection, error) {

	finalURI, err := BuildMongoURI(cfg)
	if err != nil {
		return nil, err
	}

	clientOptions := options.Client().
		ApplyURI(finalURI).
		SetConnectTimeout(time.Duration(cfg.ConnectionParams.ConnectionTimeout) * time.Second).
		SetServerSelectionTimeout(time.Duration(cfg.ConnectionParams.ServerSelectionTimeout) * time.Second).
		SetMaxPoolSize(uint64(cfg.ConnectionParams.MaxPoolSize)).
		SetMinPoolSize(uint64(cfg.ConnectionParams.MinPoolSize)).
		SetMaxConnIdleTime(time.Duration(cfg.ConnectionParams.MaxIdleTime) * time.Minute)

	// -----------------------------------------------------
	// Connect client
	// -----------------------------------------------------
	client, err := mongo.Connect(clientOptions)
	if err != nil {
		return nil, fmt.Errorf("mongo connect error: %w", err)
	}

	// -----------------------------------------------------
	// FAIL FAST: Quick Connection Validation
	// Force a strict 3-second timeout for the initial ping.
	// If the URI, credentials, or network are bad, it fails immediately.
	// -----------------------------------------------------
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("connection validation failed (check URI, credentials, or network): %w", err)
	}

	return &Connection{
		Client:   client,
		Database: client.Database(dbName),
	}, nil
}

// Disconnect gracefully closes the client connection.
func (c *Connection) Disconnect(ctx context.Context) {
	_ = c.Client.Disconnect(ctx)
}
