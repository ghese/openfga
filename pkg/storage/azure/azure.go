package azure

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/denisenkom/go-mssqldb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/logger"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	tupleUtils "github.com/openfga/openfga/pkg/tuple"
)

var tracer = otel.Tracer("openfga/pkg/storage/azure")

func startTrace(ctx context.Context, name string) (context.Context, trace.Span) {
	return tracer.Start(ctx, "azure."+name)
}

type Datastore struct {
	stbl                   sq.StatementBuilderType
	db                     *sql.DB
	dbInfo                 *sqlcommon.DBInfo
	logger                 logger.Logger
	dbStatsCollector       prometheus.Collector
	maxTuplesPerWriteField int
	maxTypesPerModelField  int
	versionReady           bool
}

var _ storage.OpenFGADatastore = (*Datastore)(nil)

func New(uri string, cfg *sqlcommon.Config) (*Datastore, error) {
	db, err := sql.Open("sqlserver", uri)
	if err != nil {
		return nil, fmt.Errorf("initialize azure sql connection: %w", err)
	}
	return NewWithDB(db, cfg)
}

func NewWithDB(db *sql.DB, cfg *sqlcommon.Config) (*Datastore, error) {
	if cfg.MaxIdleConns != 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.PingTimeout)
	defer cancel()

	err := db.PingContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	var collector prometheus.Collector
	if cfg.ExportMetrics {
		collector = collectors.NewDBStatsCollector(db, "openfga")
		if err := prometheus.Register(collector); err != nil {
			return nil, fmt.Errorf("initialize metrics: %w", err)
		}
	}

	stbl := sq.StatementBuilder.RunWith(db)
	dbInfo := sqlcommon.NewDBInfo(stbl, HandleSQLError, "sqlserver")

	return &Datastore{
		stbl:                   stbl,
		db:                     db,
		dbInfo:                 dbInfo,
		logger:                 cfg.Logger,
		dbStatsCollector:       collector,
		maxTuplesPerWriteField: cfg.MaxTuplesPerWriteField,
		maxTypesPerModelField:  cfg.MaxTypesPerModelField,
		versionReady:           false,
	}, nil
}

func (s *Datastore) Close() {
	if s.dbStatsCollector != nil {
		prometheus.Unregister(s.dbStatsCollector)
	}
	s.db.Close()
}

func (s *Datastore) Read(
	ctx context.Context,
	store string,
	filter storage.ReadFilter,
	_ storage.ReadOptions,
) (storage.TupleIterator, error) {
	ctx, span := startTrace(ctx, "Read")
	defer span.End()

	return s.read(ctx, store, filter, nil)
}

func (s *Datastore) ReadPage(ctx context.Context, store string, filter storage.ReadFilter, options storage.ReadPageOptions) ([]*openfgav1.Tuple, string, error) {
	ctx, span := startTrace(ctx, "ReadPage")
	defer span.End()

	iter, err := s.read(ctx, store, filter, &options)
	if err != nil {
		return nil, "", err
	}
	defer iter.Stop()

	return iter.ToArray(ctx, options.Pagination)
}

func (s *Datastore) read(ctx context.Context, store string, filter storage.ReadFilter, options *storage.ReadPageOptions) (*sqlcommon.SQLTupleIterator, error) {
	_, span := startTrace(ctx, "read")
	defer span.End()

	sb := s.stbl.
		Select(
			"store", "object_type", "object_id", "relation",
			"_user",
			"condition_name", "condition_context", "ulid", "inserted_at",
		).
		From("tuple").
		Where(sq.Eq{"store": store})
	if options != nil {
		sb = sb.OrderBy("ulid")
	}

	objectType, objectID := tupleUtils.SplitObject(filter.Object)
	if objectType != "" {
		sb = sb.Where(sq.Eq{"object_type": objectType})
	}
	if objectID != "" {
		sb = sb.Where(sq.Eq{"object_id": objectID})
	}
	if filter.Relation != "" {
		sb = sb.Where(sq.Eq{"relation": filter.Relation})
	}
	if filter.User != "" {
		userType, userID, _ := tupleUtils.ToUserParts(filter.User)
		if userID != "" {
			sb = sb.Where(sq.Eq{"_user": filter.User})
		} else {
			sb = sb.Where(sq.Like{"_user": userType + ":%"})
		}
	}

	if len(filter.Conditions) > 0 {
		sb = sb.Where(sq.Eq{"COALESCE(condition_name, '')": filter.Conditions})
	}

	if options != nil && options.Pagination.From != "" {
		token := options.Pagination.From
		sb = sb.Where(sq.GtOrEq{"ulid": token})
	}
	if options != nil && options.Pagination.PageSize != 0 {
		sb = sb.Limit(uint64(options.Pagination.PageSize + 1))
	}

	return sqlcommon.NewSQLTupleIterator(sqlcommon.NewSBIteratorQuery(sb), HandleSQLError), nil
}

