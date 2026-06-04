package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)
type Connection struct {
	// MongoDB connection
	Client   *mongo.Client
	Database *mongo.Database
	// MySQL connection
	DB *sql.DB
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

// TestConnection performs a real MongoDB connection validation.
// It connects using the configured URI and credentials, sends a ping,
// and disconnects immediately after the check.
func TestConnection(ctx context.Context, cfg *config.AppConfig) error {
	finalURI, err := BuildMongoURI(cfg)
	if err != nil {
		return err
	}

	clientOptions := options.Client().
		ApplyURI(finalURI).
		SetConnectTimeout(time.Duration(cfg.ConnectionParams.ConnectionTimeout) * time.Second).
		SetServerSelectionTimeout(time.Duration(cfg.ConnectionParams.ServerSelectionTimeout) * time.Second).
		SetMaxPoolSize(uint64(cfg.ConnectionParams.MaxPoolSize)).
		SetMinPoolSize(uint64(cfg.ConnectionParams.MinPoolSize)).
		SetMaxConnIdleTime(time.Duration(cfg.ConnectionParams.MaxIdleTime) * time.Minute)

	client, err := mongo.Connect(clientOptions)
	if err != nil {
		return fmt.Errorf("mongo connect error: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return fmt.Errorf("connection validation failed (check URI, credentials, or network): %w", err)
	}

	return nil
}

// ConnectMySQL connects to a MySQL server and returns a Connection.
// It validates the connection immediately with a ping.
func ConnectMySQL(ctx context.Context, cfg *config.AppConfig) (*Connection, error) {
	if cfg.ConnectionParams.Host == "" {
		cfg.ConnectionParams.Host = "localhost"
	}
	if cfg.ConnectionParams.Port == 0 {
		cfg.ConnectionParams.Port = 3306
	}

	// Build DSN: username:password@tcp(host:port)/database
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s",
		cfg.ConnectionParams.Username,
		cfg.ConnectionParams.Password,
		cfg.ConnectionParams.Host,
		cfg.ConnectionParams.Port,
		cfg.ConnectionParams.Database,
	)

	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql open error: %w", err)
	}

	// Apply connection pool settings
	sqlDB.SetMaxOpenConns(cfg.ConnectionParams.MaxPoolSize)
	sqlDB.SetMaxIdleConns(cfg.ConnectionParams.MinPoolSize)
	sqlDB.SetConnMaxIdleTime(time.Duration(cfg.ConnectionParams.MaxIdleTime) * time.Minute)

	// Test connection
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("mysql connection validation failed (check host, credentials, or network): %w", err)
	}

	return &Connection{
		DB: sqlDB,
	}, nil
}

// TestConnectionMySQL performs a real MySQL connection validation.
func TestConnectionMySQL(ctx context.Context, cfg *config.AppConfig) error {
	if cfg.ConnectionParams.Host == "" {
		cfg.ConnectionParams.Host = "localhost"
	}
	if cfg.ConnectionParams.Port == 0 {
		cfg.ConnectionParams.Port = 3306
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s",
		cfg.ConnectionParams.Username,
		cfg.ConnectionParams.Password,
		cfg.ConnectionParams.Host,
		cfg.ConnectionParams.Port,
		cfg.ConnectionParams.Database,
	)

	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("mysql open error: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(pingCtx); err != nil {
		return fmt.Errorf("mysql connection validation failed (check host, credentials, or network): %w", err)
	}

	return nil
}

// DisconnectMySQL closes the MySQL database connection.
func (c *Connection) DisconnectMySQL(ctx context.Context) error {
	if c.DB != nil {
		return c.DB.Close()
	}
	return nil
}
