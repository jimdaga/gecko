package spanner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"github.com/go-logr/logr"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type StorageFactoryConfig struct {
	Database    string
	Client      *spanner.Client
	AdminClient *database.DatabaseAdminClient
	TablePrefix string
	Context     context.Context
	ClientOpts  []option.ClientOption
	Logger      logr.Logger

	// SkipDDL skips the DDL migration step (ensureTable, ensureChangeStream,
	// ensureSearchIndex). Set this to true when migrations are handled
	// externally, e.g. by an init container running RunMigrations.
	SkipDDL bool
}

func NewStorageFactory(config StorageFactoryConfig) (func(string, *runtime.Scheme, schema.GroupVersionKind) (storage.ResourceStore, error), error) {
	ctx := config.Context
	if ctx == nil {
		ctx = context.Background()
	}

	logger := config.Logger
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}

	client := config.Client
	if client == nil {
		var err error
		client, err = spanner.NewClient(ctx, config.Database, config.ClientOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create spanner client: %w", err)
		}
	}

	countersTable := config.TablePrefix + "counters"
	resourcesTable := config.TablePrefix + "resources"
	changeStreamName := config.TablePrefix + "cs_resources"

	if !config.SkipDDL {
		adminClient := config.AdminClient
		if adminClient == nil {
			var err error
			adminClient, err = database.NewDatabaseAdminClient(ctx, config.ClientOpts...)
			if err != nil {
				return nil, fmt.Errorf("failed to create database admin client: %w", err)
			}
		}
		if err := runDDLSetup(ctx, adminClient, config.Database, countersTable, resourcesTable, changeStreamName, logger); err != nil {
			return nil, err
		}
	}

	factory := func(resourceType string, scheme *runtime.Scheme, gvk schema.GroupVersionKind) (storage.ResourceStore, error) {
		rt := gvkString(gvk)
		resourceLogger := logger.WithValues("resource", rt)

		broadcaster, err := newSpannerBroadcaster(ctx, spannerBroadcasterConfig{
			Client:           client,
			ResourceType:     rt,
			TableName:        resourcesTable,
			ChangeStreamName: changeStreamName,
			Scheme:           scheme,
			GVK:              gvk,
			Logger:           resourceLogger,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create broadcaster: %w", err)
		}

		store, err := NewSpannerStore(SpannerStoreConfig{
			Client:        client,
			ResourceType:  rt,
			Scheme:        scheme,
			GVK:           gvk,
			Broadcaster:   broadcaster,
			TableName:     resourcesTable,
			CountersTable: countersTable,
			Logger:        resourceLogger,
		})
		if err != nil {
			broadcaster.Close()
			return nil, fmt.Errorf("failed to create store: %w", err)
		}

		return store, nil
	}

	return factory, nil
}

func gvkString(gvk schema.GroupVersionKind) string {
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}

// runDDLSetup executes DDL migrations (tables, indexes, change streams)
// without retry. Called by NewStorageFactory when SkipDDL is false.
func runDDLSetup(ctx context.Context, adminClient *database.DatabaseAdminClient, dbPath, countersTable, resourcesTable, changeStreamName string, logger logr.Logger) error {
	if err := ensureTable(ctx, adminClient, dbPath, countersTable, countersSchema(countersTable)); err != nil {
		return fmt.Errorf("failed to create counters table: %w", err)
	}

	if err := ensureTable(ctx, adminClient, dbPath, resourcesTable, resourcesSchema(resourcesTable)); err != nil {
		return fmt.Errorf("failed to create resources table: %w", err)
	}

	ensureSearchIndex(ctx, adminClient, dbPath, resourcesTable, logger)

	if err := ensureChangeStream(ctx, adminClient, dbPath, changeStreamName, changeStreamSchema(changeStreamName, resourcesTable)); err != nil {
		return fmt.Errorf("failed to create change stream: %w", err)
	}

	return nil
}