func (s *Datastore) Write(
	ctx context.Context,
	store string,
	deletes storage.Deletes,
	writes storage.Writes,
	opts ...storage.TupleWriteOption,
) error {
	ctx, span := startTrace(ctx, "Write")
	defer span.End()

	return s.write(ctx, store, deletes, writes, storage.NewTupleWriteOptions(opts...), time.Now().UTC())
}

type tupleLockKey struct {
	objectType string
	objectID   string
	relation   string
	user       string
	userType   string
}

func makeTupleLockKeys(deletes storage.Deletes, writes storage.Writes) []tupleLockKey {
	keys := make([]tupleLockKey, 0, len(deletes)+len(writes))

	seen := make(map[string]struct{}, cap(keys))
	add := func(object, relation, user string) {
		objectType, objectID := tupleUtils.SplitObject(object)
		k := tupleLockKey{
			objectType: objectType,
			objectID:   objectID,
			relation:   relation,
			user:       user,
			userType:   string(tupleUtils.GetUserTypeFromUser(user)),
		}
		s := strings.Join([]string{
			k.objectType,
			k.objectID,
			k.relation,
			k.user,
			k.userType,
		}, "\x00")
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		keys = append(keys, k)
	}

	for _, tk := range deletes {
		add(tk.GetObject(), tk.GetRelation(), tk.GetUser())
	}
	for _, tk := range writes {
		add(tk.GetObject(), tk.GetRelation(), tk.GetUser())
	}

	return keys
}

func buildRowConstructorIN(keys []tupleLockKey) (string, []interface{}) {
	var sb strings.Builder
	args := make([]interface{}, 0, len(keys)*5)
	sb.WriteString("(")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?)")
		args = append(args,
			k.objectType,
			k.objectID,
			k.relation,
			k.user,
			k.userType,
		)
	}
	sb.WriteString(")")
	return sb.String(), args
}

func (s *Datastore) selectExistingRowsForWrite(ctx context.Context, store string, keys []tupleLockKey, txn *sql.Tx, existing map[string]*openfgav1.Tuple) error {
	inExpr, args := buildRowConstructorIN(keys)

	selectBuilder := s.stbl.
		Select(sqlcommon.SQLIteratorColumns()...).
		From("tuple WITH (UPDLOCK, ROWLOCK)").
		Where(sq.Eq{"store": store}).
		Where(sq.Expr("(object_type, object_id, relation, _user, user_type) IN "+inExpr, args...)).
		RunWith(txn)

	iter := sqlcommon.NewSQLTupleIterator(sqlcommon.NewSBIteratorQuery(selectBuilder), HandleSQLError)
	defer iter.Stop()

	items, _, err := iter.ToArray(ctx, storage.PaginationOptions{PageSize: len(keys)})
	if err != nil {
		return err
	}
	for _, tuple := range items {
		existing[tupleUtils.TupleKeyToString(tuple.GetKey())] = tuple
	}
	return nil
}

