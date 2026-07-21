package azure

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/logger"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	"github.com/openfga/openfga/pkg/storage/test"
	storagefixtures "github.com/openfga/openfga/pkg/testfixtures/storage"
	"github.com/openfga/openfga/pkg/testutils"
	tupleUtils "github.com/openfga/openfga/pkg/tuple"
	"github.com/openfga/openfga/pkg/typesystem"
)

func TestAzureDatastore(t *testing.T) {
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)
	defer ds.Close()

	test.RunAllTests(t, ds)

	// Run tests with a custom large max_tuples_per_write value.
	dsCustom, err := New(uri, sqlcommon.NewConfig(
		sqlcommon.WithMaxTuplesPerWrite(5000),
	))
	require.NoError(t, err)
	defer dsCustom.Close()

	t.Run("WriteTuplesWithMaxTuplesPerWrite", test.WriteTuplesWithMaxTuplesPerWrite(dsCustom, context.Background()))
}

func TestAzureDatastoreAfterCloseIsNotReady(t *testing.T) {
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)
	ds.Close()
	status, err := ds.IsReady(context.Background())
	require.Error(t, err)
	require.False(t, status.IsReady)
}

// TestReadEnsureNoOrder asserts that the read response is not ordered by ulid.
func TestReadEnsureNoOrder(t *testing.T) {
	tests := []struct {
		name  string
		mixed bool
	}{
		{
			name:  "nextOnly",
			mixed: false,
		},
		{
			name:  "mixed",
			mixed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

			uri := testDatastore.GetConnectionURI(true)
			cfg := sqlcommon.NewConfig()
			ds, err := New(uri, cfg)
			require.NoError(t, err)
			defer ds.Close()

			ctx := context.Background()
			store := "store"
			firstTuple := tupleUtils.NewTupleKey("doc:object_id_1", "relation", "user:user_1")
			secondTuple := tupleUtils.NewTupleKey("doc:object_id_2", "relation", "user:user_2")
			thirdTuple := tupleUtils.NewTupleKey("doc:object_id_3", "relation", "user:user_3")

			err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{firstTuple}, storage.NewTupleWriteOptions(), time.Now())
			require.NoError(t, err)

			// Tweak time so that ULID is smaller.
			err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{secondTuple}, storage.NewTupleWriteOptions(), time.Now().Add(time.Minute*-1))
			require.NoError(t, err)

			// Tweak time so that ULID is smaller.
			err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{thirdTuple}, storage.NewTupleWriteOptions(), time.Now().Add(time.Minute*-2))
			require.NoError(t, err)

			iter, err := ds.Read(ctx, store, storage.ReadFilter{Object: "doc:", Relation: "relation", User: ""}, storage.ReadOptions{})
			defer iter.Stop()

			require.NoError(t, err)

			// We expect that objectID1 will return first because it is inserted first.
			if tt.mixed {
				curTuple, err := iter.Head(ctx)
				require.NoError(t, err)
				require.Equal(t, firstTuple, curTuple.GetKey())

				// calling head should not change the order
				curTuple, err = iter.Head(ctx)
				require.NoError(t, err)
				require.Equal(t, firstTuple, curTuple.GetKey())
			}

			curTuple, err := iter.Next(ctx)
			require.NoError(t, err)
			require.Equal(t, firstTuple, curTuple.GetKey())

			curTuple, err = iter.Next(ctx)
			require.NoError(t, err)
			require.Equal(t, secondTuple, curTuple.GetKey())

			if tt.mixed {
				curTuple, err = iter.Head(ctx)
				require.NoError(t, err)
				require.Equal(t, thirdTuple, curTuple.GetKey())
			}

			curTuple, err = iter.Next(ctx)
			require.NoError(t, err)
			require.Equal(t, thirdTuple, curTuple.GetKey())

			if tt.mixed {
				_, err = iter.Head(ctx)
				require.ErrorIs(t, err, storage.ErrIteratorDone)
			}

			_, err = iter.Next(ctx)
			require.ErrorIs(t, err, storage.ErrIteratorDone)
		})
	}
}

