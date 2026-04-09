package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/benchmark"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/logger"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/webui"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	configFlag := flag.String("config", "config.yaml", "Path to the configuration file")
	versionFlag := flag.Bool("version", false, "Print version information and exit")
	webuiFlag := flag.Bool("webui", false, "Start the interactive Web UI")
	webuiPort := flag.Int("webui-port", 8080, "Port for the Web UI")
	injectorFlag := flag.Bool("raw-injector", false, "Enable Raw BSON Injector (High Performance Mode)")
	injectorType := flag.String("raw-injector-type", "insert", "Operation: insert, upsert, update, delete, find, mixed")
	injectorSize := flag.Int("raw-injector-size", 1024, "Document size in bytes")
	injectorBatch := flag.Int("raw-injector-batch", 1000, "Bulk batch size (ops per network round trip)")
	injectorMaxDocs := flag.Int64("raw-injector-max-docs", 10000000, "Maximum number of documents to operate on")
	injectorDrop := flag.Bool("raw-injector-drop", false, "Drop the collection before starting")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "\nplgm: Percona Load Generator for MongoDB Clusters\n")
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] [config_file]\n\n", os.Args[0])

		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  %s                    # Run with default 'config.yaml'\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s my_test.yaml       # Run with specific config file\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --help             # Show this help message\n\n", os.Args[0])

		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()

		fmt.Fprintf(os.Stderr, "\nEnvironment Variables (Overrides):\n")

		fmt.Fprintf(os.Stderr, " [Connection]\n")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_URI", "Connection URI")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_USERNAME", "Database User")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_PASSWORD", "Database Password (Recommended: Use Prompt)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_DIRECT_CONNECTION", "Force direct connection (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_REPLICASET_NAME", "Replica Set name")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_READ_PREFERENCE", "nearest")

		fmt.Fprintf(os.Stderr, "\n [Web UI]\n")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "--webui", "Flag: Start the interactive Web UI")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "--webui-port", "Flag: Port for the Web UI (default: 8080)")

		fmt.Fprintf(os.Stderr, "\n [Workload Core]\n")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_DEFAULT_WORKLOAD", "Use built-in workload (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_COLLECTIONS_PATH", "Path to collection JSON")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_QUERIES_PATH", "Path to query JSON")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_DURATION", "Test duration (e.g. 60s, 5m)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_ITERATIONS", "Number of times to repeat the workload")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INTERVAL_DELAY", "Time to pause between iterations (e.g. 5s, 1m)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_CONCURRENCY", "Number of active workers")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_DOCUMENTS_COUNT", "Initial seed document count")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_DROP_COLLECTIONS", "Drop collections on start (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_SKIP_SEED", "Do not seed initial data on start (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_SEED_BATCH_SIZE", "Number of documents to insert per batch during SEED phase")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_DEBUG_MODE", "Enable verbose logic logs (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_PPROF_ENABLED", "Enable pprof server on localhost:6060 (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_USE_TRANSACTIONS", "Enable transactional workloads (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_MAX_TRANSACTION_OPS", "Maximum number of operations to group into a single transaction block")

		fmt.Fprintf(os.Stderr, "\n [Raw Injector Mode] (High Performance Hardware Test)\n")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR", "Enable Raw Injector mode (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR_TYPE", "Operation: insert, upsert, update, delete, find, mixed")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR_SIZE", "Document size in bytes")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR_BATCH_SIZE", "Operations per network batch (default: 1000)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR_MAX_DOCS", "Total documents to operate on (default: 10M)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR_DROP", "Drop collection on start (true/false)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR_DB", "Database name")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INJECTOR_COLLECTION", "Collection name")

		fmt.Fprintf(os.Stderr, "\n [Operation Ratios] (Must sum to ~100)\n")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_FIND_PERCENT", "% of ops that are FIND")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_UPDATE_PERCENT", "% of ops that are UPDATE")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INSERT_PERCENT", "% of ops that are INSERT")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_DELETE_PERCENT", "% of ops that are DELETE")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_AGGREGATE_PERCENT", "% of ops that are AGGREGATE")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_TRANSACTION_PERCENT", "% of ops that are TRANSACTIONAL")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_BULK_INSERT_PERCENT", "% of ops that are BULK INSERTS")

		fmt.Fprintf(os.Stderr, "\n [Performance Optimization]\n")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_FIND_BATCH_SIZE", "Docs returned per cursor batch")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INSERT_BATCH_SIZE", "Number of docs in batch bulk insert")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_FIND_LIMIT", "Max docs per Find query")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_INSERT_CACHE_SIZE", "Generator buffer size")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_OP_TIMEOUT_MS", "Soft timeout per DB op (ms)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_RETRY_ATTEMPTS", "Retry attempts for failures")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_RETRY_BACKOFF_MS", "Wait time between retries (ms)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "PLGM_STATUS_REFRESH_RATE_SEC", "Status report interval (sec)")
		fmt.Fprintf(os.Stderr, "  %-35s %s\n", "GOMAXPROCS", "Go Runtime CPU limit")
		fmt.Fprintf(os.Stderr, "\n")
	}

	flag.Parse()

	if *versionFlag {
		fmt.Printf("plgm v%s\n", version)
		os.Exit(0)
	}

	configPath := *configFlag
	if len(flag.Args()) > 0 {
		configPath = flag.Args()[0]
	}

	// Determine if the user is explicitly trying to start the Web UI
	webUIRequested := *webuiFlag || strings.ToLower(os.Getenv("PLGM_WEBUI_ENABLED")) == "true"

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// If running in CLI mode, strictly require the config file
		if !webUIRequested {
			fmt.Printf("Error: Configuration file '%s' not found.\n", configPath)
			fmt.Printf("To run the CLI workload, a config file is required. To run the interactive Web UI without a config, use the --webui flag.\n")
			os.Exit(1)
		}
		// If Web UI IS requested, we just log a warning and proceed with an empty/default config
		log.Printf("Warning: Configuration file '%s' not found. Starting Web UI with default settings.\n", configPath)
	} else if err != nil {
		fmt.Printf("Error checking config file '%s': %v\n", configPath, err)
		os.Exit(1)
	}

	ctx := context.Background()
	appCfg, err := config.LoadAppConfig(configPath, webUIRequested)
	if err != nil {
		log.Fatal("Failed to load application config:", err)
	}

	if appCfg.PprofEnabled {
		go func() {
			log.Println("PPROF server running on localhost:6060")
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	if *webuiFlag || appCfg.WebUI.Enabled {
		port := appCfg.WebUI.Port
		// If the user explicitly passed the --webui-port flag, it overrides the config file
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "webui-port" {
				port = *webuiPort
			}
		})

		server := webui.NewServer(appCfg)
		if err := server.Start(port); err != nil {
			log.Fatalf("Failed to start Web UI: %v", err)
		}
		return // Exit main so we don't accidentally run the CLI workload
	}

	if !strings.Contains(appCfg.URI, "compressors=") {
		if strings.Contains(appCfg.URI, "?") {
			appCfg.URI += "&compressors=none"
		} else {
			appCfg.URI += "?compressors=none"
		}
		logger.Info("Performance: Automatically disabled driver compression (compressors=none)")
	}

	u, _ := url.Parse(appCfg.URI)
	if u.User == nil && appCfg.ConnectionParams.Username == "" {
		fmt.Print("Enter MongoDB Username: ")
		var inputUser string
		fmt.Scanln(&inputUser)
		appCfg.ConnectionParams.Username = inputUser
	}
	if appCfg.ConnectionParams.Username != "" && appCfg.ConnectionParams.Password == "" {
		fmt.Printf("Enter Password for user '%s': ", appCfg.ConnectionParams.Username)
		bytePassword, _ := term.ReadPassword(int(os.Stdin.Fd()))
		appCfg.ConnectionParams.Password = string(bytePassword)
		fmt.Println()
	}

	// -----------------------------------------------------------------------
	// EXECUTION BRANCH 1: RAW INJECTOR MODE
	// -----------------------------------------------------------------------
	if *injectorFlag || appCfg.RawInjector.Enabled {
		appCfg.RawInjector.Enabled = true

		if *injectorType != "insert" {
			appCfg.RawInjector.Type = *injectorType
		}
		if *injectorSize != 1024 {
			appCfg.RawInjector.DocumentSize = *injectorSize
		}
		if *injectorBatch != 1000 {
			appCfg.RawInjector.BatchSize = *injectorBatch
		}
		if *injectorMaxDocs != 10000000 {
			appCfg.RawInjector.MaxDocs = *injectorMaxDocs
		}
		if *injectorDrop {
			appCfg.RawInjector.DropCollection = true
		}

		if appCfg.RawInjector.Type == "" {
			appCfg.RawInjector.Type = "insert"
		}
		if appCfg.RawInjector.DocumentSize == 0 {
			appCfg.RawInjector.DocumentSize = 200
		}
		if appCfg.RawInjector.BatchSize == 0 {
			appCfg.RawInjector.BatchSize = 1000
		}
		if appCfg.RawInjector.MaxDocs == 0 {
			appCfg.RawInjector.MaxDocs = 10000000
		}

		injectorDBName := appCfg.RawInjector.DBName
		if injectorDBName == "" {
			injectorDBName = "plgm_injector"
		}

		benchConn, err := db.Connect(ctx, appCfg, injectorDBName)
		if err != nil {
			log.Fatal(err)
		}
		defer benchConn.Disconnect(ctx)

		intervalDuration, _ := time.ParseDuration(appCfg.IntervalDelay)
		for i := 1; i <= appCfg.Iterations; i++ {
			log.Printf("Starting Raw Injector iteration %d of %d", i, appCfg.Iterations)
			if err := benchmark.RunRawInjector(ctx, benchConn.Database, appCfg); err != nil {
				log.Fatal(err)
			}
			if i < appCfg.Iterations && intervalDuration > 0 {
				log.Printf("Waiting %s before next iteration...", appCfg.IntervalDelay)
				time.Sleep(intervalDuration)
			}
		}
		return
	}

	// -----------------------------------------------------------------------
	// EXECUTION BRANCH 2: STANDARD WORKLOAD
	// -----------------------------------------------------------------------
	collectionsCfg, err := config.LoadCollections(appCfg.CollectionsPath, appCfg.DefaultWorkload)
	if err != nil {
		log.Fatal(err)
	}

	if len(collectionsCfg.Collections) == 0 {
		log.Fatal("No collections found")
	}

	queriesCfg, err := config.LoadQueries(appCfg.QueriesPath, appCfg.DefaultWorkload)
	if err != nil {
		log.Fatal(err)
	}

	if err := config.ValidateCollectionDefinitions(collectionsCfg.Collections); err != nil {
		log.Fatal(err)
	}

	if err := config.NormalizeAndValidateQueries(queriesCfg.Queries); err != nil {
		log.Fatal(err)
	}
	boundQueries, err := config.ValidateAndBindQueriesToCollections(queriesCfg.Queries, collectionsCfg.Collections)
	if err != nil {
		log.Fatal(err)
	}
	queriesCfg.Queries = boundQueries

	dbName := collectionsCfg.Collections[0].DatabaseName
	stats.PrintConfiguration(appCfg, collectionsCfg.Collections, version)

	conn, err := db.Connect(ctx, appCfg, dbName)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Disconnect(ctx)

	initStart := time.Now()
	log.Printf("[Lifecycle] Phase=initializing: preparing collections, indexes, sharding setup, and optional seed data")
	log.Printf("[Lifecycle] Init Step 1/3: preparing collections")
	if err := mongo.CreateCollectionsFromConfig(ctx, conn.Database, collectionsCfg, appCfg.DropCollections); err != nil {
		log.Fatal(err)
	}
	totalIndexes := 0
	for _, col := range collectionsCfg.Collections {
		totalIndexes += len(col.Indexes)
	}
	log.Printf("[Lifecycle] Init Step 2/3: creating indexes (declared indexes=%d)", totalIndexes)
	if err := mongo.CreateIndexesFromConfig(ctx, conn.Database, collectionsCfg); err != nil {
		log.Fatal(err)
	}

	log.Printf("[Lifecycle] Init Step 3/3: optional seed (skip_seed=%v, documents_count=%d)", appCfg.SkipSeed, appCfg.DocumentsCount)
	if !appCfg.SkipSeed && appCfg.DocumentsCount > 0 {
		for _, col := range collectionsCfg.Collections {
			if err := mongo.InsertRandomDocuments(ctx, conn.Database, col, appCfg.DocumentsCount, appCfg); err != nil {
				log.Fatal(err)
			}
		}
	}
	initDuration := time.Since(initStart)
	log.Printf("[Lifecycle] Initialization completed in %s", initDuration.Round(10*time.Millisecond))
	log.Printf("[Lifecycle] Phase=running: execution timers now track query workload only")

	intervalDuration, _ := time.ParseDuration(appCfg.IntervalDelay)
	for i := 1; i <= appCfg.Iterations; i++ {
		iterStart := time.Now()
		log.Printf("Starting Standard Workload iteration %d of %d", i, appCfg.Iterations)
		if err := mongo.RunWorkload(ctx, conn.Database, collectionsCfg.Collections, queriesCfg.Queries, appCfg); err != nil {
			log.Fatal(err)
		}
		log.Printf("[Lifecycle] Iteration %d execution duration: %s", i, time.Since(iterStart).Round(10*time.Millisecond))
		if i < appCfg.Iterations && intervalDuration > 0 {
			log.Printf("Waiting %s before next iteration...", appCfg.IntervalDelay)
			time.Sleep(intervalDuration)
		}
	}
}