func (s *Datastore) write(
	ctx context.Context,
	store string,
	deletes storage.Deletes,
	writes storage.Writes,
	opts storage.TupleWriteOptions,
	now time.Time,
) error {
	txn, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return HandleSQLError(err)
	}
	defer func() { _ = txn.Rollback() }()

	lockKeys := makeTupleLockKeys(deletes, writes)
	total := len(lockKeys)
	if total == 0 {
		return nil
	}

	existing := make(map[string]*openfgav1.Tuple, total)

	for start := 0; start < total; start += storage.DefaultMaxTuplesPerWrite {
		end := start + storage.DefaultMaxTuplesPerWrite
		if end > total {
			end = total
		}
		keys := lockKeys[start:end]

		if err := s.selectExistingRowsForWrite(ctx, store, keys, txn, existing); err != nil {
			return err
		}
	}

	deleteConditions, writeItems, changeLogItems, err := sqlcommon.GetDeleteWriteChangelogItems(store, existing,
		sqlcommon.WriteData{
			Deletes:       deletes,
			Writes:        writes,
			Opts:          opts,
			Now:           now,
			TimestampExpr: "SYSDATETIME()",
		})
	if err != nil {
		return err
	}

	for start, totalDeletes := 0, len(deleteConditions); start < totalDeletes; start += storage.DefaultMaxTuplesPerWrite {
		end := start + storage.DefaultMaxTuplesPerWrite
		if end > totalDeletes {
			end = totalDeletes
		}

		deleteConditionsBatch := deleteConditions[start:end]

		res, err := s.stbl.Delete("tuple").Where(sq.Eq{"store": store}).
			Where(deleteConditionsBatch).
			RunWith(txn).
			ExecContext(ctx)
		if err != nil {
			return HandleSQLError(err)
		}

		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return HandleSQLError(err)
		}

		if rowsAffected != int64(len(deleteConditionsBatch)) {
			return storage.ErrWriteConflictOnDelete
		}
	}

	for start, totalWrites := 0, len(writeItems); start < totalWrites; start += storage.DefaultMaxTuplesPerWrite {
		end := start + storage.DefaultMaxTuplesPerWrite
		if end > totalWrites {
			end = totalWrites
		}

		writesBatch := writeItems[start:end]

		insertBuilder := s.stbl.
			Insert("tuple").
			Columns(
				"store",
				"object_type",
				"object_id",
				"relation",
				"_user",
				"user_type",
				"condition_name",
				"condition_context",
				"ulid",
				"inserted_at",
			)

		for _, item := range writesBatch {
			insertBuilder = insertBuilder.Values(item...)
		}

		_, err = insertBuilder.
			RunWith(txn).
			ExecContext(ctx)
		if err != nil {
			dberr := HandleSQLError(err)
			if errors.Is(dberr, storage.ErrCollision) {
				return storage.ErrWriteConflictOnInsert
			}
			return dberr
		}
	}

	for start, totalItems := 0, len(changeLogItems); start < totalItems; start += storage.DefaultMaxTuplesPerWrite {
		end := start + storage.DefaultMaxTuplesPerWrite
		if end > totalItems {
			end = totalItems
		}

		changeLogBatch := changeLogItems[start:end]

		changelogBuilder := s.stbl.
			Insert("changelog").
			Columns(
				"store",
				"object_type",
				"object_id",
				"relation",
				"_user",
				"condition_name",
				"condition_context",
				"operation",
				"ulid",
				"inserted_at",
			)

		for _, item := range changeLogBatch {
			changelogBuilder = changelogBuilder.Values(item...)
		}

		_, err = changelogBuilder.RunWith(txn).ExecContext(ctx)
		if err != nil {
			return HandleSQLError(err)
		}
	}

	if err := txn.Commit(); err != nil {
		return HandleSQLError(err)
	}

	return nil
}

func (s *Datastore) ReadUserTuple(ctx context.Context, store string, filter storage.ReadUserTupleFilter, _ storage.ReadUserTupleOptions) (*openfgav1.Tuple, error) {
	ctx, span := startTrace(ctx, "ReadUserTuple")
	defer span.End()

	objectType, objectID := tupleUtils.SplitObject(filter.Object)
	userType := tupleUtils.GetUserTypeFromUser(filter.User)

	var conditionName sql.NullString
	var conditionContext []byte
	var record storage.TupleRecord

	sb := s.stbl.
		Select(
			"object_type", "object_id", "relation",
			"_user",
			"condition_name", "condition_context",
		).
		From("tuple").
		Where(sq.Eq{
			"store":       store,
			"object_type": objectType,
			"object_id":   objectID,
			"relation":    filter.Relation,
			"_user":       filter.User,
			"user_type":   userType,
		})

	if len(filter.Conditions) > 0 {
		sb = sb.Where(sq.Eq{"COALESCE(condition_name, '')": filter.Conditions})
	}

	err := sb.QueryRowContext(ctx).
		Scan(
			&record.ObjectType,
			&record.ObjectID,
			&record.Relation,
			&record.User,
			&conditionName,
			&conditionContext,
		)
	if err != nil {
		return nil, HandleSQLError(err)
	}

	if conditionName.String != "" {
		record.ConditionName = conditionName.String

		if conditionContext != nil {
			var conditionContextStruct structpb.Struct
			if err := proto.Unmarshal(conditionContext, &conditionContextStruct); err != nil {
				return nil, err
			}
			record.ConditionContext = &conditionContextStruct
		}
	}

	return record.AsTuple(), nil
}