func TestCtxCancel(t *testing.T) {
	tests := []struct {
		name  string
		mixed bool
	}{
		{
			name:  "nextOnly",
			mixed: false,
		},
		{
			name:  "mixed",
			mixed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

			uri := testDatastore.GetConnectionURI(true)
			cfg := sqlcommon.NewConfig()
			ds, err := New(uri, cfg)
			require.NoError(t, err)
			defer ds.Close()

			ctx, cancel := context.WithCancel(context.Background())

			store := "store"
			firstTuple := tupleUtils.NewTupleKey("doc:object_id_1", "relation", "user:user_1")
			secondTuple := tupleUtils.NewTupleKey("doc:object_id_2", "relation", "user:user_2")
			thirdTuple := tupleUtils.NewTupleKey("doc:object_id_3", "relation", "user:user_3")

			err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{firstTuple}, storage.NewTupleWriteOptions(), time.Now())
			require.NoError(t, err)

			// Tweak time so that ULID is smaller.
			err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{secondTuple}, storage.NewTupleWriteOptions(), time.Now().Add(time.Minute*-1))
			require.NoError(t, err)

			// Tweak time so that ULID is smaller.
			err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{thirdTuple}, storage.NewTupleWriteOptions(), time.Now().Add(time.Minute*-2))
			require.NoError(t, err)

			iter, err := ds.Read(ctx, store, storage.ReadFilter{Object: "doc:", Relation: "relation", User: ""}, storage.ReadOptions{})
			defer iter.Stop()
			require.NoError(t, err)

			cancel()

			if tt.mixed {
				_, err = iter.Head(ctx)
				require.Error(t, err)
				require.NotEqual(t, storage.ErrIteratorDone, err)
			}

			_, err = iter.Next(ctx)
			require.Error(t, err)
			require.NotEqual(t, storage.ErrIteratorDone, err)
		})
	}
}

// TestReadPageEnsureOrder asserts that the read page is ordered by ulid.
func TestReadPageEnsureOrder(t *testing.T) {
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)
	defer ds.Close()

	ctx := context.Background()

	store := "store"
	firstTuple := tupleUtils.NewTupleKey("doc:object_id_1", "relation", "user:user_1")
	secondTuple := tupleUtils.NewTupleKey("doc:object_id_2", "relation", "user:user_2")

	err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{firstTuple}, storage.NewTupleWriteOptions(), time.Now())
	require.NoError(t, err)

	// Tweak time so that ULID is smaller.
	err = ds.write(ctx, store, nil, []*openfgav1.TupleKey{secondTuple}, storage.NewTupleWriteOptions(), time.Now().Add(time.Minute*-1))
	require.NoError(t, err)

	opts := storage.ReadPageOptions{
		Pagination: storage.NewPaginationOptions(storage.DefaultPageSize, ""),
	}
	tuples, _, err := ds.ReadPage(ctx,
		store,
		storage.ReadFilter{Object: "doc:", Relation: "relation", User: ""}, opts)
	require.NoError(t, err)

	require.Len(t, tuples, 2)
	// We expect that objectID2 will return first because it has a smaller ulid.
	require.Equal(t, secondTuple, tuples[0].GetKey())
	require.Equal(t, firstTuple, tuples[1].GetKey())
}

