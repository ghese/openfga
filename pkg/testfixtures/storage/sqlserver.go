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
	azureImage      = "mcr.microsoft.com/mssql/server:2022-latest"
	azureDBPrefix   = "openfga-test-db-"
	azureTemplateDB = azureDBPrefix + "template"
	azureUsername   = "sa"
	azurePassword   = "YourStrong@Pass1"
)

var (
	_ DatastoreTestContainer = (*azureTestContainer)(nil)

	azureContainerName = "openfga-test-azure-" + ulid.Make().String()
	azurePort          = network.MustParsePort("1433/tcp")
	azureDockerCont    atomic.Pointer[container.InspectResponse]

	azureBootstrapping bool
	azureCond          = sync.NewCond(&sync.Mutex{})
)

type azureTestContainer struct {
	host string
	port string

	database string
	username string
	password string

	version int64
}

// GetConnectionURI returns the azure sql connection uri for the running azure test container.
func (a *azureTestContainer) GetConnectionURI(includeCredentials bool) string {
	var username, password string
	if includeCredentials {
		username = a.username
		password = a.password
	}

	return azureConnectionURI(a.host, a.port, a.database, username, password)
}

func (a *azureTestContainer) GetDatabaseSchemaVersion() int64 {
	return a.version
}

func (a *azureTestContainer) GetUsername() string {
	return a.username
}

func (a *azureTestContainer) GetPassword() string {
	return a.password
}

func (a *azureTestContainer) CreateSecondary(t testing.TB) error {
	return nil
}

func (a *azureTestContainer) GetSecondaryConnectionURI(includeCredentials bool) string {
	return ""
}

func RunAzureTestContainer(t testing.TB) DatastoreTestContainer {
	docker, err := testutils.NewDockerClient()
	require.NoError(t, err)

	t.Cleanup(func() {
		docker.Close()
	})

	// Shared Azure SQL container bootstrap for concurrent tests using sync.Cond.
	// Only one test bootstraps the shared container at a time, while others wait efficiently using sync.Cond.
	// If bootstrap fails, waiting tests are awakened so another test can retry without being affected by the failure.
	azureCond.L.Lock()
	for azureDockerCont.Load() == nil {
		if !azureBootstrapping {
			azureBootstrapping = true
			azureCond.L.Unlock()

			dockerCont, err := bootstrapAzureContainer(t.Context(), docker)
			azureCond.L.Lock()
			azureBootstrapping = false
			if err == nil {
				azureDockerCont.Store(dockerCont)
			}

			azureCond.Broadcast()

			if err != nil {
				// Unlock before failing the test to allow waiting tests to proceed with bootstrapping.
				azureCond.L.Unlock()
				require.NoError(t, err)
			}

			continue
		}

		azureCond.Wait()
	}
	azureCond.L.Unlock()

	dockerCont := azureDockerCont.Load()
	port, err := docker.GetHostPort(dockerCont, azurePort)
	require.NoError(t, err)

	version, err := latestMigrationVersion(assets.AzureMigrationDir)
	require.NoError(t, err, "get expected azure migration version")

	testCont := &azureTestContainer{
		host:     "localhost",
		port:     port,
		database: azureDBPrefix + ulid.Make().String(),
		username: azureUsername,
		password: azurePassword,
		version:  version,
	}

	tplURI := azureConnectionURI(testCont.host, testCont.port, azureTemplateDB, testCont.username, testCont.password)
	require.NoError(t, waitForMigrationVersion("sqlserver", tplURI, testCont.version))

	// Create test database
	createExec := client.ExecCreateOptions{
		Cmd: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost", "-U", "sa", "-P", azurePassword, "-C", "-Q", fmt.Sprintf("CREATE DATABASE [%s];", testCont.database)},
	}
	require.NoError(t, docker.ExecCommand(t.Context(), dockerCont.ID, createExec))

	// Run migrations on the test database
	db, err := goose.OpenDBWithDriver("sqlserver", testCont.GetConnectionURI(true))
	require.NoError(t, err)
	require.NoError(t, goose.Up(db, assets.AzureMigrationDir))
	require.NoError(t, db.Close())

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		dropQuery := fmt.Sprintf("DROP DATABASE [%s];", testCont.database)
		dropExec := client.ExecCreateOptions{
			Cmd: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost", "-U", "sa", "-P", azurePassword, "-C", "-Q", dropQuery},
		}
		if err := docker.ExecCommand(ctx, dockerCont.ID, dropExec); err != nil {
			t.Errorf("drop test database in the azure container: %v", err)
		}
	})

	require.NoError(t, waitForMigrationVersion("sqlserver", testCont.GetConnectionURI(true), testCont.version))

	return testCont
}

// CleanupAzureContainer removes the shared azure test container.
// It should be called from TestMain after all tests in a package have finished.
func CleanupAzureContainer() {
	_ = cleanupDatastoreTestContainer(azureContainerName)
}

func bootstrapAzureContainer(ctx context.Context, docker *testutils.DockerClient) (*container.InspectResponse, error) {
	if err := docker.PullImage(ctx, azureImage); err != nil {
		return nil, fmt.Errorf("pull azure image: %w", err)
	}

	contCfg := &container.Config{
		Env: []string{
			"ACCEPT_EULA=Y",
			"MSSQL_SA_PASSWORD=" + azurePassword,
		},
		ExposedPorts: network.PortSet{
			azurePort: {},
		},
		Image: azureImage,
	}

	hostCfg := &container.HostConfig{
		PublishAllPorts: true,
	}

	cont, err := docker.RunContainer(ctx, contCfg, hostCfg, azureContainerName)
	if err != nil {
		return nil, fmt.Errorf("run azure container: %w", err)
	}

	needsCleanup := true
	defer func() {
		if needsCleanup {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			_ = docker.RemoveContainer(cleanupCtx, cont.ID)
		}
	}()

	port, err := docker.GetHostPort(cont, azurePort)
	if err != nil {
		return nil, fmt.Errorf("get azure host port: %w", err)
	}

	dbURI := azureConnectionURI("localhost", port, "master", azureUsername, azurePassword)
	if err := waitForDatabaseWithTimeout("sqlserver", dbURI, 120*time.Second); err != nil {
		return nil, fmt.Errorf("wait for azure database: %w", err)
	}

	// Create template database
	createTplExec := client.ExecCreateOptions{
		Cmd: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost", "-U", "sa", "-P", azurePassword, "-C", "-Q", fmt.Sprintf("CREATE DATABASE [%s];", azureTemplateDB)},
	}
	if err := docker.ExecCommand(ctx, cont.ID, createTplExec); err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}

	waitForDBURI := azureConnectionURI("localhost", port, azureTemplateDB, azureUsername, azurePassword)
	if err := waitForDatabaseWithTimeout("sqlserver", waitForDBURI, 120*time.Second); err != nil {
		return nil, fmt.Errorf("wait for template database: %w", err)
	}

	db, err := goose.OpenDBWithDriver("sqlserver", waitForDBURI)
	if err != nil {
		return nil, fmt.Errorf("open azure database: %w", err)
	}
	defer db.Close()

	if err := goose.Up(db, assets.AzureMigrationDir); err != nil {
		return nil, fmt.Errorf("apply azure migrations: %w", err)
	}

	needsCleanup = false
	return cont, nil
}

func azureConnectionURI(host, port, database, username, password string) string {
	return fmt.Sprintf("sqlserver://%s:%s@%s:%s?database=%s", username, password, host, port, database)
}