func (s *Datastore) ReadUsersetTuples(
	ctx context.Context,
	store string,
	filter storage.ReadUsersetTuplesFilter,
	_ storage.ReadUsersetTuplesOptions,
) (storage.TupleIterator, error) {
	_, span := startTrace(ctx, "ReadUsersetTuples")
	defer span.End()

	sb := s.stbl.
		Select(
			"store", "object_type", "object_id", "relation",
			"_user",
			"condition_name", "condition_context", "ulid", "inserted_at",
		).
		From("tuple").
		Where(sq.Eq{"store": store}).
		Where(sq.Eq{"user_type": tupleUtils.UserSet})

	objectType, objectID := tupleUtils.SplitObject(filter.Object)
	if objectType != "" {
		sb = sb.Where(sq.Eq{"object_type": objectType})
	}
	if objectID != "" {
		sb = sb.Where(sq.Eq{"object_id": objectID})
	}
	if filter.Relation != "" {
		sb = sb.Where(sq.Eq{"relation": filter.Relation})
	}
	if len(filter.AllowedUserTypeRestrictions) > 0 {
		orConditions := sq.Or{}
		for _, userset := range filter.AllowedUserTypeRestrictions {
			if _, ok := userset.GetRelationOrWildcard().(*openfgav1.RelationReference_Relation); ok {
				orConditions = append(orConditions, sq.Like{
					"_user": userset.GetType() + ":%#" + userset.GetRelation(),
				})
			}
			if _, ok := userset.GetRelationOrWildcard().(*openfgav1.RelationReference_Wildcard); ok {
				orConditions = append(orConditions, sq.Eq{
					"_user": userset.GetType() + ":*",
				})
			}
		}
		sb = sb.Where(orConditions)
	}
	if len(filter.Conditions) > 0 {
		sb = sb.Where(sq.Eq{"COALESCE(condition_name, '')": filter.Conditions})
	}

	return sqlcommon.NewSQLTupleIterator(sqlcommon.NewSBIteratorQuery(sb), HandleSQLError), nil
}

func (s *Datastore) ReadStartingWithUser(
	ctx context.Context,
	store string,
	filter storage.ReadStartingWithUserFilter,
	_ storage.ReadStartingWithUserOptions,
) (storage.TupleIterator, error) {
	_, span := startTrace(ctx, "ReadStartingWithUser")
	defer span.End()

	var targetUsersArg []string
	for _, u := range filter.UserFilter {
		targetUser := u.GetObject()
		if u.GetRelation() != "" {
			targetUser = strings.Join([]string{u.GetObject(), u.GetRelation()}, "#")
		}
		targetUsersArg = append(targetUsersArg, targetUser)
	}

	builder := s.stbl.
		Select(
			"store", "object_type", "object_id", "relation",
			"_user",
			"condition_name", "condition_context", "ulid", "inserted_at",
		).
		From("tuple").
		Where(sq.Eq{
			"store":       store,
			"object_type": filter.ObjectType,
			"relation":    filter.Relation,
			"_user":       targetUsersArg,
		}).OrderBy("object_id")

	if filter.ObjectIDs != nil && filter.ObjectIDs.Size() > 0 {
		builder = builder.Where(sq.Eq{"object_id": filter.ObjectIDs.Values()})
	}
	if len(filter.Conditions) > 0 {
		builder = builder.Where(sq.Eq{"COALESCE(condition_name, '')": filter.Conditions})
	}
	return sqlcommon.NewSQLTupleIterator(sqlcommon.NewSBIteratorQuery(builder), HandleSQLError), nil
}

func (s *Datastore) MaxTuplesPerWrite() int {
	return s.maxTuplesPerWriteField
}