func TestAzureDatastore_ReadPageWithUserFiltering(t *testing.T) {
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)
	defer ds.Close()

	ctx := context.Background()

	store := ulid.Make().String()

	// Create multiple tuples with same user type for pagination testing
	var tuples []*openfgav1.TupleKey
	for i := 0; i < 10; i++ {
		tuples = append(tuples, &openfgav1.TupleKey{
			Object:   "doc:group1",
			Relation: "viewer",
			User:     fmt.Sprintf("user:user%d", i),
		})
		tuples = append(tuples, &openfgav1.TupleKey{
			Object:   "doc:group1",
			Relation: "viewer",
			User:     fmt.Sprintf("group:admin%d", i),
		})
	}

	err = ds.Write(ctx, store, nil, tuples)
	require.NoError(t, err)

	// Test pagination with user type filtering
	filter := storage.ReadFilter{
		Object:   "doc:group1",
		Relation: "viewer",
		User:     "user:",
	}

	readPageOptions := storage.ReadPageOptions{
		Pagination: storage.PaginationOptions{
			PageSize: 5,
		},
	}

	// First page
	tuples1, token, err := ds.ReadPage(ctx, store, filter, readPageOptions)
	require.NoError(t, err)
	require.Len(t, tuples1, 5)
	require.NotEmpty(t, token)

	// All returned tuples should have user type "user"
	for _, tuple := range tuples1 {
		userType, _, _ := tupleUtils.ToUserParts(tuple.GetKey().GetUser())
		require.Equal(t, "user", userType)
	}

	// Second page
	readPageOptions.Pagination.From = token
	tuples2, token2, err := ds.ReadPage(ctx, store, filter, readPageOptions)
	require.NoError(t, err)
	require.Len(t, tuples2, 5)
	require.Empty(t, token2)
}

func TestReadAuthorizationModelUnmarshallError(t *testing.T) {
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)

	ctx := context.Background()
	defer ds.Close()
	store := "store"
	modelID := "foo"
	schemaVersion := typesystem.SchemaVersion1_0

	bytes, err := proto.Marshal(&openfgav1.TypeDefinition{Type: "document"})
	require.NoError(t, err)
	pbdata := []byte{0x01, 0x02, 0x03}

	_, err = ds.db.ExecContext(ctx, "INSERT INTO authorization_model (store, authorization_model_id, schema_version, type, type_definition, serialized_protobuf) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)", store, modelID, schemaVersion, "document", bytes, pbdata)
	require.NoError(t, err)

	_, err = ds.ReadAuthorizationModel(ctx, store, modelID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot parse invalid wire-format data")
}

func TestReadAuthorizationModelReturnValue(t *testing.T) {
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)

	ctx := context.Background()
	defer ds.Close()
	store := "store"
	modelID := "foo"
	schemaVersion := typesystem.SchemaVersion1_0

	bytes, err := proto.Marshal(&openfgav1.TypeDefinition{Type: "document"})
	require.NoError(t, err)

	_, err = ds.db.ExecContext(ctx, "INSERT INTO authorization_model (store, authorization_model_id, schema_version, type, type_definition, serialized_protobuf) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)", store, modelID, schemaVersion, "document", bytes, []byte(nil))

	require.NoError(t, err)

	res, err := ds.ReadAuthorizationModel(ctx, store, modelID)
	require.NoError(t, err)
	// AuthorizationModel should return only 1 type which is of type "document"
	require.Len(t, res.GetTypeDefinitions(), 1)
	require.Equal(t, "document", res.GetTypeDefinitions()[0].GetType())
}

