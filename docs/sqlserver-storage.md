# SQL Server Storage Engine (SQL Server & Azure SQL Database)

This document describes how to build, configure, and verify OpenFGA with the `sqlserver` storage engine. The engine supports **Microsoft SQL Server** (2019+, self-hosted or containerized) and **Azure SQL Database** (the managed cloud service), including Microsoft Entra ID authentication.

## Prerequisites

- Go 1.25.7+
- A SQL Server instance: an Azure SQL Database, or a local Docker SQL Server for development
- OpenFGA source code

---

## Build

**PowerShell:**
```powershell
go build -o ".\dist\openfga.exe" .\cmd\openfga\
.\dist\openfga.exe version
```

**bash:**
```bash
go build -o ./dist/openfga ./cmd/openfga/
./dist/openfga version
```

---

## Azure SQL Database Setup

### 1. Provision an Azure SQL Database

Create an Azure SQL Database through the Azure Portal, CLI, or infrastructure-as-code. Note the following:

- **Server name**: `your-server.database.windows.net`
- **Database name**: `openfga`
- **Admin credentials**: SQL authentication login and password, or Microsoft Entra ID authentication

### 2. Configure the Firewall

Add your client IP address to the server-level firewall rule in Azure Portal → your SQL server → Networking → Firewall rules.

If running OpenFGA on Azure (e.g., an Azure VM or App Service), place it in the same VNet or use Private Endpoint.

### 3. Create the Database

If the database doesn't exist yet, connect and create it using any SQL client:

```sql
CREATE DATABASE openfga;
```

### 4. Run Migrations

**PowerShell:**
```powershell
.\dist\openfga.exe migrate `
  --datastore-engine sqlserver `
  --datastore-uri "server=your-server.database.windows.net;user id=your-admin;password=your-password;port=1433;database=openfga;encrypt=true;trustservercertificate=false"
```

**bash:**
```bash
./dist/openfga migrate \
  --datastore-engine sqlserver \
  --datastore-uri "server=your-server.database.windows.net;user id=your-admin;password=your-password;port=1433;database=openfga;encrypt=true;trustservercertificate=false"
```

Expected output:
```
db info {"current version": 0}
running all migrations
migration done
```

> The migration files are embedded at build time via `//go:embed`. Rebuild the binary if the migration files change.

### 5. Run the Server

**PowerShell:**
```powershell
.\dist\openfga.exe run `
  --datastore-engine sqlserver `
  --datastore-uri "server=your-server.database.windows.net;user id=your-admin;password=your-password;port=1433;database=openfga;encrypt=true;trustservercertificate=false"
```

**bash:**
```bash
./dist/openfga run \
  --datastore-engine sqlserver \
  --datastore-uri "server=your-server.database.windows.net;user id=your-admin;password=your-password;port=1433;database=openfga;encrypt=true;trustservercertificate=false"
```

---

## Connection String Reference

The `microsoft/go-mssqldb` driver accepts both the **URL format** (`sqlserver://...`) and the **DSN format** (semicolon-delimited key=value pairs). The repository's own tooling (Makefile `dev-run`, test fixtures) uses the URL format.

> The `--datastore-username` / `--datastore-password` override flags (env: `OPENFGA_DATASTORE_USERNAME` / `OPENFGA_DATASTORE_PASSWORD`) work with **both** formats, for both `openfga run` and `openfga migrate`. This lets you keep the non-secret part of the connection string in plain configuration and inject only the password from a secret store.

### Connection Parameters (DSN format)

| Parameter | Required | Description |
|---|---|---|
| `server` | Yes | SQL Server host or Azure SQL server name (e.g. `your-server.database.windows.net`) |
| `user id` | SQL auth only | SQL authentication username (omit when using `fedauth=` Entra ID modes, except user-assigned managed identity) |
| `password` | SQL auth only | SQL authentication password (omit when using `fedauth=` Entra ID modes) |
| `database` | Yes | Database name |
| `port` | No | Default: `1433` |
| `encrypt` | No | Driver default is `false` — set `true` explicitly for Azure SQL and any production server (see note below); `false` only for local dev |
| `trustservercertificate` | No | `true` for self-signed certs (local dev); `false` for Azure SQL / production (requires valid cert) |

> **Encryption note:** the `go-mssqldb` driver defaults to `encrypt=false`. Azure SQL Database *forces* TLS during the TDS handshake, so connections to it are encrypted even without the parameter — but always set `encrypt=true;trustservercertificate=false` explicitly for downgrade protection and to fail fast on misconfiguration.

### Examples