func (s *Datastore) ReadAuthorizationModel(ctx context.Context, store string, modelID string) (*openfgav1.AuthorizationModel, error) {
	ctx, span := startTrace(ctx, "ReadAuthorizationModel")
	defer span.End()

	return sqlcommon.ReadAuthorizationModel(ctx, s.dbInfo, store, modelID)
}

func (s *Datastore) ReadAuthorizationModels(ctx context.Context, store string, options storage.ReadAuthorizationModelsOptions) ([]*openfgav1.AuthorizationModel, string, error) {
	ctx, span := startTrace(ctx, "ReadAuthorizationModels")
	defer span.End()

	sb := s.stbl.
		Select("authorization_model_id").
		Distinct().
		From("authorization_model").
		Where(sq.Eq{"store": store}).
		OrderBy("authorization_model_id desc")

	if options.Pagination.From != "" {
		token := options.Pagination.From
		sb = sb.Where(sq.LtOrEq{"authorization_model_id": token})
	}
	if options.Pagination.PageSize > 0 {
		sb = sb.Limit(uint64(options.Pagination.PageSize + 1))
	}

	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, "", HandleSQLError(err)
	}
	defer rows.Close()

	var modelIDs []string
	var modelID string

	for rows.Next() {
		err = rows.Scan(&modelID)
		if err != nil {
			return nil, "", HandleSQLError(err)
		}

		modelIDs = append(modelIDs, modelID)
	}

	if err := rows.Err(); err != nil {
		return nil, "", HandleSQLError(err)
	}

	var token string
	numModelIDs := len(modelIDs)
	if len(modelIDs) > options.Pagination.PageSize {
		numModelIDs = options.Pagination.PageSize
		token = modelID
	}

	models := make([]*openfgav1.AuthorizationModel, 0, numModelIDs)
	for i := 0; i < numModelIDs; i++ {
		model, err := s.ReadAuthorizationModel(ctx, store, modelIDs[i])
		if err != nil {
			return nil, "", err
		}
		models = append(models, model)
	}

	return models, token, nil
}

func (s *Datastore) FindLatestAuthorizationModel(ctx context.Context, store string) (*openfgav1.AuthorizationModel, error) {
	ctx, span := startTrace(ctx, "FindLatestAuthorizationModel")
	defer span.End()

	return sqlcommon.FindLatestAuthorizationModel(ctx, s.dbInfo, store)
}

func (s *Datastore) MaxTypesPerAuthorizationModel() int {
	return s.maxTypesPerModelField
}

func (s *Datastore) WriteAuthorizationModel(ctx context.Context, store string, model *openfgav1.AuthorizationModel) error {
	ctx, span := startTrace(ctx, "WriteAuthorizationModel")
	defer span.End()

	return sqlcommon.WriteAuthorizationModel(ctx, s.dbInfo, store, model)
}

func (s *Datastore) CreateStore(ctx context.Context, store *openfgav1.Store) (*openfgav1.Store, error) {
	ctx, span := startTrace(ctx, "CreateStore")
	defer span.End()

	var id, name string
	var createdAt, updatedAt time.Time

	txn, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, HandleSQLError(err)
	}
	defer func() {
		_ = txn.Rollback()
	}()

	_, err = s.stbl.
		Insert("store").
		Columns("id", "name", "created_at", "updated_at").
		Values(store.GetId(), store.GetName(), sq.Expr("SYSDATETIME()"), sq.Expr("SYSDATETIME()")).
		RunWith(txn).
		ExecContext(ctx)
	if err != nil {
		return nil, HandleSQLError(err)
	}

	err = s.stbl.
		Select("id", "name", "created_at", "updated_at").
		From("store").
		Where(sq.Eq{"id": store.GetId()}).
		RunWith(txn).
		QueryRowContext(ctx).
		Scan(&id, &name, &createdAt, &updatedAt)
	if err != nil {
		return nil, HandleSQLError(err)
	}

	err = txn.Commit()
	if err != nil {
		return nil, HandleSQLError(err)
	}

	return &openfgav1.Store{
		Id:        id,
		Name:      name,
		CreatedAt: timestamppb.New(createdAt),
		UpdatedAt: timestamppb.New(updatedAt),
	}, nil
}