func TestFindLatestModel(t *testing.T) {
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)
	defer ds.Close()

	ctx := context.Background()
	store := ulid.Make().String()

	schemaVersion := typesystem.SchemaVersion1_1

	t.Run("works_when_first_model_is_one_row_and_second_model_is_split", func(t *testing.T) {
		model := testutils.MustTransformDSLToProtoWithID(`
			model
				schema 1.1
			type user1`)
		err := ds.WriteAuthorizationModel(ctx, store, model)
		require.NoError(t, err)

		latestModel, err := ds.FindLatestAuthorizationModel(ctx, store)
		require.NoError(t, err)
		require.Len(t, latestModel.GetTypeDefinitions(), 1)

		modelID := ulid.Make().String()
		// write type "document"
		bytesDocumentType, err := proto.Marshal(&openfgav1.TypeDefinition{Type: "document"})
		require.NoError(t, err)
		_, err = ds.db.ExecContext(ctx, "INSERT INTO authorization_model (store, authorization_model_id, schema_version, type, type_definition, serialized_protobuf) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)",
			store, modelID, schemaVersion, "document", bytesDocumentType, []byte(nil))
		require.NoError(t, err)

		// write type "user"
		bytesUserType, err := proto.Marshal(&openfgav1.TypeDefinition{Type: "user"})
		require.NoError(t, err)
		_, err = ds.db.ExecContext(ctx, "INSERT INTO authorization_model (store, authorization_model_id, schema_version, type, type_definition, serialized_protobuf) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)",
			store, modelID, schemaVersion, "user", bytesUserType, []byte(nil))
		require.NoError(t, err)

		latestModel, err = ds.FindLatestAuthorizationModel(ctx, store)
		require.NoError(t, err)
		require.Len(t, latestModel.GetTypeDefinitions(), 2)
	})

	t.Run("works_when_first_model_is_split_and_second_model_is_one_row", func(t *testing.T) {
		modelID := ulid.Make().String()
		// write type "document"
		bytesDocumentType, err := proto.Marshal(&openfgav1.TypeDefinition{Type: "document"})
		require.NoError(t, err)
		_, err = ds.db.ExecContext(ctx, "INSERT INTO authorization_model (store, authorization_model_id, schema_version, type, type_definition, serialized_protobuf) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)",
			store, modelID, schemaVersion, "document", bytesDocumentType, []byte(nil))
		require.NoError(t, err)

		// write type "user"
		bytesUserType, err := proto.Marshal(&openfgav1.TypeDefinition{Type: "user"})
		require.NoError(t, err)
		_, err = ds.db.ExecContext(ctx, "INSERT INTO authorization_model (store, authorization_model_id, schema_version, type, type_definition, serialized_protobuf) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)",
			store, modelID, schemaVersion, "user", bytesUserType, []byte(nil))
		require.NoError(t, err)

		latestModel, err := ds.FindLatestAuthorizationModel(ctx, store)
		require.NoError(t, err)
		require.Len(t, latestModel.GetTypeDefinitions(), 2)

		model := testutils.MustTransformDSLToProtoWithID(`
			model
				schema 1.1
			type user1`)
		err = ds.WriteAuthorizationModel(ctx, store, model)
		require.NoError(t, err)

		latestModel, err = ds.FindLatestAuthorizationModel(ctx, store)
		require.NoError(t, err)
		require.Len(t, latestModel.GetTypeDefinitions(), 1)
	})
}

// TestAllowNullCondition tests that tuple and changelog rows existing before
// migration 005_add_conditions_to_tuples can be successfully read.
func TestAllowNullCondition(t *testing.T) {
	ctx := context.Background()
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)
	defer ds.Close()

	stmt := `
		INSERT INTO tuple (
			store, object_type, object_id, relation, _user, user_type, ulid,
			condition_name, condition_context, inserted_at
		) VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, SYSDATETIME());
	`
	_, err = ds.db.ExecContext(
		ctx, stmt, "store", "folder", "2021-budget", "owner", "user:anne", "user",
		ulid.Make().String(), nil, []byte(nil),
	)
	require.NoError(t, err)

	tk := tupleUtils.NewTupleKey("folder:2021-budget", "owner", "user:anne")
	filter := storage.ReadFilter{
		Object:   "folder:2021-budget",
		Relation: "owner",
		User:     "user:anne",
	}
	iter, err := ds.Read(ctx, "store", filter, storage.ReadOptions{})
	require.NoError(t, err)
	defer iter.Stop()

	curTuple, err := iter.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, tk, curTuple.GetKey())

	opts := storage.ReadPageOptions{
		Pagination: storage.NewPaginationOptions(2, ""),
	}
	tuples, _, err := ds.ReadPage(ctx, "store", storage.ReadFilter{}, opts)
	require.NoError(t, err)
	require.Len(t, tuples, 1)
	require.Equal(t, tk, tuples[0].GetKey())

	userTuple, err := ds.ReadUserTuple(ctx, "store", filter, storage.ReadUserTupleOptions{})
	require.NoError(t, err)
	require.Equal(t, tk, userTuple.GetKey())

	tk2 := tupleUtils.NewTupleKey("folder:2022-budget", "viewer", "user:anne")
	_, err = ds.db.ExecContext(
		ctx, stmt, "store", "folder", "2022-budget", "viewer", "user:anne", "userset",
		ulid.Make().String(), nil, []byte(nil),
	)

	require.NoError(t, err)
	iter, err = ds.ReadUsersetTuples(ctx, "store", storage.ReadUsersetTuplesFilter{Object: "folder:2022-budget"}, storage.ReadUsersetTuplesOptions{})
	require.NoError(t, err)
	defer iter.Stop()

	curTuple, err = iter.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, tk2, curTuple.GetKey())

	iter, err = ds.ReadStartingWithUser(ctx, "store", storage.ReadStartingWithUserFilter{
		ObjectType: "folder",
		Relation:   "owner",
		UserFilter: []*openfgav1.ObjectRelation{
			{Object: "user:anne"},
		},
	}, storage.ReadStartingWithUserOptions{})
	require.NoError(t, err)
	defer iter.Stop()

	curTuple, err = iter.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, tk, curTuple.GetKey())

	stmt = `
	INSERT INTO changelog (
		store, object_type, object_id, relation, _user, ulid,
		condition_name, condition_context, inserted_at, operation
	) VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, SYSDATETIME(), @p9);
`
	_, err = ds.db.ExecContext(
		ctx, stmt, "store", "folder", "2021-budget", "owner", "user:anne",
		ulid.Make().String(), nil, []byte(nil), openfgav1.TupleOperation_TUPLE_OPERATION_WRITE,
	)
	require.NoError(t, err)

	_, err = ds.db.ExecContext(
		ctx, stmt, "store", "folder", "2021-budget", "owner", "user:anne",
		ulid.Make().String(), nil, []byte(nil), openfgav1.TupleOperation_TUPLE_OPERATION_DELETE,
	)
	require.NoError(t, err)

	readChangesOpts := storage.ReadChangesOptions{
		Pagination: storage.NewPaginationOptions(storage.DefaultPageSize, ""),
	}
	changes, _, err := ds.ReadChanges(ctx, "store", storage.ReadChangesFilter{ObjectType: "folder"}, readChangesOpts)
	require.NoError(t, err)
	require.Len(t, changes, 2)
	require.Equal(t, tk, changes[0].GetTupleKey())
	require.Equal(t, tk, changes[1].GetTupleKey())
}

