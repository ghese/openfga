package storage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/oklog/ulid/v2"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/openfga/openfga/assets"
	"github.com/openfga/openfga/pkg/testutils"
)

const (
	sqlserverImage      = "mcr.microsoft.com/mssql/server:2022-latest"
	sqlserverDBPrefix   = "openfga-test-db-"
	sqlserverTemplateDB = sqlserverDBPrefix + "template"
	sqlserverUsername   = "sa"
	sqlserverPassword   = "YourStrong@Pass1"
)

var (
	_ DatastoreTestContainer = (*sqlserverTestContainer)(nil)

	sqlserverContainerName = "openfga-test-sqlserver-" + ulid.Make().String()
	sqlserverPort          = network.MustParsePort("1433/tcp")
	sqlserverDockerCont    atomic.Pointer[container.InspectResponse]

	sqlserverBootstrapping bool
	sqlserverCond          = sync.NewCond(&sync.Mutex{})
)

type sqlserverTestContainer struct {
	host string
	port string

	database string
	username string
	password string

	version int64
}

// GetConnectionURI returns the sqlserver connection uri for the running sqlserver test container.
func (a *sqlserverTestContainer) GetConnectionURI(includeCredentials bool) string {
	var username, password string
	if includeCredentials {
		username = a.username
		password = a.password
	}

	return sqlserverConnectionURI(a.host, a.port, a.database, username, password)
}

func (a *sqlserverTestContainer) GetDatabaseSchemaVersion() int64 {
	return a.version
}

func (a *sqlserverTestContainer) GetUsername() string {
	return a.username
}

func (a *sqlserverTestContainer) GetPassword() string {
	return a.password
}

func (a *sqlserverTestContainer) CreateSecondary(t testing.TB) error {
	return nil
}

func (a *sqlserverTestContainer) GetSecondaryConnectionURI(includeCredentials bool) string {
	return ""
}

func RunSQLServerTestContainer(t testing.TB) DatastoreTestContainer {
	docker, err := testutils.NewDockerClient()
	require.NoError(t, err)

	t.Cleanup(func() {
		docker.Close()
	})

	// Shared SQL Server container bootstrap for concurrent tests using sync.Cond.
	// Only one test bootstraps the shared container at a time, while others wait efficiently using sync.Cond.
	// If bootstrap fails, waiting tests are awakened so another test can retry without being affected by the failure.
	sqlserverCond.L.Lock()
	for sqlserverDockerCont.Load() == nil {
		if !sqlserverBootstrapping {
			sqlserverBootstrapping = true
			sqlserverCond.L.Unlock()

			dockerCont, err := bootstrapSQLServerContainer(t.Context(), docker)
			sqlserverCond.L.Lock()
			sqlserverBootstrapping = false
			if err == nil {
				sqlserverDockerCont.Store(dockerCont)
			}

			sqlserverCond.Broadcast()

			if err != nil {
				// Unlock before failing the test to allow waiting tests to proceed with bootstrapping.
				sqlserverCond.L.Unlock()
				require.NoError(t, err)
			}

			continue
		}

		sqlserverCond.Wait()
	}
	sqlserverCond.L.Unlock()

	dockerCont := sqlserverDockerCont.Load()
	port, err := docker.GetHostPort(dockerCont, sqlserverPort)
	require.NoError(t, err)

	version, err := latestMigrationVersion(assets.SQLServerMigrationDir)
	require.NoError(t, err, "get expected sqlserver migration version")

	testCont := &sqlserverTestContainer{
		host:     "localhost",
		port:     port,
		database: sqlserverDBPrefix + ulid.Make().String(),
		username: sqlserverUsername,
		password: sqlserverPassword,
		version:  version,
	}

	tplURI := sqlserverConnectionURI(testCont.host, testCont.port, sqlserverTemplateDB, testCont.username, testCont.password)
	require.NoError(t, waitForMigrationVersion("sqlserver", tplURI, testCont.version))

	// Create test database
	createExec := client.ExecCreateOptions{
		Cmd: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost", "-U", "sa", "-P", sqlserverPassword, "-C", "-Q", fmt.Sprintf("CREATE DATABASE [%s];", testCont.database)},
	}
	require.NoError(t, docker.ExecCommand(t.Context(), dockerCont.ID, createExec))

	// Run migrations on the test database
	db, err := goose.OpenDBWithDriver("sqlserver", testCont.GetConnectionURI(true))
	require.NoError(t, err)
	require.NoError(t, goose.Up(db, assets.SQLServerMigrationDir))
	require.NoError(t, db.Close())

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		dropQuery := fmt.Sprintf("DROP DATABASE [%s];", testCont.database)
		dropExec := client.ExecCreateOptions{
			Cmd: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost", "-U", "sa", "-P", sqlserverPassword, "-C", "-Q", dropQuery},
		}
		if err := docker.ExecCommand(ctx, dockerCont.ID, dropExec); err != nil {
			t.Errorf("drop test database in the sqlserver container: %v", err)
		}
	})

	require.NoError(t, waitForMigrationVersion("sqlserver", testCont.GetConnectionURI(true), testCont.version))

	return testCont
}