func (s *Datastore) GetStore(ctx context.Context, id string) (*openfgav1.Store, error) {
	ctx, span := startTrace(ctx, "GetStore")
	defer span.End()

	row := s.stbl.
		Select("id", "name", "created_at", "updated_at").
		From("store").
		Where(sq.Eq{
			"id":         id,
			"deleted_at": nil,
		}).
		QueryRowContext(ctx)

	var storeID, name string
	var createdAt, updatedAt time.Time
	err := row.Scan(&storeID, &name, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, storage.ErrNotFound
		}
		return nil, HandleSQLError(err)
	}

	return &openfgav1.Store{
		Id:        storeID,
		Name:      name,
		CreatedAt: timestamppb.New(createdAt),
		UpdatedAt: timestamppb.New(updatedAt),
	}, nil
}

func (s *Datastore) ListStores(ctx context.Context, options storage.ListStoresOptions) ([]*openfgav1.Store, string, error) {
	ctx, span := startTrace(ctx, "ListStores")
	defer span.End()

	whereClause := sq.And{
		sq.Eq{"deleted_at": nil},
	}

	if len(options.IDs) > 0 {
		whereClause = append(whereClause, sq.Eq{"id": options.IDs})
	}

	if options.Name != "" {
		whereClause = append(whereClause, sq.Eq{"name": options.Name})
	}

	if options.Pagination.From != "" {
		whereClause = append(whereClause, sq.GtOrEq{"id": options.Pagination.From})
	}

	sb := s.stbl.
		Select("id", "name", "created_at", "updated_at").
		From("store").
		Where(whereClause).
		OrderBy("id")

	if options.Pagination.PageSize > 0 {
		sb = sb.Limit(uint64(options.Pagination.PageSize + 1))
	}

	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, "", HandleSQLError(err)
	}
	defer rows.Close()

	var stores []*openfgav1.Store
	var id string
	for rows.Next() {
		var name string
		var createdAt, updatedAt time.Time
		err := rows.Scan(&id, &name, &createdAt, &updatedAt)
		if err != nil {
			return nil, "", HandleSQLError(err)
		}

		stores = append(stores, &openfgav1.Store{
			Id:        id,
			Name:      name,
			CreatedAt: timestamppb.New(createdAt),
			UpdatedAt: timestamppb.New(updatedAt),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, "", HandleSQLError(err)
	}

	if len(stores) > options.Pagination.PageSize {
		return stores[:options.Pagination.PageSize], id, nil
	}

	return stores, "", nil
}

func (s *Datastore) DeleteStore(ctx context.Context, id string) error {
	ctx, span := startTrace(ctx, "DeleteStore")
	defer span.End()

	_, err := s.stbl.
		Update("store").
		Set("deleted_at", sq.Expr("SYSDATETIME()")).
		Where(sq.Eq{"id": id}).
		ExecContext(ctx)
	if err != nil {
		return HandleSQLError(err)
	}

	return nil
}

func (s *Datastore) WriteAssertions(ctx context.Context, store, modelID string, assertions []*openfgav1.Assertion) error {
	ctx, span := startTrace(ctx, "WriteAssertions")
	defer span.End()

	marshalledAssertions, err := proto.Marshal(&openfgav1.Assertions{Assertions: assertions})
	if err != nil {
		return err
	}

	mergeStmt := `
		MERGE assertion WITH (HOLDLOCK) AS target
		USING (SELECT @p1 AS store, @p2 AS authorization_model_id, @p3 AS assertions) AS source
		ON target.store = source.store AND target.authorization_model_id = source.authorization_model_id
		WHEN MATCHED THEN UPDATE SET assertions = source.assertions
		WHEN NOT MATCHED THEN INSERT (store, authorization_model_id, assertions)
		VALUES (source.store, source.authorization_model_id, source.assertions);`

	_, err = s.db.ExecContext(ctx, mergeStmt,
		sql.Named("p1", store),
		sql.Named("p2", modelID),
		sql.Named("p3", marshalledAssertions),
	)
	if err != nil {
		return HandleSQLError(err)
	}

	return nil
}

func (s *Datastore) ReadAssertions(ctx context.Context, store, modelID string) ([]*openfgav1.Assertion, error) {
	ctx, span := startTrace(ctx, "ReadAssertions")
	defer span.End()

	var marshalledAssertions []byte
	err := s.stbl.
		Select("assertions").
		From("assertion").
		Where(sq.Eq{
			"store":                  store,
			"authorization_model_id": modelID,
		}).
		QueryRowContext(ctx).
		Scan(&marshalledAssertions)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*openfgav1.Assertion{}, nil
		}
		return nil, HandleSQLError(err)
	}

	var assertions openfgav1.Assertions
	err = proto.Unmarshal(marshalledAssertions, &assertions)
	if err != nil {
		return nil, err
	}

	return assertions.GetAssertions(), nil
}