// TestMarshalledAssertions tests that previously persisted marshalled
// assertions can be read back. In any case where the Assertions proto model
// needs to change, we'll likely need to introduce a series of data migrations.
func TestMarshalledAssertions(t *testing.T) {
	ctx := context.Background()
	testDatastore := storagefixtures.RunDatastoreTestContainer(t, "azure")

	uri := testDatastore.GetConnectionURI(true)
	cfg := sqlcommon.NewConfig()
	ds, err := New(uri, cfg)
	require.NoError(t, err)
	defer ds.Close()

	// Note: this represents an assertion written on v1.3.7.
	stmt := `
		INSERT INTO assertion (
			store, authorization_model_id, assertions
		) VALUES (@p1, @p2, CAST(0x0A2B0A270A12666F6C6465723A323032312D62756467657412056F776E65721A0A757365723A616E6E657A1001 AS VARBINARY(MAX)));
	`
	_, err = ds.db.ExecContext(ctx, stmt, "store", "model")
	require.NoError(t, err)

	assertions, err := ds.ReadAssertions(ctx, "store", "model")
	require.NoError(t, err)

	expectedAssertions := []*openfgav1.Assertion{
		{
			TupleKey: &openfgav1.AssertionTupleKey{
				Object:   "folder:2021-budget",
				Relation: "owner",
				User:     "user:annez",
			},
			Expectation: true,
		},
	}
	require.Equal(t, expectedAssertions, assertions)
}