// RunMigrations creates Spanner clients, runs all DDL migrations (tables,
// indexes, change streams) with retry and exponential backoff, then closes
// the clients. It is designed to run in an init container with
// roles/spanner.databaseAdmin so that the main application container can
// use the lower-privilege roles/spanner.databaseUser.
func RunMigrations(ctx context.Context, database string, tablePrefix string, clientOpts []option.ClientOption, logger logr.Logger) error {
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}

	adminClient, err := newDatabaseAdminClient(ctx, clientOpts...)
	if err != nil {
		return fmt.Errorf("failed to create database admin client: %w", err)
	}
	defer adminClient.Close()

	countersTable := tablePrefix + "counters"
	resourcesTable := tablePrefix + "resources"
	changeStreamName := tablePrefix + "cs_resources"

	const maxRetries = 7 // ~1+2+4+8+16+32 ≈ 63s total wait before the last attempt
	backoff := time.Second

	for attempt := 0; ; attempt++ {
		err := runDDLSetup(ctx, adminClient, database, countersTable, resourcesTable, changeStreamName, logger)
		if err == nil {
			logger.Info("Spanner DDL migrations completed successfully")
			return nil
		}

		if !isRetryableErr(err) || attempt >= maxRetries {
			return fmt.Errorf("DDL migration failed after %d attempts: %w", attempt+1, err)
		}

		logger.Info("Spanner DDL migration failed, retrying",
			"error", err.Error(),
			"attempt", attempt+1,
			"backoff", backoff.String())

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during DDL migration retry: %w", ctx.Err())
		case <-time.After(backoff):
		}

		if backoff < 32*time.Second {
			backoff *= 2
		}
	}
}

// grpcStatuser is satisfied by any error that carries a gRPC status,
// including the concrete *status.Status type and wrapped gRPC errors.
type grpcStatuser interface {
	GRPCStatus() *status.Status
}

// isRetryableErr returns true for gRPC errors that may resolve on their own,
// such as PermissionDenied (IAM propagation delay) or Unavailable (transient
// backend issue). It uses errors.As to traverse the full error tree,
// including multi-wrapped errors (Go 1.20+).
func isRetryableErr(err error) bool {
	var gs grpcStatuser
	if !errors.As(err, &gs) {
		return false
	}
	switch gs.GRPCStatus().Code() {
	case codes.PermissionDenied, codes.Unavailable:
		return true
	}
	return false
}

// newDatabaseAdminClient creates a Spanner admin client. Extracted for
// testability and reuse between NewStorageFactory and RunMigrations.
func newDatabaseAdminClient(ctx context.Context, opts ...option.ClientOption) (*database.DatabaseAdminClient, error) {
	return database.NewDatabaseAdminClient(ctx, opts...)
}

func countersSchema(tableName string) []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE %s (
			counter_id STRING(253) NOT NULL,
			value INT64 NOT NULL,
		) PRIMARY KEY (counter_id)`, tableName),
	}
}

func resourcesSchema(tableName string) []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE %s (
			resource_type STRING(253) NOT NULL,
			context_filter STRING(253) NOT NULL,
			namespace STRING(253) NOT NULL,
			name STRING(253) NOT NULL,
			uid STRING(36) NOT NULL,
			resource_version INT64 NOT NULL,
			object_version INT64 NOT NULL,
			labels JSON,
			data JSON NOT NULL,
			deletion_timestamp TIMESTAMP,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
		) PRIMARY KEY (resource_type, context_filter, namespace, name)`, tableName),
		fmt.Sprintf(`CREATE INDEX idx_%s_resource_type ON %s(resource_type)`, tableName, tableName),
		fmt.Sprintf(`CREATE UNIQUE INDEX idx_%s_uid ON %s(uid)`, tableName, tableName),
		fmt.Sprintf(`CREATE INDEX idx_%s_namespace ON %s(resource_type, namespace)`, tableName, tableName),
		fmt.Sprintf(`CREATE INDEX idx_%s_rv ON %s(resource_type, resource_version) STORING (data, labels)`, tableName, tableName),
		fmt.Sprintf(`CREATE NULL_FILTERED INDEX idx_%s_deletion_timestamp ON %s(deletion_timestamp)`, tableName, tableName),
	}
}