func (s *Datastore) ReadChanges(ctx context.Context, store string, filter storage.ReadChangesFilter, options storage.ReadChangesOptions) ([]*openfgav1.TupleChange, string, error) {
	ctx, span := startTrace(ctx, "ReadChanges")
	defer span.End()

	objectTypeFilter := filter.ObjectType
	horizonOffset := filter.HorizonOffset

	orderBy := "ulid asc"
	if options.SortDesc {
		orderBy = "ulid desc"
	}

	sb := s.stbl.
		Select(
			"ulid", "object_type", "object_id", "relation",
			"_user",
			"operation",
			"condition_name", "condition_context", "inserted_at",
		).
		From("changelog").
		Where(sq.Eq{"store": store}).
		Where(fmt.Sprintf("inserted_at <= DATEADD(MICROSECOND, %d, SYSDATETIME())", -horizonOffset.Microseconds())).
		OrderBy(orderBy)

	if objectTypeFilter != "" {
		sb = sb.Where(sq.Eq{"object_type": objectTypeFilter})
	}
	if options.Pagination.From != "" {
		sb = sqlcommon.AddFromUlid(sb, options.Pagination.From, options.SortDesc)
	}
	if options.Pagination.PageSize > 0 {
		sb = sb.Limit(uint64(options.Pagination.PageSize))
	}

	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, "", HandleSQLError(err)
	}
	defer rows.Close()

	var changes []*openfgav1.TupleChange
	var ulid string
	for rows.Next() {
		var objectType, objectID, relation, user string
		var operation int
		var insertedAt time.Time
		var conditionName sql.NullString
		var conditionContext []byte

		err = rows.Scan(
			&ulid,
			&objectType,
			&objectID,
			&relation,
			&user,
			&operation,
			&conditionName,
			&conditionContext,
			&insertedAt,
		)
		if err != nil {
			return nil, "", HandleSQLError(err)
		}

		var conditionContextStruct structpb.Struct
		if conditionName.String != "" {
			if conditionContext != nil {
				if err := proto.Unmarshal(conditionContext, &conditionContextStruct); err != nil {
					return nil, "", err
				}
			}
		}

		tk := tupleUtils.NewTupleKeyWithCondition(
			tupleUtils.BuildObject(objectType, objectID),
			relation,
			user,
			conditionName.String,
			&conditionContextStruct,
		)

		changes = append(changes, &openfgav1.TupleChange{
			TupleKey:  tk,
			Operation: openfgav1.TupleOperation(operation),
			Timestamp: timestamppb.New(insertedAt.UTC()),
		})
	}

	if len(changes) == 0 {
		return nil, "", storage.ErrNotFound
	}

	return changes, ulid, nil
}

func (s *Datastore) IsReady(ctx context.Context) (storage.ReadinessStatus, error) {
	versionReady, err := sqlcommon.IsReady(ctx, s.versionReady, s.db)
	if err != nil {
		return versionReady, err
	}

	s.versionReady = versionReady.IsReady
	return versionReady, nil
}

func HandleSQLError(err error, args ...interface{}) error {
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ErrNotFound
	}

	var mssqlErr mssql.Error
	if errors.As(err, &mssqlErr) {
		if mssqlErr.Number == 2627 || mssqlErr.Number == 2601 {
			if len(args) > 0 {
				if tk, ok := args[0].(*openfgav1.TupleKey); ok {
					return storage.InvalidWriteInputError(tk, openfgav1.TupleOperation_TUPLE_OPERATION_WRITE)
				}
			}
			return storage.ErrCollision
		}
	}

	return fmt.Errorf("sql error: %w", err)
}