func TestNew(t *testing.T) {
	type args struct {
		uri string
		cfg *sqlcommon.Config
	}
	tests := []struct {
		name    string
		args    args
		want    *Datastore
		wantErr bool
	}{
		{
			name: "bad_uri",
			args: args{
				uri: "my;uri?bad=true",
				cfg: &sqlcommon.Config{
					Logger: logger.NewNoopLogger(),
				},
			},
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := sqlcommon.NewConfig()
			got, err := New(tt.args.uri, cfg)
			if got != nil {
				defer got.Close()
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}

func TestApplyCredentials(t *testing.T) {
	tests := []struct {
		name         string
		uri          string
		username     string
		password     string
		wantUser     string
		wantPassword string
		wantErr      error
	}{
		{
			name:         "no_overrides_returns_uri_unchanged",
			uri:          "server=localhost;user id=sa;password=original;database=openfga",
			wantUser:     "sa",
			wantPassword: "original",
		},
		{
			name:         "dsn_format_overrides_both",
			uri:          "server=localhost;user id=sa;password=original;database=openfga;encrypt=true",
			username:     "override-user",
			password:     "override-pass",
			wantUser:     "override-user",
			wantPassword: "override-pass",
		},
		{
			name:         "dsn_format_overrides_password_only",
			uri:          "server=localhost;user id=sa;password=original;database=openfga",
			password:     "override-pass",
			wantUser:     "sa",
			wantPassword: "override-pass",
		},
		{
			name:         "dsn_format_password_with_special_characters",
			uri:          "server=localhost;user id=sa;database=openfga",
			password:     `p@ss;word="quoted"`,
			wantUser:     "sa",
			wantPassword: `p@ss;word="quoted"`,
		},
		{
			name:         "dsn_format_trailing_semicolon",
			uri:          "server=localhost;database=openfga;",
			username:     "sa",
			password:     "secret",
			wantUser:     "sa",
			wantPassword: "secret",
		},
		{
			name:         "url_format_overrides_both",
			uri:          "sqlserver://sa:original@localhost:1433?database=openfga",
			username:     "override-user",
			password:     "override-pass",
			wantUser:     "override-user",
			wantPassword: "override-pass",
		},
		{
			name:         "url_format_overrides_username_only_preserves_password",
			uri:          "sqlserver://sa:original@localhost:1433?database=openfga",
			username:     "override-user",
			wantUser:     "override-user",
			wantPassword: "original",
		},
		{
			name:     "url_format_username_only_no_existing_credentials",
			uri:      "sqlserver://localhost:1433?database=openfga",
			username: "sa",
			wantUser: "sa",
		},
		{
			name:         "url_format_no_existing_credentials",
			uri:          "sqlserver://localhost:1433?database=openfga",
			username:     "sa",
			password:     "secret",
			wantUser:     "sa",
			wantPassword: "secret",
		},
		{
			name:         "url_format_password_only_no_existing_credentials",
			uri:          "sqlserver://localhost:1433?database=openfga",
			password:     "secret",
			wantUser:     "",
			wantPassword: "secret",
		},
		{
			name:         "url_format_unencoded_at_in_existing_password",
			uri:          "sqlserver://sa:YourStrong@Pass1@localhost:1433?database=openfga",
			username:     "override-user",
			wantUser:     "override-user",
			wantPassword: "YourStrong@Pass1",
		},
		{
			name:         "url_format_override_password_with_at",
			uri:          "sqlserver://sa:original@localhost:1433?database=openfga",
			password:     "new@pass:word",
			wantUser:     "sa",
			wantPassword: "new@pass:word",
		},
		{
			name:         "url_format_mixed_case_scheme",
			uri:          "SqlServer://sa:original@localhost:1433?database=openfga",
			password:     "override-pass",
			wantUser:     "sa",
			wantPassword: "override-pass",
		},
		{
			name:     "odbc_format_unsupported",
			uri:      "odbc:server=localhost;database=openfga",
			username: "sa",
			wantErr:  ErrCredentialOverridesUnsupportedFormat,
		},
		{
			name:     "odbc_format_mixed_case_unsupported",
			uri:      "ODBC:server=localhost;database=openfga",
			username: "sa",
			wantErr:  ErrCredentialOverridesUnsupportedFormat,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ApplyCredentials(tt.uri, tt.username, tt.password)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)

			parsed, err := msdsn.Parse(got)
			require.NoError(t, err)
			require.Equal(t, tt.wantUser, parsed.User)
			require.Equal(t, tt.wantPassword, parsed.Password)
			require.Equal(t, "openfga", parsed.Database)
		})
	}
}

func TestHandleSQLError(t *testing.T) {
	t.Run("no_rows_maps_to_not_found", func(t *testing.T) {
		require.ErrorIs(t, HandleSQLError(sql.ErrNoRows), storage.ErrNotFound)
	})

	t.Run("duplicate_key_maps_to_collision", func(t *testing.T) {
		err := HandleSQLError(mssql.Error{Number: 2627, Message: "duplicate key"})
		require.ErrorIs(t, err, storage.ErrCollision)
	})

	t.Run("duplicate_key_with_tuple_key_maps_to_invalid_write_input", func(t *testing.T) {
		tk := &openfgav1.TupleKey{Object: "document:1", Relation: "viewer", User: "user:anne"}
		err := HandleSQLError(mssql.Error{Number: 2601, Message: "duplicate key"}, tk)
		require.ErrorIs(t, err, storage.ErrInvalidWriteInput)
		require.NotErrorIs(t, err, storage.ErrCollision)
	})

	t.Run("deadlock_not_mapped_on_read_paths", func(t *testing.T) {
		err := HandleSQLError(mssql.Error{Number: 1205, Message: "deadlock victim"})
		require.NotErrorIs(t, err, storage.ErrTransactionalWriteFailed)
		require.ErrorContains(t, err, "sql error")
	})

	t.Run("throttling_errors_map_to_transaction_throttled", func(t *testing.T) {
		for _, number := range []int32{10928, 10929, 40501, 49918, 49919, 49920} {
			err := HandleSQLError(mssql.Error{Number: number, Message: "throttled"})
			require.ErrorIs(t, err, storage.ErrTransactionThrottled, "error number %d", number)

			var mssqlErr mssql.Error
			require.ErrorAs(t, err, &mssqlErr, "error number %d", number)
			require.Equal(t, number, mssqlErr.Number)
		}
	})

	t.Run("availability_errors_not_mapped_to_throttled", func(t *testing.T) {
		// Failover/reconfiguration errors are transient but not throttling;
		// a 429 would be semantically wrong, so they stay generic.
		for _, number := range []int32{4060, 40143, 40197, 40613} {
			err := HandleSQLError(mssql.Error{Number: number, Message: "unavailable"})
			require.NotErrorIs(t, err, storage.ErrTransactionThrottled, "error number %d", number)
			require.ErrorContains(t, err, "sql error", "error number %d", number)
		}
	})

	t.Run("other_errors_preserve_original_error", func(t *testing.T) {
		original := mssql.Error{Number: 40613, Message: "database unavailable"}
		err := HandleSQLError(original)
		require.ErrorContains(t, err, "sql error")
		var mssqlErr mssql.Error
		require.ErrorAs(t, err, &mssqlErr)
		require.Equal(t, int32(40613), mssqlErr.Number)
	})

	t.Run("other_errors_wrapped_generically", func(t *testing.T) {
		err := HandleSQLError(mssql.Error{Number: 547, Message: "constraint violation"})
		require.NotErrorIs(t, err, storage.ErrTransactionalWriteFailed)
		require.NotErrorIs(t, err, storage.ErrTransactionThrottled)
		require.ErrorContains(t, err, "sql error")
	})
}

func TestHandleWriteSQLError(t *testing.T) {
	t.Run("deadlock_maps_to_transactional_write_failed", func(t *testing.T) {
		err := handleWriteSQLError(mssql.Error{Number: 1205, Message: "deadlock victim"})
		require.ErrorIs(t, err, storage.ErrTransactionalWriteFailed)
	})

	t.Run("non_deadlock_errors_delegate_to_HandleSQLError", func(t *testing.T) {
		require.ErrorIs(t, handleWriteSQLError(sql.ErrNoRows), storage.ErrNotFound)

		err := handleWriteSQLError(mssql.Error{Number: 2627, Message: "duplicate key"})
		require.ErrorIs(t, err, storage.ErrCollision)

		err = handleWriteSQLError(mssql.Error{Number: 10928, Message: "resource limit reached"})
		require.ErrorIs(t, err, storage.ErrTransactionThrottled)

		err = handleWriteSQLError(mssql.Error{Number: 40613, Message: "database unavailable"})
		require.ErrorContains(t, err, "sql error")
	})
}
