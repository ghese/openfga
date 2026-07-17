package validatemodels

import (
	"os"
	"testing"

	storagefixtures "github.com/openfga/openfga/pkg/testfixtures/storage"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storagefixtures.CleanupPostgresContainer()
	storagefixtures.CleanupMysqlContainer()
	storagefixtures.CleanupAzureContainer()
	os.Exit(code)
}
