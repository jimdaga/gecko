package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/stdr"
	_ "github.com/lib/pq"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/postgres"
	spannerbackend "github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/spanner"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

func main() {
	var (
		address         string
		privatePort     int
		publicPort      int
		corsOrigins     string
		enablePublic    bool
		tlsCertFile     string
		tlsKeyFile      string
		authnKubeconfig string
		authzKubeconfig string
		disableAuth     bool
		migrateOnly     bool
	)

	flag.StringVar(&address, "address", "0.0.0.0", "address to bind to")
	flag.IntVar(&privatePort, "private-port", 8080, "port for private API")
	flag.IntVar(&publicPort, "public-port", 8081, "port for public API")
	flag.BoolVar(&enablePublic, "enable-public-api", true, "enable public API server")
	flag.StringVar(&corsOrigins, "cors-origins", "*", "comma-separated list of allowed CORS origins")
	flag.StringVar(&tlsCertFile, "tls-cert-file", "", "path to TLS certificate (auto-generated if empty)")
	flag.StringVar(&tlsKeyFile, "tls-key-file", "", "path to TLS private key (auto-generated if empty)")
	flag.StringVar(&authnKubeconfig, "authentication-kubeconfig", "", "kubeconfig for delegated authentication (in-cluster if empty)")
	flag.StringVar(&authzKubeconfig, "authorization-kubeconfig", "", "kubeconfig for delegated authorization (in-cluster if empty)")
	flag.BoolVar(&disableAuth, "disable-auth", false, "disable authentication/authorization (for testing/local dev)")
	flag.BoolVar(&migrateOnly, "migrate-only", false, "run Spanner DDL migrations and exit (for init containers)")
	flag.Parse()

	logger := stdr.New(nil)

	// --migrate-only: run Spanner DDL migrations with retry and exit.
	// Designed for use as a Kubernetes init container with elevated IAM
	// permissions (roles/spanner.databaseAdmin) so the main container can
	// run with only roles/spanner.databaseUser.
	if migrateOnly {
		spannerDB := os.Getenv("SPANNER_DATABASE")
		if spannerDB == "" {
			log.Fatal("--migrate-only requires SPANNER_DATABASE to be set")
		}
		tablePrefix := os.Getenv("SPANNER_TABLE_PREFIX")

		log.Printf("Running Spanner DDL migrations: %s", spannerDB)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		if err := spannerbackend.RunMigrations(ctx, spannerDB, tablePrefix, nil, logger); err != nil {
			log.Fatalf("Spanner DDL migration failed: %v", err)
		}

		log.Println("Spanner DDL migrations completed successfully")
		os.Exit(0)
	}

	// Parse CORS origins
	origins := []string{}
	if corsOrigins != "" {
		origins = strings.Split(corsOrigins, ",")
		for i, origin := range origins {
			origins[i] = strings.TrimSpace(origin)
		}
	}

	// Configure storage backend
	// Priority: Spanner (SPANNER_DATABASE set) > PostgreSQL (DB_HOST set) > in-memory (default)
	var storageFactory apiserver.StorageFactory
	if spannerDB := os.Getenv("SPANNER_DATABASE"); spannerDB != "" {
		// SPANNER_DATABASE must be a full Spanner database resource path:
		//   projects/<project>/instances/<instance>/databases/<database>
		// Set SPANNER_EMULATOR_HOST to redirect to a local emulator, e.g.:
		//   SPANNER_EMULATOR_HOST=localhost:9010
		tablePrefix := os.Getenv("SPANNER_TABLE_PREFIX")

		// SKIP_DDL_SETUP is set by the Helm chart when an init container
		// handles DDL migrations. The init container runs with
		// roles/spanner.databaseAdmin; the main container only needs
		// roles/spanner.databaseUser for data operations.
		skipDDL := os.Getenv("SKIP_DDL_SETUP") == "true"
		if skipDDL {
			log.Println("Skipping DDL setup (handled by init container)")
		}

		log.Printf("Connecting to Spanner database: %s", spannerDB)

		factory, err := spannerbackend.NewStorageFactory(spannerbackend.StorageFactoryConfig{
			Database:    spannerDB,
			TablePrefix: tablePrefix,
			Context:     context.Background(),
			Logger:      logger,
			SkipDDL:     skipDDL,
		})
		if err != nil {
			log.Fatalf("Failed to create Spanner storage factory: %v", err)
		}

		log.Printf("Connected to Spanner: %s", spannerDB)
		storageFactory = factory
	} else if dbHost := os.Getenv("DB_HOST"); dbHost != "" {
		dbPort := os.Getenv("DB_PORT")
		if dbPort == "" {
			dbPort = "5432"
		}
		dbName := os.Getenv("DB_NAME")
		if dbName == "" {
			dbName = "orlop"
		}
		dbUser := os.Getenv("DB_USER")
		if dbUser == "" {
			dbUser = "orlop"
		}
		dbPassword := os.Getenv("DB_PASSWORD")
		dbSSLMode := os.Getenv("DB_SSLMODE")
		if dbSSLMode == "" {
			dbSSLMode = "disable"
		}

		connStr := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=%s",
			dbHost, dbPort, dbName, dbUser, dbPassword, dbSSLMode)

		db, err := sql.Open("postgres", connStr)
		if err != nil {
			log.Fatalf("Failed to open database: %v", err)
		}
		defer db.Close()

		if err := db.Ping(); err != nil {
			log.Fatalf("Failed to connect to database: %v", err)
		}

		log.Printf("Connected to PostgreSQL at %s:%s/%s", dbHost, dbPort, dbName)

		storageFactory = postgres.NewStorageFactory(postgres.StorageFactoryConfig{
			DB:         db,
			ConnString: connStr,
			Context:    context.Background(),
		})
	} else {
		storageFactory = func(resourceType string, scheme *runtime.Scheme, gvk runtimeschema.GroupVersionKind) (storage.ResourceStore, error) {
			return memory.NewMemoryStore(resourceType, scheme, gvk), nil
		}
		log.Println("No SPANNER_DATABASE or DB_HOST set, using in-memory storage")
	}

	// Create server with resource configuration
	opts := apiserver.Options{
		Address: address,
		Private: apiserver.PrivateAPIOptions{
			Port:                     privatePort,
			Resources:                getPrivateResources(),
			Scheme:                   getPrivateScheme(),
			TLSCertFile:              tlsCertFile,
			TLSKeyFile:               tlsKeyFile,
			AuthenticationKubeconfig: authnKubeconfig,
			AuthorizationKubeconfig:  authzKubeconfig,
			DisableAuth:              disableAuth,
		},
		Public: apiserver.PublicAPIOptions{
			Enable:    enablePublic,
			Port:      publicPort,
			Resources: getPublicResources(),
			Scheme:    getPublicScheme(),
		},
		StorageFactory: storageFactory,
		CORSOrigins:    origins,
		Logger:         logger,
	}

	server, err := apiserver.New(opts)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start server in goroutine
	go func() {
		if err := server.Run(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for signal
	<-sigChan
	log.Println("Shutting down server...")

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown error: %v", err)
	}

	log.Println("Server stopped")
}