**Azure SQL Database** (production, DSN format):
```
server=my-server.database.windows.net;user id=admin;password=P@ssw0rd;port=1433;database=openfga;encrypt=true;trustservercertificate=false
```

**Azure SQL with Microsoft Entra ID authentication** — the engine connects through the `go-mssqldb/azuread` driver, which reads the **`fedauth`** connection string parameter (note: *not* `authentication`, the ADO.NET name — the driver silently ignores that and falls back to SQL/Windows auth). Supported workflows include `ActiveDirectoryDefault`, `ActiveDirectoryManagedIdentity` (`ActiveDirectoryMSI`), `ActiveDirectoryServicePrincipal`, `ActiveDirectoryWorkloadIdentity`, `ActiveDirectoryAzCli`, and `ActiveDirectoryInteractive`:

System-assigned managed identity:
```
server=my-server.database.windows.net;database=openfga;encrypt=true;fedauth=ActiveDirectoryManagedIdentity
```

User-assigned managed identity (pass the identity's client ID as `user id`):
```
server=my-server.database.windows.net;database=openfga;encrypt=true;fedauth=ActiveDirectoryManagedIdentity;user id=<client-id-of-identity>
```

Azure CLI login (local development against Azure SQL):
```
server=my-server.database.windows.net;database=openfga;encrypt=true;fedauth=ActiveDirectoryAzCli
```

> The managed identity (or the server's Entra ID principal) must be created as a database user and granted permissions, e.g. `CREATE USER [my-app] FROM EXTERNAL PROVIDER; ALTER ROLE db_owner ADD MEMBER [my-app];` (migrations require DDL rights; the runtime server only needs `db_datareader`/`db_datawriter`).

**Local Docker SQL Server** (development, URL format):
```
sqlserver://sa:YourStrong%40Pass1@localhost:1433?database=openfga
```

**Local Docker SQL Server** (development, DSN format):
```
server=localhost;user id=sa;password=YourStrong@Pass1;port=1433;database=openfga;encrypt=false;trustservercertificate=true
```

> Note: In the URL format, percent-encode special characters in the username and password (`@` → `%40`, `:` → `%3A`, `/` → `%2F`). An unencoded `@` in the password happens to parse (Go splits the authority at the last `@`), but do not rely on it. The DSN format needs no escaping.

---

## Azure SQL Database Production Notes

- **Connection pooling**: Azure SQL tiers cap concurrent sessions and workers (per DTU/vCore). Size `--datastore-max-open-conns` below your tier's worker limit — a pool larger than the worker limit converts load spikes into throttling errors (`10928`/`10929`) instead of queueing. When running multiple OpenFGA replicas, divide the budget across replicas.
- **Serverless tier auto-pause**: the serverless tier pauses after inactivity, and the first connection after resume can take 30–60 s. Set `--datastore-conn-max-idle-time` below the auto-pause delay to avoid holding stale connections across a pause, or disable auto-pause for latency-sensitive deployments.
- **Retry semantics**: the engine maps Azure SQL conditions to retryable API errors — clients should retry with backoff:
  - Throttling (error numbers `10928`, `10929`, `40501`, `49918`–`49920`) → `429 ResourceExhausted`
  - Write deadlock victim (error `1205`) → `409 Conflict` (the transaction was rolled back; safe to retry)
  - Failover/reconfiguration (`4060`, `40143`, `40197`, `40613`) surface as generic errors; they are transient — retry with backoff
- **Least-privilege permissions**: `openfga migrate` needs DDL rights (`db_ddladmin` + `db_datareader`/`db_datawriter`, or `db_owner`). The `openfga run` server only needs `db_datareader` + `db_datawriter` — use a separate, lower-privileged principal for the runtime.
- **Entra ID over SQL auth**: prefer `fedauth=ActiveDirectoryManagedIdentity` (no secrets to rotate) when OpenFGA runs on Azure compute. SQL authentication remains available for non-Azure hosting.

---

## Local Development with Docker

For development and testing without an Azure subscription, use a local SQL Server container:

**PowerShell:**
```powershell
docker run -d --name mssql `
  -e "ACCEPT_EULA=Y" `
  -e "MSSQL_SA_PASSWORD=YourStrong@Pass1" `
  -p 1433:1433 `
  mcr.microsoft.com/mssql/server:2022-latest

# Create the database
docker exec mssql /opt/mssql-tools18/bin/sqlcmd `
  -S localhost -U sa -P "YourStrong@Pass1" -C `
  -Q "CREATE DATABASE [openfga];"

# Run migrations
.\dist\openfga.exe migrate `
  --datastore-engine sqlserver `
  --datastore-uri "server=localhost;user id=sa;password=YourStrong@Pass1;port=1433;database=openfga;encrypt=false;trustservercertificate=true"

# Start the server
.\dist\openfga.exe run `
  --datastore-engine sqlserver `
  --datastore-uri "server=localhost;user id=sa;password=YourStrong@Pass1;port=1433;database=openfga;encrypt=false;trustservercertificate=true"
```

**bash:**
```bash
docker run -d --name mssql \
  -e "ACCEPT_EULA=Y" \
  -e "MSSQL_SA_PASSWORD=YourStrong@Pass1" \
  -p 1433:1433 \
  mcr.microsoft.com/mssql/server:2022-latest

# Create the database
docker exec mssql /opt/mssql-tools18/bin/sqlcmd \
  -S localhost -U sa -P "YourStrong@Pass1" -C \
  -Q "CREATE DATABASE [openfga];"

# Run migrations
./dist/openfga migrate \
  --datastore-engine sqlserver \
  --datastore-uri "server=localhost;user id=sa;password=YourStrong@Pass1;port=1433;database=openfga;encrypt=false;trustservercertificate=true"

# Start the server
./dist/openfga run \
  --datastore-engine sqlserver \
  --datastore-uri "server=localhost;user id=sa;password=YourStrong@Pass1;port=1433;database=openfga;encrypt=false;trustservercertificate=true"
```

> The `-C` flag on sqlcmd and `trustservercertificate=true` in the connection string are needed for the self-signed certificate in the Docker container. Azure SQL Database uses a proper CA-signed certificate and requires `encrypt=true;trustservercertificate=false`.

---

## Manual Smoke Tests

These tests verify the SQL Server engine end-to-end via the HTTP API. Run them against your Azure SQL Database instance to validate a cloud deployment.

### 1. Health Check

**PowerShell:**
```powershell
curl.exe -s http://localhost:8080/healthz
```

**bash:**
```bash
curl -s http://localhost:8080/healthz
```

Expected: `{"status":"SERVING"}`

### 2. Create a Store

**PowerShell:**
```powershell
$response = curl.exe -s -X POST http://localhost:8080/stores `
  -H "Content-Type: application/json" `
  -d '{"name":"smoke-test"}'
Write-Output $response
```

**bash:**
```bash
curl -s -X POST http://localhost:8080/stores \
  -H "Content-Type: application/json" \
  -d '{"name":"smoke-test"}'
```

Extract the store ID from the response.

### 3. Write an Authorization Model

**PowerShell:**
```powershell
$storeId = "<extracted-store-id>"

curl.exe -s -X POST "http://localhost:8080/stores/$storeId/authorization-models" `
  -H "Content-Type: application/json" `
  -d '{
    "schema_version": "1.1",
    "type_definitions": [
      {"type": "user"},
      {"type": "document", "relations": {"viewer": {"this": {}}}, "metadata": {"relations": {"viewer": {"directly_related_user_types": [{"type": "user"}]}}}}
    ]
  }'
```

**bash:**
```bash
storeId="<extracted-store-id>"

curl -s -X POST "http://localhost:8080/stores/$storeId/authorization-models" \
  -H "Content-Type: application/json" \
  -d '{
    "schema_version": "1.1",
    "type_definitions": [
      {"type": "user"},
      {"type": "document", "relations": {"viewer": {"this": {}}}, "metadata": {"relations": {"viewer": {"directly_related_user_types": [{"type": "user"}]}}}}
    ]
  }'
```

Expected: response containing an authorization model ID.

### 4. Write a Tuple

**PowerShell:**
```powershell
curl.exe -s -X POST "http://localhost:8080/stores/$storeId/write" `
  -H "Content-Type: application/json" `
  -d '{
    "writes": [
      {"tuple_key": {"object": "document:1", "relation": "viewer", "user": "user:alice"}}
    ]
  }'
```

**bash:**
```bash
curl -s -X POST "http://localhost:8080/stores/$storeId/write" \
  -H "Content-Type: application/json" \
  -d '{
    "writes": [
      {"tuple_key": {"object": "document:1", "relation": "viewer", "user": "user:alice"}}
    ]
  }'
```

Expected: `{}`

### 5. Check Authorization

**PowerShell:**
```powershell
curl.exe -s -X POST "http://localhost:8080/stores/$storeId/check" `
  -H "Content-Type: application/json" `
  -d '{
    "tuple_key": {"object": "document:1", "relation": "viewer", "user": "user:alice"}
  }'
```

**bash:**
```bash
curl -s -X POST "http://localhost:8080/stores/$storeId/check" \
  -H "Content-Type: application/json" \
  -d '{
    "tuple_key": {"object": "document:1", "relation": "viewer", "user": "user:alice"}
  }'
```

Expected: `{"allowed":true}`

### 6. Negative Check

**PowerShell:**
```powershell
curl.exe -s -X POST "http://localhost:8080/stores/$storeId/check" `
  -H "Content-Type: application/json" `
  -d '{
    "tuple_key": {"object": "document:1", "relation": "viewer", "user": "user:bob"}
  }'
```

**bash:**
```bash
curl -s -X POST "http://localhost:8080/stores/$storeId/check" \
  -H "Content-Type: application/json" \
  -d '{
    "tuple_key": {"object": "document:1", "relation": "viewer", "user": "user:bob"}
  }'
```

Expected: `{"allowed":false}`

### 7. Read Tuples

**PowerShell:**
```powershell
curl.exe -s -X POST "http://localhost:8080/stores/$storeId/read" `
  -H "Content-Type: application/json" `
  -d '{"tuple_key": {"object": "document:1", "relation": "viewer"}}'
```

**bash:**
```bash
curl -s -X POST "http://localhost:8080/stores/$storeId/read" \
  -H "Content-Type: application/json" \
  -d '{"tuple_key": {"object": "document:1", "relation": "viewer"}}'
```

Expected: returns the tuple written in step 4.

### 8. Read Changes

**PowerShell:**
```powershell
curl.exe -s "http://localhost:8080/stores/$storeId/changes?type=document&page_size=10"
```

**bash:**
```bash
curl -s "http://localhost:8080/stores/$storeId/changes?type=document&page_size=10"
```

Expected: returns a change entry for the write from step 4.

### 9. List Stores

**PowerShell:**
```powershell
curl.exe -s "http://localhost:8080/stores?page_size=10"
```

**bash:**
```bash
curl -s "http://localhost:8080/stores?page_size=10"
```

Expected: the smoke-test store appears in the list.

### 10. Delete Store

**PowerShell:**
```powershell
curl.exe -s -X DELETE "http://localhost:8080/stores/$storeId"
```

**bash:**
```bash
curl -s -X DELETE "http://localhost:8080/stores/$storeId"
```

Expected: `{}`

---

## Running Automated Tests

All of the tests below require Docker; they spin up a shared SQL Server container, create per-test databases, and run migrations automatically.

### Storage Test Suite

Runs the complete `OpenFGADatastore` test suite (`test.RunAllTests`) plus sqlserver-specific tests:

```bash
go test -count=1 -timeout 20m ./pkg/storage/sqlserver/
```

The first run pulls the `mcr.microsoft.com/mssql/server:2022-latest` image. The full suite completes in ~90 seconds.

> On Windows, omit `-race` unless a cgo toolchain is installed.

### Matrix and API Tests

Runs the primary correctness tests (Check, ListObjects, ListUsers) against sqlserver:

```bash
go test -count=1 -timeout 60m -run "TestMatrixSQLServer|TestCheckSQLServer|TestListObjectsSQLServer|TestListUsersSQLServer" ./tests/check/ ./tests/listobjects/ ./tests/listusers/
```

### Migration Rollback Test

Exercises every goose `Up`/`Down` migration:

```bash
go test -count=1 -run "TestMigrateCommandRollbacks/sqlserver" ./pkg/storage/migrate/
```

### Shared sqlcommon Tests

Tests the shared SQL logic used by all backends:

```bash
go test ./pkg/storage/sqlcommon/
```

---

## Architecture

### Component Map

| Component | Location | Purpose |
|---|---|---|
| Datastore | `pkg/storage/sqlserver/sqlserver.go` | Full `OpenFGADatastore` implementation |
| Datastore tests | `pkg/storage/sqlserver/sqlserver_test.go` | Storage test suite via Docker container |
| Test fixture | `pkg/testfixtures/storage/sqlserver.go` | Docker container bootstrap and cleanup |
| Migration dispatch | `pkg/storage/migrate/migrate.go` | Routes `"sqlserver"` engine to the `"azuresql"` goose driver (the azuread driver's registered name) |
| Server dispatch | `cmd/run/run.go` | Routes `"sqlserver"` engine to `sqlserver.New()` |
| Test bootstrap | `cmd/util/util.go` | `MustBootstrapDatastore` support for `"sqlserver"` |
| Model validation | `cmd/validatemodels/validate_models.go` | `validate-models` command support for `"sqlserver"` |
| Matrix tests | `tests/check`, `tests/listobjects`, `tests/listusers` | `TestMatrixSQLServer`, `TestCheckSQLServer`, `TestListObjectsSQLServer`, `TestListUsersSQLServer` |
| Shared SQL | `pkg/storage/sqlcommon/sqlcommon.go` | `TimestampExpr` on `WriteData` for dialect-specific timestamps |
| Build config | `Makefile` | Adds sqlserver to `STORAGE_PACKAGES` and `dev-run` |

### Migration Files

```
assets/migrations/sqlserver/
  001_initialize_schema.sql
  002_add_authorization_model_version.sql
  003_add_reverse_lookup_index.sql
  004_add_authorization_model_serialized_protobuf.sql
  005_add_conditions_to_tuples.sql
```

5 separate migration files are required because `build.MinimumSupportedDatastoreSchemaRevision = 4` — the readiness check requires at least version 4.

All textual identifier columns are created with `COLLATE Latin1_General_BIN2` (see Known Limitations). The `Down` migration in 002 drops the named default constraint `df_auth_model_schema_version` before dropping the column, as SQL Server refuses to drop a column that a constraint references.

### Resolver Chain

The sqlserver engine uses the same resolver chain as other storage engines (resolvers in brackets are added conditionally based on configuration; the chain is circular — the last resolver delegates back to the first):
```
[CachedCheckResolver]? → [DispatchThrottlingCheckResolver]? → [ShadowResolver | LocalChecker] ⟲
```

See `internal/graph/builder.go` for details.

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---|---|---|
| `Login failed for user` | Wrong credentials or Azure SQL firewall | Verify credentials in connection string; add client IP to Azure SQL firewall rules |
| `Cannot open database "openfga"` | Database doesn't exist | Create the database before running migrations |
| `Incorrect syntax near '?'` | Placeholder format mismatch | The driver uses `@pN` format implicitly — any `?` in generated SQL indicates wrong placeholder format |
| `Incorrect syntax near 'LIMIT'` | SQL Server pagination syntax | Use `OFFSET 0 ROWS FETCH NEXT N ROWS ONLY` instead of `LIMIT N` |
| `nvarchar to varbinary(max) not allowed` | Untyped nil for VARBINARY column | Use `[]byte(nil)` instead of bare `nil` |
| `datastore requires migrations: at revision '1', but requires '4'` | Too few migration files | Keep 5 separate migration files (001–005); single file produces version 1 |
| Server shows `NOT_SERVING` | Missing or incomplete migrations | Run `openfga migrate` |
| `429` / `transaction was throttled by the datastore` (error numbers 10928, 10929, 40501, 49918–49920) | Azure SQL resource limits or throttling ([transient errors](https://learn.microsoft.com/azure/azure-sql/database/troubleshoot-common-errors-issues)) | Retry after a backoff. Watch elastic pool / DTU limits if persistent |
| `sql error` with mssql error number 4060, 40143, 40197, or 40613 | Azure SQL failover or reconfiguration ([transient errors](https://learn.microsoft.com/azure/azure-sql/database/troubleshoot-common-errors-issues)) | Transient; retry after a backoff |
| `409 Conflict` on Write during load | Deadlock (error 1205) mapped to retryable conflict | Safe to retry the write |
| `unable to open tcp connection` (Docker) | SQL Server still initializing | Wait 15–30 seconds or increase timeout |
| `The system directory [/.system] could not be created` (Docker) | tmpfs permission in non-root container | Remove `Tmpfs` from container HostConfig |

---

## Known Limitations

- **Collation**: All textual identifier columns are declared with `COLLATE Latin1_General_BIN2` in the schema migrations, so equality, uniqueness, and ordering are case-sensitive and bytewise — matching Postgres, MySQL (post-008), and SQLite. The `ReadStartingWithUser` query additionally specifies `COLLATE Latin1_General_BIN2` on `ORDER BY object_id` explicitly. Note that per ANSI SQL padding rules, SQL Server still ignores trailing spaces in `VARCHAR` equality comparisons regardless of collation; OpenFGA identifier validation rejects whitespace, so this does not affect API traffic.
- **Driver**: Uses `microsoft/go-mssqldb` v1.10.0 through the `azuread` driver (`azuread.DriverName`), which supports SQL authentication and all Entra ID `fedauth=` workflows. Placeholders use `sq.AtP` format (`@p1`, `@p2`, ...). Standard `?` placeholders via `database/sql` are not used because the driver does not universally convert them.
- **Test containers**: The test fixture creates per-test databases and runs migrations on each, unlike Postgres which supports `CREATE DATABASE ... TEMPLATE ...`.