// CleanupSQLServerContainer removes the shared sqlserver test container.
// It should be called from TestMain after all tests in a package have finished.
func CleanupSQLServerContainer() {
	_ = cleanupDatastoreTestContainer(sqlserverContainerName)
}

func bootstrapSQLServerContainer(ctx context.Context, docker *testutils.DockerClient) (*container.InspectResponse, error) {
	if err := docker.PullImage(ctx, sqlserverImage); err != nil {
		return nil, fmt.Errorf("pull sqlserver image: %w", err)
	}

	contCfg := &container.Config{
		Env: []string{
			"ACCEPT_EULA=Y",
			"MSSQL_SA_PASSWORD=" + sqlserverPassword,
		},
		ExposedPorts: network.PortSet{
			sqlserverPort: {},
		},
		Image: sqlserverImage,
	}

	hostCfg := &container.HostConfig{
		PublishAllPorts: true,
	}

	cont, err := docker.RunContainer(ctx, contCfg, hostCfg, sqlserverContainerName)
	if err != nil {
		return nil, fmt.Errorf("run sqlserver container: %w", err)
	}

	needsCleanup := true
	defer func() {
		if needsCleanup {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			_ = docker.RemoveContainer(cleanupCtx, cont.ID)
		}
	}()

	port, err := docker.GetHostPort(cont, sqlserverPort)
	if err != nil {
		return nil, fmt.Errorf("get sqlserver host port: %w", err)
	}

	dbURI := sqlserverConnectionURI("localhost", port, "master", sqlserverUsername, sqlserverPassword)
	if err := waitForDatabaseWithTimeout("sqlserver", dbURI, 120*time.Second); err != nil {
		return nil, fmt.Errorf("wait for sqlserver database: %w", err)
	}

	// Create template database
	createTplExec := client.ExecCreateOptions{
		Cmd: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost", "-U", "sa", "-P", sqlserverPassword, "-C", "-Q", fmt.Sprintf("CREATE DATABASE [%s];", sqlserverTemplateDB)},
	}
	if err := docker.ExecCommand(ctx, cont.ID, createTplExec); err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}

	waitForDBURI := sqlserverConnectionURI("localhost", port, sqlserverTemplateDB, sqlserverUsername, sqlserverPassword)
	if err := waitForDatabaseWithTimeout("sqlserver", waitForDBURI, 120*time.Second); err != nil {
		return nil, fmt.Errorf("wait for template database: %w", err)
	}

	db, err := goose.OpenDBWithDriver("sqlserver", waitForDBURI)
	if err != nil {
		return nil, fmt.Errorf("open sqlserver database: %w", err)
	}
	defer db.Close()

	if err := goose.Up(db, assets.SQLServerMigrationDir); err != nil {
		return nil, fmt.Errorf("apply sqlserver migrations: %w", err)
	}

	needsCleanup = false
	return cont, nil
}

func sqlserverConnectionURI(host, port, database, username, password string) string {
	return fmt.Sprintf("sqlserver://%s:%s@%s:%s?database=%s", username, password, host, port, database)
}