func ensureTable(ctx context.Context, adminClient *database.DatabaseAdminClient, dbPath string, tableName string, ddlStatements []string) error {
	// Check if the table already exists by getting the DDL
	resp, err := adminClient.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{
		Database: dbPath,
	})
	if err != nil {
		return fmt.Errorf("failed to get database DDL: %w", err)
	}

	if ddlContains(resp.Statements, "CREATE TABLE "+tableName) {
		return nil
	}

	op, err := adminClient.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{
		Database:   dbPath,
		Statements: ddlStatements,
	})
	if err != nil {
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.AlreadyExists {
			return nil
		}
		return fmt.Errorf("failed to update DDL: %w", err)
	}

	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("DDL update failed: %w", err)
	}

	return nil
}

func ddlContains(statements []string, prefix string) bool {
	lower := strings.ToLower(prefix)
	for _, stmt := range statements {
		s := strings.ToLower(stmt)
		if !strings.HasPrefix(s, lower) {
			continue
		}
		rest := s[len(lower):]
		if rest == "" || rest[0] == ' ' || rest[0] == '(' || rest[0] == '\n' {
			return true
		}
	}
	return false
}

func changeStreamSchema(streamName, tableName string) []string {
	// NEW_ROW_AND_OLD_VALUES: gives all columns in new_values for INSERT/UPDATE,
	// and the deleted row's columns in old_values for DELETE (NEW_ROW leaves both empty on DELETE).
	return []string{
		fmt.Sprintf(`CREATE CHANGE STREAM %s FOR %s OPTIONS (value_capture_type = 'NEW_ROW_AND_OLD_VALUES', retention_period = '24h')`, streamName, tableName),
	}
}

func ensureChangeStream(ctx context.Context, adminClient *database.DatabaseAdminClient, dbPath string, streamName string, ddlStatements []string) error {
	resp, err := adminClient.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{
		Database: dbPath,
	})
	if err != nil {
		return fmt.Errorf("failed to get database DDL: %w", err)
	}

	if ddlContains(resp.Statements, "CREATE CHANGE STREAM "+streamName) {
		return nil
	}

	op, err := adminClient.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{
		Database:   dbPath,
		Statements: ddlStatements,
	})
	if err != nil {
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.AlreadyExists {
			return nil
		}
		return fmt.Errorf("failed to update DDL: %w", err)
	}

	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("DDL update failed: %w", err)
	}

	return nil
}

func searchIndexSchema(tableName string) []string {
	return []string{
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN labels_tokens TOKENLIST AS (TOKENIZE_JSON_OBJECT(labels)) HIDDEN`, tableName),
		fmt.Sprintf(`CREATE SEARCH INDEX idx_%s_labels ON %s(labels_tokens)`, tableName, tableName),
	}
}

func ensureSearchIndex(ctx context.Context, adminClient *database.DatabaseAdminClient, dbPath string, tableName string, logger logr.Logger) {
	resp, err := adminClient.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{
		Database: dbPath,
	})
	if err != nil {
		return
	}

	indexName := fmt.Sprintf("idx_%s_labels", tableName)
	if ddlContains(resp.Statements, "CREATE SEARCH INDEX "+indexName) {
		return
	}

	op, err := adminClient.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{
		Database:   dbPath,
		Statements: searchIndexSchema(tableName),
	})
	if err != nil {
		logger.V(1).Info("Search index not supported, label queries will work without index", "error", err)
		return
	}

	if err := op.Wait(ctx); err != nil {
		logger.V(1).Info("Search index creation failed, label queries will work without index", "error", err)
	}
}
