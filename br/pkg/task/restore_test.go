// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package task_test

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/go-units"
	"github.com/gogo/protobuf/proto"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/encryptionpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/tidb/br/pkg/backup"
	"github.com/pingcap/tidb/br/pkg/checkpoint"
	"github.com/pingcap/tidb/br/pkg/gluetidb"
	gluemock "github.com/pingcap/tidb/br/pkg/gluetidb/mock"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/mock"
	"github.com/pingcap/tidb/br/pkg/restore/split"
	"github.com/pingcap/tidb/br/pkg/restore/tiflashrec"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/br/pkg/task"
	"github.com/pingcap/tidb/br/pkg/utils"
	utiltest "github.com/pingcap/tidb/br/pkg/utiltest"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/meta/model"
	pmodel "github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/store/mockstore"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/types"
	filter "github.com/pingcap/tidb/pkg/util/table-filter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/oracle"
	pdhttp "github.com/tikv/pd/client/http"
)

const pb uint64 = units.PB

func recordedMaterializedViewSchemas(tables []*metautil.Table) *stream.SchemasReplace {
	dbs := make(map[int64]*stream.DBReplace)
	for _, table := range tables {
		db := dbs[table.DB.ID]
		if db == nil {
			db = stream.NewDBReplace(table.DB.Name.O, table.DB.ID)
			dbs[table.DB.ID] = db
		}
		db.TableMap[table.Info.ID] = stream.NewTableReplace(table.Info.Name.O, table.Info.ID)
	}
	sr := stream.NewSchemasReplace(dbs, false, nil, 0, filter.All(), nil, nil, nil)
	for _, table := range tables {
		sr.MaterializedViewRecorder.Seed(table.DB.ID, table.Info)
	}
	return sr
}

func TestRestoreMaterializedViews(t *testing.T) {
	ctx := context.Background()
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table mv_base (a int primary key, b int not null)")
	tk.MustExec("insert into mv_base values (1, 10), (2, 20)")
	tk.MustExec("create materialized view log on mv_base (a,b) purge next now() + interval 1 hour")
	tk.MustExec("create materialized view mv (a,s,cnt) refresh fast next now() + interval 1 hour as select a,sum(b),count(*) from mv_base group by a")
	tk.MustExec("alter table mv add index idx_s(s) invisible")
	db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, ok)
	infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
	require.NoError(t, err)
	var tables []*metautil.Table
	for _, info := range infos {
		tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
	}
	tk.MustExec("drop materialized view mv")
	tk.MustExec("drop materialized view log on mv_base")
	g := gluetidb.New()
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, g, dom, tables))
	tk.MustQuery("select * from mv order by a").Check(testkit.Rows("1 10 1", "2 20 1"))
	tk.MustQuery("select INDEX_NAME, IS_VISIBLE from information_schema.statistics where TABLE_SCHEMA='test' and TABLE_NAME='mv' and INDEX_NAME='idx_s'").Check(testkit.Rows("idx_s NO"))
	tk.MustQuery("select count(*) from mysql.tidb_mview_refresh_info where NEXT_REFRESH_UNIX_SECONDS is not null").Check(testkit.Rows("1"))
	tk.MustQuery("select count(*) from mysql.tidb_mlog_purge_info where NEXT_PURGE_UNIX_SECONDS is not null").Check(testkit.Rows("1"))
	objectIDs := make(map[string]int64)
	for _, table := range tables {
		actual, err := dom.InfoSchema().TableByName(ctx, db.Name, table.Info.Name)
		require.NoError(t, err)
		objectIDs[table.Info.Name.O] = actual.Meta().ID
	}
	// A retry keeps completed data builds and finishes any missing indexes.
	tk.MustExec("alter table mv drop index idx_s")
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, g, dom, tables))
	for name, id := range objectIDs {
		actual, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr(name))
		require.NoError(t, err)
		require.Equal(t, id, actual.Meta().ID, "completed object %s must be reused", name)
	}
	tk.MustQuery("select INDEX_NAME, IS_VISIBLE from information_schema.statistics where TABLE_SCHEMA='test' and TABLE_NAME='mv' and INDEX_NAME='idx_s'").Check(testkit.Rows("idx_s NO"))
	tk.MustQuery("select NEXT_REFRESH_UNIX_SECONDS is not null from mysql.tidb_mview_refresh_info").Check(testkit.Rows("1"))
	// A partial attempt with only the log completed must not recreate the log.
	tk.MustExec("drop materialized view mv")
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, g, dom, tables))
	for name, id := range objectIDs {
		if name == "mv" {
			continue
		}
		actual, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr(name))
		require.NoError(t, err)
		require.Equal(t, id, actual.Meta().ID)
	}
	mv, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("mv"))
	require.NoError(t, err)
	require.NotEmpty(t, mv.Meta().MaterializedView.RefreshNext)
	tk.MustExec("update mv_base set b=11 where a=1")
	tk.MustExec("delete from mv_base where a=2")
	tk.MustExec("insert into mv_base values (3,30)")
	tk.MustExec("refresh materialized view mv fast")
	tk.MustQuery("select * from mv order by a").Check(testkit.Rows("1 11 1", "3 30 1"))

	// Incomplete source graphs still restore their ordinary base data.
	var backupBase, backupLog, backupMV *metautil.Table
	for _, table := range tables {
		if table.Info.MaterializedViewLog != nil {
			backupLog = table
		} else if table.Info.MaterializedView != nil {
			backupMV = table
		} else {
			backupBase = table
		}
	}
	require.NotNil(t, backupBase)
	require.NotNil(t, backupLog)
	require.NotNil(t, backupMV)
	for _, incomplete := range []struct {
		name    string
		tableID int64
		state   model.SchemaState
		build   model.MViewInitBuildState
	}{
		{"non-public base", backupBase.Info.ID, model.StateWriteOnly, model.MViewInitBuildReady},
		{"non-public log", backupLog.Info.ID, model.StateDeleteOnly, model.MViewInitBuildReady},
		{"non-public MV", backupMV.Info.ID, model.StateNone, model.MViewInitBuildReady},
		{"deferred MV", backupMV.Info.ID, model.StatePublic, model.MViewInitBuildDeferred},
		{"building MV", backupMV.Info.ID, model.StatePublic, model.MViewInitBuildBuilding},
	} {
		t.Run(incomplete.name, func(t *testing.T) {
			input := make([]*metautil.Table, 0, len(tables))
			for _, table := range tables {
				clone := *table
				clone.Info = table.Info.Clone()
				if clone.Info.ID == incomplete.tableID {
					clone.Info.State = incomplete.state
					if clone.Info.MaterializedView != nil {
						clone.Info.MaterializedView.InitBuildState = incomplete.build
					}
				}
				input = append(input, &clone)
			}
			prepared, err := task.PrepareMaterializedViewTablesForTest(input)
			require.NoError(t, err)
			require.Len(t, prepared, 1)
			require.Equal(t, backupBase.Info.ID, prepared[0].Info.ID)
			require.Nil(t, prepared[0].Info.MaterializedViewBase)
			targetStore, targetDom := testkit.CreateMockStoreAndDomain(t)
			targetTK := testkit.NewTestKit(t, targetStore)
			targetTK.MustExec("use test")
			targetSession, err := g.CreateSession(targetStore)
			require.NoError(t, err)
			defer targetSession.Close()
			for _, table := range prepared {
				require.NoError(t, targetSession.CreateTable(ctx, db.Name, table.Info))
			}
			require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, g, targetDom, input))
			// Invalid derived objects are excluded; only their base data is restored.
			targetTK.MustQuery("show tables like 'mv'").Check(testkit.Rows())
			if incomplete.tableID != backupMV.Info.ID {
				targetTK.MustQuery("show tables like '" + backupLog.Info.Name.O + "'").Check(testkit.Rows())
			}
			targetTK.MustExec("insert into mv_base values (1,42)")
			targetTK.MustQuery("select * from mv_base").Check(testkit.Rows("1 42"))
			require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, g, targetDom, input))
			targetTK.MustQuery("show tables like 'mv'").Check(testkit.Rows())
		})
	}
	// A base-only plan must leave the completed target graph untouched.
	base, err := dom.InfoSchema().TableByName(ctx, db.Name, backupBase.Info.Name)
	require.NoError(t, err)
	links := base.Meta().MaterializedViewBase.Clone()
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, g, dom, []*metautil.Table{backupBase}))
	base, err = dom.InfoSchema().TableByName(ctx, db.Name, backupBase.Info.Name)
	require.NoError(t, err)
	require.Equal(t, links, base.Meta().MaterializedViewBase)
}

func TestRepairMaterializedViewsAfterLogRestore(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t, mockstore.WithMockTiFlash(2))
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set sql_mode = 'ANSI_QUOTES,NO_BACKSLASH_ESCAPES'")
	tk.MustExec("set time_zone = '-05:30'")
	tk.MustExec("set div_precision_increment = 6")
	tk.MustExec("create table mv_base (a int primary key, b int not null)")
	tk.MustExec("insert into mv_base values (1,10), (2,20)")
	tk.MustExec("create materialized view log on mv_base (a,b) purge next now() + interval 1 hour")
	tk.MustExec("create materialized view mv (a,s,cnt) refresh fast next now() + interval 1 hour as select a,sum(b),count(*) from mv_base group by a")
	tk.MustExec(`alter table mv add index idx_s(s) comment 'tail\' invisible`)
	tk.MustExec("create materialized view log on mv (a,s,cnt) purge next now() + interval 1 hour")
	tk.MustExec("create materialized view mv_child (a,s,cs,cnt) refresh fast as select a,sum(s),count(s),count(*) from mv group by a")
	tk.MustExec("update mv_base set b=11 where a=1")
	tk.MustExec("delete from mv_base where a=2")
	tk.MustExec("insert into mv_base values (3,30)")
	tk.MustExec("set sql_mode = 'NO_UNSIGNED_SUBTRACTION'")
	tk.MustExec("alter materialized view mv refresh next cast(date_add('2030-01-01', interval (cast(0 as unsigned) - 1 + 2) day) as datetime)")
	tk.MustExec("alter table mv set tiflash replica 1 location labels 'rack', 'host'")
	tk.MustExec("alter table `$mlog$mv_base` set tiflash replica 1 location labels 'rack', 'host'")
	ctx := context.Background()
	cfg := &task.RestoreConfig{Config: task.Config{TableFilter: filter.All()}}
	db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, ok)
	infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
	require.NoError(t, err)
	var tables []*metautil.Table
	for _, info := range infos {
		tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
	}
	sr := recordedMaterializedViewSchemas(tables)
	recorder := sr.MaterializedViewRecorder
	tk.MustExec("set global time_zone = '+02:00'")
	g := gluetidb.New()
	se, err := g.CreateSession(store)
	require.NoError(t, err)
	defer se.Close()
	tk.MustExec("create database " + utils.EncloseName(checkpoint.LogRestoreCheckpointDatabaseName))
	require.False(t, checkpoint.ExistsCheckpointMaterializedViews(dom))
	require.NoError(t, checkpoint.SaveCheckpointMaterializedViews(ctx, se, recorder))
	require.True(t, checkpoint.ExistsCheckpointMaterializedViews(dom))
	loaded, err := checkpoint.LoadCheckpointMaterializedViews(ctx, se.GetSessionCtx().GetRestrictedSQLExecutor())
	require.NoError(t, err)
	expectedJSON, err := json.Marshal(recorder.Tables)
	require.NoError(t, err)
	actualJSON, err := json.Marshal(loaded.Tables)
	require.NoError(t, err)
	require.JSONEq(t, string(expectedJSON), string(actualJSON))
	require.Equal(t, recorder.Skipped, loaded.Skipped)
	sr.MaterializedViewRecorder = loaded
	// Full snapshot restore imports only ordinary base schemas and data. Remove
	// the source derived objects to reproduce that state before log rebuild.
	dropDerivedObjects := func() {
		tk.MustExec("drop materialized view mv_child")
		tk.MustExec("drop materialized view log on mv")
		tk.MustExec("drop materialized view mv")
		tk.MustExec("drop materialized view log on mv_base")
	}
	dropDerivedObjects()
	require.False(t, dom.InfoSchema().TableExists(db.Name, pmodel.NewCIStr("mv")))
	base, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("mv_base"))
	require.NoError(t, err)
	require.Nil(t, base.Meta().MaterializedViewBase)
	tk.MustQuery("select count(*) from mysql.tidb_mview_refresh_info").Check(testkit.Rows("0"))
	tk.MustQuery("select count(*) from mysql.tidb_mlog_purge_info").Check(testkit.Rows("0"))
	// Exporting repair SQL must not create any derived objects. Executing the
	// generated file must restore the cascade, source SQL context and indexes.
	sqlStorage, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	cfg.RepairSQLStorage = sqlStorage
	require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, g, dom, cfg, sr))
	require.False(t, dom.InfoSchema().TableExists(db.Name, pmodel.NewCIStr("mv")))
	script, err := sqlStorage.ReadFile(ctx, "materialized-views.sql")
	require.NoError(t, err)
	sqlText := string(script)
	parentPos := strings.Index(sqlText, `CREATE MATERIALIZED VIEW "mv" `)
	logPos := strings.Index(sqlText, "CREATE MATERIALIZED VIEW LOG ON ")
	require.GreaterOrEqual(t, logPos, 0)
	require.Greater(t, parentPos, logPos)
	require.Contains(t, sqlText, "SET SESSION time_zone = '+02:00'")
	require.Contains(t, sqlText, "SET SESSION time_zone = '-05:30'")
	require.Contains(t, sqlText, "SET SESSION div_precision_increment = 6")
	require.Contains(t, sqlText, "CREATE INDEX IF NOT EXISTS `idx_s`")
	require.Contains(t, sqlText, "ALTER MATERIALIZED VIEW `mv` REFRESH NEXT")
	for _, name := range []string{"mv", "$mlog$mv_base"} {
		require.Contains(t, sqlText, "ALTER TABLE `test`.`"+name+"` SET TIFLASH REPLICA 1 LOCATION LABELS 'rack', 'host'")
	}
	tk.MustExec("set sql_mode = ''")
	tk.MustExec("set time_zone = '+00:00'")
	tk.MustExec("set div_precision_increment = 4")
	for _, sql := range strings.Split(sqlText, ";\n") {
		if strings.TrimSpace(sql) != "" {
			tk.MustExec(sql)
		}
	}
	tk.MustQuery("select a,s,cnt from mv_child order by a").Check(testkit.Rows("1 11 1", "3 30 1"))
	tk.MustQuery("select IS_VISIBLE from information_schema.statistics where TABLE_SCHEMA='test' and TABLE_NAME='mv' and INDEX_NAME='idx_s'").Check(testkit.Rows("NO"))
	rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("mv"))
	require.NoError(t, err)
	require.Equal(t, mysql.ModeANSIQuotes|mysql.ModeNoBackslashEscapes, rebuilt.Meta().MaterializedView.DefinitionSQLMode)
	require.Equal(t, -19800, rebuilt.Meta().MaterializedView.DefinitionTimeZone.Offset)
	require.Equal(t, "tail\\", rebuilt.Meta().FindIndexByName("idx_s").Comment)
	require.Equal(t, mysql.ModeNoUnsignedSubtraction, rebuilt.Meta().MaterializedView.RefreshScheduleSQLMode)
	checkReplicas := func() {
		for _, name := range []string{"mv", "$mlog$mv_base"} {
			actual, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr(name))
			require.NoError(t, err)
			require.NotNil(t, actual.Meta().TiFlashReplica)
			require.EqualValues(t, 1, actual.Meta().TiFlashReplica.Count)
			require.Equal(t, []string{"rack", "host"}, actual.Meta().TiFlashReplica.LocationLabels)
		}
	}
	checkReplicas()
	dropDerivedObjects()
	cfg.RepairSQLStorage = nil
	// Fail while creating the child, after the parent and its log are complete.
	var childID int64
	for id, table := range sr.MaterializedViewRecorder.Tables {
		if table.Info.Name.O == "mv_child" {
			childID = id
		}
	}
	require.NotZero(t, childID)
	child := sr.MaterializedViewRecorder.Tables[childID]
	brokenChild := *child
	brokenChild.Info = child.Info.Clone()
	brokenChild.Info.MaterializedView.SQLContent = "select a,sum(missing_column),count(s),count(*) from mv group by a"
	sr.MaterializedViewRecorder.Tables[childID] = &brokenChild
	require.ErrorContains(t, task.RebuildRecordedMaterializedViewsForTest(ctx, g, dom, cfg, sr), "missing_column")
	parent, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("mv"))
	require.NoError(t, err)
	parentID, parentLogID := parent.Meta().ID, parent.Meta().MaterializedViewBase.MLogID
	// Scheduling is restored with the object, even if a later object cannot be built.
	tk.MustQuery("select NEXT_REFRESH_UNIX_SECONDS is not null from mysql.tidb_mview_refresh_info where MVIEW_ID = ?", parentID).Check(testkit.Rows("1"))
	tk.MustQuery("select NEXT_PURGE_UNIX_SECONDS is not null from mysql.tidb_mlog_purge_info where MLOG_ID = ?", parentLogID).Check(testkit.Rows("1"))
	sr.MaterializedViewRecorder.Tables[childID] = child
	for range 2 {
		// Finish the child without rebuilding completed upstream objects.
		require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, g, dom, cfg, sr))
		parent, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("mv"))
		require.NoError(t, err)
		require.Equal(t, parentID, parent.Meta().ID)
		require.Equal(t, parentLogID, parent.Meta().MaterializedViewBase.MLogID)
		require.Equal(t, mysql.ModeNoUnsignedSubtraction, parent.Meta().MaterializedView.RefreshScheduleSQLMode)
		checkReplicas()
		tk.MustQuery("select * from mv order by a").Check(testkit.Rows("1 11 1", "3 30 1"))
		tk.MustQuery("select a,s,cnt from mv_child order by a").Check(testkit.Rows("1 11 1", "3 30 1"))
	}
	require.NoError(t, checkpoint.RemoveCheckpointDataForLogRestore(ctx, dom, se))
	require.False(t, checkpoint.ExistsCheckpointMaterializedViews(dom))
}

func TestPreCheckTableTiFlashReplicas(t *testing.T) {
	mockStores := []*metapb.Store{
		{
			Id: 1,
			Labels: []*metapb.StoreLabel{
				{
					Key:   "engine",
					Value: "tiflash",
				},
			},
		},
		{
			Id: 2,
			Labels: []*metapb.StoreLabel{
				{
					Key:   "engine",
					Value: "tiflash",
				},
			},
		},
	}

	pdClient := split.NewFakePDClient(mockStores, false, nil)

	tables := make([]*metautil.Table, 4)
	for i := 0; i < len(tables); i++ {
		tiflashReplica := &model.TiFlashReplicaInfo{
			Count: uint64(i),
		}
		if i == 0 {
			tiflashReplica = nil
		}

		tables[i] = &metautil.Table{
			DB: &model.DBInfo{Name: pmodel.NewCIStr("test")},
			Info: &model.TableInfo{
				ID:             int64(i),
				Name:           pmodel.NewCIStr("test" + strconv.Itoa(i)),
				TiFlashReplica: tiflashReplica,
			},
		}
	}
	ctx := context.Background()
	require.Nil(t, task.PreCheckTableTiFlashReplica(ctx, pdClient, tables, nil))

	for i := 0; i < len(tables); i++ {
		if i == 0 || i > 2 {
			require.Nil(t, tables[i].Info.TiFlashReplica)
		} else {
			require.NotNil(t, tables[i].Info.TiFlashReplica)
			obtainCount := int(tables[i].Info.TiFlashReplica.Count)
			require.Equal(t, i, obtainCount)
		}
	}

	require.Nil(t, task.PreCheckTableTiFlashReplica(ctx, pdClient, tables, tiflashrec.New()))
	for i := 0; i < len(tables); i++ {
		require.Nil(t, tables[i].Info.TiFlashReplica)
	}
}

func TestPreCheckTableClusterIndex(t *testing.T) {
	ctx := context.Background()
	m, err := mock.NewCluster()
	if err != nil {
		panic(err)
	}
	err = m.Start()
	if err != nil {
		panic(err)
	}
	defer m.Stop()
	g := gluetidb.New()
	se, err := g.CreateSession(m.Storage)
	require.NoError(t, err)

	info, err := m.Domain.GetSnapshotInfoSchema(math.MaxUint64)
	require.NoError(t, err)
	dbSchema, isExist := info.SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, isExist)

	tables := make([]*metautil.Table, 4)
	intField := types.NewFieldType(mysql.TypeLong)
	intField.SetCharset("binary")
	for i := len(tables) - 1; i >= 0; i-- {
		tables[i] = &metautil.Table{
			DB: dbSchema,
			Info: &model.TableInfo{
				ID:   int64(i),
				Name: pmodel.NewCIStr("test" + strconv.Itoa(i)),
				Columns: []*model.ColumnInfo{{
					ID:        1,
					Name:      pmodel.NewCIStr("id"),
					FieldType: *intField,
					State:     model.StatePublic,
				}},
				Charset: "utf8mb4",
				Collate: "utf8mb4_bin",
			},
		}
		err = se.CreateTable(ctx, tables[i].DB.Name, tables[i].Info, ddl.WithOnExist(ddl.OnExistIgnore))
		require.NoError(t, err)
	}

	// exist different tables
	tables[1].Info.IsCommonHandle = true
	err = task.PreCheckTableClusterIndex(tables, nil, m.Domain)
	require.Error(t, err)
	require.Regexp(t, `.*@@tidb_enable_clustered_index should be ON \(backup table = true, created table = false\).*`, err.Error())

	// exist different DDLs
	jobs := []*model.Job{{
		ID:         5,
		Type:       model.ActionCreateTable,
		SchemaName: "test",
		Query:      "",
		BinlogInfo: &model.HistoryInfo{
			TableInfo: &model.TableInfo{
				Name:           pmodel.NewCIStr("test1"),
				IsCommonHandle: true,
			},
		},
	}}
	err = task.PreCheckTableClusterIndex(nil, jobs, m.Domain)
	require.Error(t, err)
	require.Regexp(t, `.*@@tidb_enable_clustered_index should be ON \(backup table = true, created table = false\).*`, err.Error())

	// should pass pre-check cluster index
	tables[1].Info.IsCommonHandle = false
	jobs[0].BinlogInfo.TableInfo.IsCommonHandle = false
	require.Nil(t, task.PreCheckTableClusterIndex(tables, jobs, m.Domain))
}

func TestCheckNewCollationEnable(t *testing.T) {
	caseList := []struct {
		backupMeta                  *backuppb.BackupMeta
		newCollationEnableInCluster string
		CheckRequirements           bool
		isErr                       bool
	}{
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: "True"},
			newCollationEnableInCluster: "True",
			CheckRequirements:           true,
			isErr:                       false,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: "True"},
			newCollationEnableInCluster: "False",
			CheckRequirements:           true,
			isErr:                       true,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: "False"},
			newCollationEnableInCluster: "True",
			CheckRequirements:           true,
			isErr:                       true,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: "False"},
			newCollationEnableInCluster: "false",
			CheckRequirements:           true,
			isErr:                       false,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: "False"},
			newCollationEnableInCluster: "True",
			CheckRequirements:           false,
			isErr:                       true,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: "True"},
			newCollationEnableInCluster: "False",
			CheckRequirements:           false,
			isErr:                       true,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: ""},
			newCollationEnableInCluster: "True",
			CheckRequirements:           false,
			isErr:                       false,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: ""},
			newCollationEnableInCluster: "True",
			CheckRequirements:           true,
			isErr:                       true,
		},
		{
			backupMeta:                  &backuppb.BackupMeta{NewCollationsEnabled: ""},
			newCollationEnableInCluster: "False",
			CheckRequirements:           false,
			isErr:                       false,
		},
	}

	for i, ca := range caseList {
		g := &gluemock.MockGlue{
			GlobalVars: map[string]string{"new_collation_enabled": ca.newCollationEnableInCluster},
		}
		enabled, err := task.CheckNewCollationEnable(ca.backupMeta.GetNewCollationsEnabled(), g, nil, ca.CheckRequirements)
		t.Logf("[%d] Got Error: %v\n", i, err)
		if ca.isErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, ca.newCollationEnableInCluster == "True", enabled)
	}
}

func TestFilterDDLJobs(t *testing.T) {
	s := utiltest.CreateRestoreSchemaSuite(t)
	tk := testkit.NewTestKit(t, s.Mock.Storage)
	tk.MustExec("CREATE DATABASE IF NOT EXISTS test_db;")
	tk.MustExec("CREATE TABLE IF NOT EXISTS test_db.test_table (c1 INT);")
	lastTS, err := s.Mock.GetOracle().GetTimestamp(context.Background(), &oracle.Option{TxnScope: oracle.GlobalTxnScope})
	require.NoErrorf(t, err, "Error get last ts: %s", err)
	tk.MustExec("RENAME TABLE test_db.test_table to test_db.test_table1;")
	tk.MustExec("DROP TABLE test_db.test_table1;")
	tk.MustExec("DROP DATABASE test_db;")
	tk.MustExec("CREATE DATABASE test_db;")
	tk.MustExec("USE test_db;")
	tk.MustExec("CREATE TABLE test_table1 (c2 CHAR(255));")
	tk.MustExec("RENAME TABLE test_table1 to test_table;")
	tk.MustExec("TRUNCATE TABLE test_table;")

	ts, err := s.Mock.GetOracle().GetTimestamp(context.Background(), &oracle.Option{TxnScope: oracle.GlobalTxnScope})
	require.NoErrorf(t, err, "Error get ts: %s", err)

	cipher := backuppb.CipherInfo{
		CipherType: encryptionpb.EncryptionMethod_PLAINTEXT,
	}

	metaWriter := metautil.NewMetaWriter(s.Storage, metautil.MetaFileSize, false, "", &cipher)
	ctx := context.Background()
	metaWriter.StartWriteMetasAsync(ctx, metautil.AppendDDL)
	s.MockGlue.SetSession(tk.Session())
	err = backup.WriteBackupDDLJobs(metaWriter, s.MockGlue, s.Mock.Storage, lastTS, ts, false)
	require.NoErrorf(t, err, "Error get ddl jobs: %s", err)
	err = metaWriter.FinishWriteMetas(ctx, metautil.AppendDDL)
	require.NoErrorf(t, err, "Flush failed", err)
	err = metaWriter.FlushBackupMeta(ctx)
	require.NoErrorf(t, err, "Finially flush backupmeta failed", err)
	infoSchema, err := s.Mock.Domain.GetSnapshotInfoSchema(ts)
	require.NoErrorf(t, err, "Error get snapshot info schema: %s", err)
	dbInfo, ok := infoSchema.SchemaByName(pmodel.NewCIStr("test_db"))
	require.Truef(t, ok, "DB info not exist")
	tableInfo, err := infoSchema.TableByName(context.Background(), pmodel.NewCIStr("test_db"), pmodel.NewCIStr("test_table"))
	require.NoErrorf(t, err, "Error get table info: %s", err)
	tables := []*metautil.Table{{
		DB:   dbInfo,
		Info: tableInfo.Meta(),
	}}
	metaBytes, err := s.Storage.ReadFile(ctx, metautil.MetaFile)
	require.NoError(t, err)
	mockMeta := &backuppb.BackupMeta{}
	err = proto.Unmarshal(metaBytes, mockMeta)
	require.NoError(t, err)
	// check the schema version
	require.Equal(t, int32(metautil.MetaV1), mockMeta.Version)
	metaReader := metautil.NewMetaReader(mockMeta, s.Storage, &cipher)
	allDDLJobsBytes, err := metaReader.ReadDDLs(ctx)
	require.NoError(t, err)
	var allDDLJobs []*model.Job
	err = json.Unmarshal(allDDLJobsBytes, &allDDLJobs)
	require.NoError(t, err)

	ddlJobs := task.FilterDDLJobs(allDDLJobs, tables)
	for _, job := range ddlJobs {
		t.Logf("get ddl job: %s", job.Query)
	}
	require.Equal(t, 7, len(ddlJobs))
}

func TestFilterDDLJobsV2(t *testing.T) {
	s := utiltest.CreateRestoreSchemaSuite(t)
	tk := testkit.NewTestKit(t, s.Mock.Storage)
	tk.MustExec("CREATE DATABASE IF NOT EXISTS test_db;")
	tk.MustExec("CREATE TABLE IF NOT EXISTS test_db.test_table (c1 INT);")
	lastTS, err := s.Mock.GetOracle().GetTimestamp(context.Background(), &oracle.Option{TxnScope: oracle.GlobalTxnScope})
	require.NoErrorf(t, err, "Error get last ts: %s", err)
	tk.MustExec("RENAME TABLE test_db.test_table to test_db.test_table1;")
	tk.MustExec("DROP TABLE test_db.test_table1;")
	tk.MustExec("DROP DATABASE test_db;")
	tk.MustExec("CREATE DATABASE test_db;")
	tk.MustExec("USE test_db;")
	tk.MustExec("CREATE TABLE test_table1 (c2 CHAR(255));")
	tk.MustExec("RENAME TABLE test_table1 to test_table;")
	tk.MustExec("TRUNCATE TABLE test_table;")

	ts, err := s.Mock.GetOracle().GetTimestamp(context.Background(), &oracle.Option{TxnScope: oracle.GlobalTxnScope})
	require.NoErrorf(t, err, "Error get ts: %s", err)

	cipher := backuppb.CipherInfo{
		CipherType: encryptionpb.EncryptionMethod_PLAINTEXT,
	}

	metaWriter := metautil.NewMetaWriter(s.Storage, metautil.MetaFileSize, true, "", &cipher)
	ctx := context.Background()
	metaWriter.StartWriteMetasAsync(ctx, metautil.AppendDDL)
	s.MockGlue.SetSession(tk.Session())
	err = backup.WriteBackupDDLJobs(metaWriter, s.MockGlue, s.Mock.Storage, lastTS, ts, false)
	require.NoErrorf(t, err, "Error get ddl jobs: %s", err)
	err = metaWriter.FinishWriteMetas(ctx, metautil.AppendDDL)
	require.NoErrorf(t, err, "Flush failed", err)
	err = metaWriter.FlushBackupMeta(ctx)
	require.NoErrorf(t, err, "Flush BackupMeta failed", err)

	infoSchema, err := s.Mock.Domain.GetSnapshotInfoSchema(ts)
	require.NoErrorf(t, err, "Error get snapshot info schema: %s", err)
	dbInfo, ok := infoSchema.SchemaByName(pmodel.NewCIStr("test_db"))
	require.Truef(t, ok, "DB info not exist")
	tableInfo, err := infoSchema.TableByName(context.Background(), pmodel.NewCIStr("test_db"), pmodel.NewCIStr("test_table"))
	require.NoErrorf(t, err, "Error get table info: %s", err)
	tables := []*metautil.Table{{
		DB:   dbInfo,
		Info: tableInfo.Meta(),
	}}
	metaBytes, err := s.Storage.ReadFile(ctx, metautil.MetaFile)
	require.NoError(t, err)
	mockMeta := &backuppb.BackupMeta{}
	err = proto.Unmarshal(metaBytes, mockMeta)
	require.NoError(t, err)
	// check the schema version
	require.Equal(t, int32(metautil.MetaV2), mockMeta.Version)
	metaReader := metautil.NewMetaReader(mockMeta, s.Storage, &cipher)
	allDDLJobsBytes, err := metaReader.ReadDDLs(ctx)
	require.NoError(t, err)
	var allDDLJobs []*model.Job
	err = json.Unmarshal(allDDLJobsBytes, &allDDLJobs)
	require.NoError(t, err)

	ddlJobs := task.FilterDDLJobs(allDDLJobs, tables)
	for _, job := range ddlJobs {
		t.Logf("get ddl job: %s", job.Query)
	}
	require.Equal(t, 7, len(ddlJobs))
}

func TestFilterDDLJobByRules(t *testing.T) {
	ddlJobs := []*model.Job{
		{
			Type: model.ActionSetTiFlashReplica,
		},
		{
			Type: model.ActionAddPrimaryKey,
		},
		{
			Type: model.ActionUpdateTiFlashReplicaStatus,
		},
		{
			Type: model.ActionCreateTable,
		},
		{
			Type: model.ActionLockTable,
		},
		{
			Type: model.ActionAddIndex,
		},
		{
			Type: model.ActionUnlockTable,
		},
		{
			Type: model.ActionCreateSchema,
		},
		{
			Type: model.ActionModifyColumn,
		},
	}

	expectedDDLTypes := []model.ActionType{
		model.ActionAddPrimaryKey,
		model.ActionCreateTable,
		model.ActionAddIndex,
		model.ActionCreateSchema,
		model.ActionModifyColumn,
	}

	ddlJobs = task.FilterDDLJobByRules(ddlJobs, task.DDLJobBlockListRule)

	require.Equal(t, len(expectedDDLTypes), len(ddlJobs))
	for i, ddlJob := range ddlJobs {
		assert.Equal(t, expectedDDLTypes[i], ddlJob.Type)
	}
}

func TestCheckDDLJobByRules(t *testing.T) {
	ddlJobs := []*model.Job{
		{
			Type: model.ActionSetTiFlashReplica,
		},
		{
			Type: model.ActionAddPrimaryKey,
		},
		{
			Type: model.ActionUpdateTiFlashReplicaStatus,
		},
		{
			Type: model.ActionCreateTable,
		},
		{
			Type: model.ActionLockTable,
		},
		{
			Type: model.ActionAddIndex,
		},
		{
			Type: model.ActionUnlockTable,
		},
		{
			Type: model.ActionCreateSchema,
		},
		{
			Type: model.ActionModifyColumn,
		},
		{
			Type: model.ActionReorganizePartition,
		},
	}

	filteredDDlJobs := task.FilterDDLJobByRules(ddlJobs, task.DDLJobLogIncrementalCompactBlockListRule)

	expectedDDLTypes := []model.ActionType{
		model.ActionSetTiFlashReplica,
		model.ActionAddPrimaryKey,
		model.ActionUpdateTiFlashReplicaStatus,
		model.ActionCreateTable,
		model.ActionLockTable,
		model.ActionUnlockTable,
		model.ActionCreateSchema,
	}

	require.Equal(t, len(expectedDDLTypes), len(filteredDDlJobs))
	expectedDDLJobs := make([]*model.Job, 0, len(expectedDDLTypes))
	for i, ddlJob := range filteredDDlJobs {
		assert.Equal(t, expectedDDLTypes[i], ddlJob.Type)
		expectedDDLJobs = append(expectedDDLJobs, ddlJob)
	}

	require.NoError(t, task.CheckDDLJobByRules(expectedDDLJobs, task.DDLJobLogIncrementalCompactBlockListRule))
	require.Error(t, task.CheckDDLJobByRules(ddlJobs, task.DDLJobLogIncrementalCompactBlockListRule))
}

// NOTICE: Once there is a new backfilled type ddl, BR needs to ensure that it is correctly cover by the rules:
func TestMonitorTheIncrementalUnsupportDDLType(t *testing.T) {
	require.Equal(t, int(5), ddl.BackupFillerTypeCount())
}

func TestTikvUsage(t *testing.T) {
	files := []*backuppb.File{
		{Name: "F1", Size_: 1 * pb},
		{Name: "F2", Size_: 2 * pb},
		{Name: "F3", Size_: 3 * pb},
		{Name: "F4", Size_: 4 * pb},
		{Name: "F5", Size_: 5 * pb},
	}
	replica := uint64(3)
	storeCnt := uint64(6)
	total := uint64(0)
	for _, f := range files {
		total += f.GetSize_()
	}
	ret := task.EstimateTikvUsage(files, replica, storeCnt)
	require.Equal(t, total*replica/storeCnt, ret)
}

func TestTiflashUsage(t *testing.T) {
	tables := []*metautil.Table{
		{Info: &model.TableInfo{TiFlashReplica: &model.TiFlashReplicaInfo{Count: 0}},
			Files: []*backuppb.File{{Size_: 1 * pb}}},
		{Info: &model.TableInfo{TiFlashReplica: &model.TiFlashReplicaInfo{Count: 1}},
			Files: []*backuppb.File{{Size_: 2 * pb}}},
		{Info: &model.TableInfo{TiFlashReplica: &model.TiFlashReplicaInfo{Count: 2}},
			Files: []*backuppb.File{{Size_: 3 * pb}}},
	}

	var storeCnt uint64 = 3
	ret := task.EstimateTiflashUsage(tables, storeCnt)
	require.Equal(t, 8*pb/3, ret)
}

func TestCheckTikvSpace(t *testing.T) {
	store := pdhttp.StoreInfo{Store: pdhttp.MetaStore{ID: 1}, Status: pdhttp.StoreStatus{Available: "500PB"}}
	require.NoError(t, task.CheckStoreSpace(400*pb, &store))
}

func TestRestoreMaterializedViewRefreshScheduleSQLMode(t *testing.T) {
	ctx := context.Background()
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set time_zone = '+00:00'")
	tk.MustExec("set sql_mode = ''")
	tk.MustExec("create table review_base (a int primary key, b int not null)")
	tk.MustExec("insert into review_base values (1,10)")
	tk.MustExec("create materialized view log on review_base (a,b)")
	tk.MustExec("create materialized view review_mv (a,s,cnt) refresh fast as select a,sum(b),count(*) from review_base group by a")
	tk.MustExec("set sql_mode = 'NO_UNSIGNED_SUBTRACTION'")
	tk.MustExec("alter materialized view review_mv refresh next cast(date_add('2030-01-01', interval (cast(0 as unsigned) - 1 + 2) day) as datetime)")
	db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, ok)
	infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
	require.NoError(t, err)
	var tables []*metautil.Table
	for _, info := range infos {
		tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
	}
	tk.MustExec("drop materialized view review_mv")
	tk.MustExec("drop materialized view log on review_base")
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
	restored, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("review_mv"))
	require.NoError(t, err)
	require.Zero(t, restored.Meta().MaterializedView.DefinitionSQLMode)
	require.Equal(t, mysql.ModeNoUnsignedSubtraction, restored.Meta().MaterializedView.RefreshScheduleSQLMode)
	tk.MustQuery("select NEXT_REFRESH_UNIX_SECONDS = TIMESTAMPDIFF(SECOND, '1970-01-01 00:00:00', '2030-01-02 00:00:00') from mysql.tidb_mview_refresh_info").Check(testkit.Rows("1"))
	// Resume a CREATE that completed before its separate schedule was applied.
	id := restored.Meta().ID
	tk.MustExec("alter materialized view review_mv refresh")
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
	restored, err = dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("review_mv"))
	require.NoError(t, err)
	require.Equal(t, id, restored.Meta().ID)
	tk.MustQuery("select NEXT_REFRESH_UNIX_SECONDS is not null from mysql.tidb_mview_refresh_info").Check(testkit.Rows("1"))
}

func TestRestoreMaterializedViewTiFlashReplicas(t *testing.T) {
	ctx := context.Background()
	store, dom := testkit.CreateMockStoreAndDomain(t, mockstore.WithMockTiFlash(2))
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table review_tf_base (a int primary key, b int not null)")
	tk.MustExec("insert into review_tf_base values (1,10)")
	tk.MustExec("create materialized view log on review_tf_base (a,b)")
	tk.MustExec("create materialized view review_tf_mv (a,s,cnt) refresh fast as select a,sum(b),count(*) from review_tf_base group by a")
	tk.MustExec("alter table review_tf_mv set tiflash replica 1")
	tk.MustExec("alter table `$mlog$review_tf_base` set tiflash replica 1")
	db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, ok)
	infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
	require.NoError(t, err)
	var tables []*metautil.Table
	for _, info := range infos {
		tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
	}
	tk.MustExec("drop materialized view review_tf_mv")
	tk.MustExec("drop materialized view log on review_tf_base")
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
	for _, name := range []string{"review_tf_mv", "$mlog$review_tf_base"} {
		actual, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr(name))
		require.NoError(t, err)
		if assert.NotNil(t, actual.Meta().TiFlashReplica, "TiFlash config is lost for %s", name) {
			require.EqualValues(t, 1, actual.Meta().TiFlashReplica.Count)
			require.False(t, actual.Meta().TiFlashReplica.Available)
		}
	}
	// A TiKV-only target still restores the data, as for ordinary snapshot tables.
	targetStore, targetDom := testkit.CreateMockStoreAndDomain(t)
	targetTK := testkit.NewTestKit(t, targetStore)
	targetTK.MustExec("use test")
	targetTK.MustExec("create table review_tf_base (a int primary key, b int not null)")
	targetTK.MustExec("insert into review_tf_base values (1,10)")
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), targetDom, tables))
	targetTK.MustQuery("select * from review_tf_mv").Check(testkit.Rows("1 10 1"))
	actual, err := targetDom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("review_tf_mv"))
	require.NoError(t, err)
	require.Nil(t, actual.Meta().TiFlashReplica)
}

func TestRestoreMaterializedViewRenamedGroupIndex(t *testing.T) {
	for _, column := range []string{"a", "primary"} {
		t.Run(column, func(t *testing.T) {
			ctx := context.Background()
			store, dom := testkit.CreateMockStoreAndDomain(t)
			tk := testkit.NewTestKit(t, store)
			tk.MustExec("use test")
			tk.MustExec("create table review_group_base (a int, b int not null)")
			tk.MustExec("insert into review_group_base values (1,10)")
			tk.MustExec("create materialized view log on review_group_base (a,b)")
			tk.MustExec("create materialized view review_group_mv (`" + column + "`,s,cnt) as select a,sum(b),count(*) from review_group_base group by a")
			original, err := dom.InfoSchema().TableByName(ctx, pmodel.NewCIStr("test"), pmodel.NewCIStr("review_group_mv"))
			require.NoError(t, err)
			require.Len(t, original.Meta().Indices, 1)
			require.True(t, original.Meta().Indices[0].Unique)
			name := original.Meta().Indices[0].Name.O
			tk.MustExec("alter table review_group_mv rename index `" + name + "` to group_key")
			tk.MustExec("alter table review_group_mv alter index group_key invisible")
			tk.MustExec("alter table review_group_mv add index `" + name + "` (s)")
			db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
			require.True(t, ok)
			infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
			require.NoError(t, err)
			var tables []*metautil.Table
			for _, info := range infos {
				tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
			}
			for _, export := range []bool{false, true} {
				tk.MustExec("drop materialized view review_group_mv")
				tk.MustExec("drop materialized view log on review_group_base")
				if export {
					sqlStorage, err := storage.NewLocalStorage(t.TempDir())
					require.NoError(t, err)
					cfg := &task.RestoreConfig{Config: task.Config{TableFilter: filter.All()}, RepairSQLStorage: sqlStorage}
					require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, gluetidb.New(), dom, cfg, recordedMaterializedViewSchemas(tables)))
					script, err := sqlStorage.ReadFile(ctx, "materialized-views.sql")
					require.NoError(t, err)
					for _, sql := range strings.Split(string(script), ";\n") {
						if strings.TrimSpace(sql) != "" {
							tk.MustExec(sql)
						}
					}
				} else {
					require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
				}
				checkIndexes := func() {
					tk.MustQuery("select INDEX_NAME, NON_UNIQUE, COLUMN_NAME, IS_VISIBLE from information_schema.statistics where TABLE_SCHEMA='test' and TABLE_NAME='review_group_mv' order by NON_UNIQUE, INDEX_NAME").Check(testkit.Rows("group_key 0 "+column+" NO", name+" 1 s YES"))
				}
				checkIndexes()
				rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("review_group_mv"))
				require.NoError(t, err)
				// Simulate an interrupted attempt after the group-index rename.
				tk.MustExec("alter table review_group_mv alter index group_key visible")
				tk.MustExec("alter table review_group_mv drop index `" + name + "`")
				require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
				retried, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("review_group_mv"))
				require.NoError(t, err)
				require.Equal(t, rebuilt.Meta().ID, retried.Meta().ID)
				checkIndexes()
			}
		})
	}
}

func TestRestoreMaterializedViewIndexCommentEscaping(t *testing.T) {
	ctx := context.Background()
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set sql_mode = ''")
	tk.MustExec("create table review_comment_base (a int primary key, b int not null)")
	tk.MustExec("insert into review_comment_base values (1,10)")
	tk.MustExec("create materialized view log on review_comment_base (a,b)")
	tk.MustExec("create materialized view review_comment_mv (a,s,cnt) as select a,sum(b),count(*) from review_comment_base group by a")
	tk.MustExec(`create index idx_s on review_comment_mv(s) comment 'tail\\'`)
	original, err := dom.InfoSchema().TableByName(ctx, pmodel.NewCIStr("test"), pmodel.NewCIStr("review_comment_mv"))
	require.NoError(t, err)
	require.Equal(t, "tail\\", original.Meta().FindIndexByName("idx_s").Comment)
	db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, ok)
	infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
	require.NoError(t, err)
	var tables []*metautil.Table
	for _, info := range infos {
		tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
	}
	tk.MustExec("drop materialized view review_comment_mv")
	tk.MustExec("drop materialized view log on review_comment_base")
	require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
	rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("review_comment_mv"))
	require.NoError(t, err)
	require.Equal(t, "tail\\", rebuilt.Meta().FindIndexByName("idx_s").Comment)
}

func TestRestoreMaterializedViewClusteredIndexMode(t *testing.T) {
	ctx := context.Background()
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set session tidb_enable_clustered_index = OFF")
	tk.MustExec("create table review_cluster_base (a int not null, b int not null)")
	tk.MustExec("insert into review_cluster_base values (1,10)")
	tk.MustExec("create materialized view log on review_cluster_base (a,b)")
	tk.MustExec("create materialized view review_cluster_mv (a,s,cnt) shard_row_id_bits = 2 as select a,sum(b),count(*) from review_cluster_base group by a")
	original, err := dom.InfoSchema().TableByName(ctx, pmodel.NewCIStr("test"), pmodel.NewCIStr("review_cluster_mv"))
	require.NoError(t, err)
	require.False(t, original.Meta().HasClusteredIndex())
	db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, ok)
	infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
	require.NoError(t, err)
	var tables []*metautil.Table
	for _, info := range infos {
		tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
	}
	tk.MustExec("set global tidb_enable_clustered_index = ON")
	for _, export := range []bool{false, true} {
		tk.MustExec("drop materialized view review_cluster_mv")
		tk.MustExec("drop materialized view log on review_cluster_base")
		if export {
			sqlStorage, err := storage.NewLocalStorage(t.TempDir())
			require.NoError(t, err)
			cfg := &task.RestoreConfig{Config: task.Config{TableFilter: filter.All()}, RepairSQLStorage: sqlStorage}
			require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, gluetidb.New(), dom, cfg, recordedMaterializedViewSchemas(tables)))
			script, err := sqlStorage.ReadFile(ctx, "materialized-views.sql")
			require.NoError(t, err)
			tk.MustExec("set session tidb_enable_clustered_index = ON")
			for _, sql := range strings.Split(string(script), ";\n") {
				if strings.TrimSpace(sql) != "" {
					tk.MustExec(sql)
				}
			}
		} else {
			require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
		}
		rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("review_cluster_mv"))
		require.NoError(t, err)
		require.False(t, rebuilt.Meta().HasClusteredIndex())
		require.EqualValues(t, 2, rebuilt.Meta().ShardRowIDBits)
	}
}

func TestRestoreMaterializedViewDroppedGroupIndex(t *testing.T) {
	for _, tc := range []struct {
		name, column       string
		notNull, composite bool
	}{
		{"unique", "a", false, false}, {"reserved name", "primary", false, false},
		{"primary", "a", true, false}, {"composite", "primary", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, dom := testkit.CreateMockStoreAndDomain(t)
			tk := testkit.NewTestKit(t, store)
			tk.MustExec("use test")
			tk.MustExec("set tidb_enable_clustered_index = OFF")
			nullability := ""
			if tc.notNull {
				nullability = " not null"
			}
			tk.MustExec("create table dropped_group_base(a int" + nullability + ",b int not null,c int)")
			tk.MustExec("insert into dropped_group_base values(1,10,2)")
			createLog := "create materialized view log on dropped_group_base(a,b,c)"
			createMV := "create materialized view dropped_group_mv(`" + tc.column + "`,s,cnt) as select a,sum(b),count(*) from dropped_group_base group by a"
			if tc.composite {
				// GROUP BY order differs from SELECT order, with an aggregate first.
				createMV = "create materialized view dropped_group_mv(s,`primary`,a,cnt) as select sum(b),c,a,count(*) from dropped_group_base group by c,a"
			}
			tk.MustExec(createLog)
			tk.MustExec(createMV)
			original, err := dom.InfoSchema().TableByName(ctx, pmodel.NewCIStr("test"), pmodel.NewCIStr("dropped_group_mv"))
			require.NoError(t, err)
			require.Len(t, original.Meta().Indices, 1)
			name := original.Meta().Indices[0].Name.O
			if tc.notNull {
				tk.MustExec("alter table dropped_group_mv drop primary key")
				name = "idx_s"
			} else {
				tk.MustExec("alter table dropped_group_mv drop index `" + name + "`")
			}
			tk.MustExec("create index `" + name + "` on dropped_group_mv(s)")
			db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
			require.True(t, ok)
			infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
			require.NoError(t, err)
			var tables []*metautil.Table
			for _, info := range infos {
				tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
			}
			for _, phase := range []string{"native", "export", "after create"} {
				tk.MustExec("drop materialized view dropped_group_mv")
				tk.MustExec("drop materialized view log on dropped_group_base")
				if phase == "export" {
					sqlStorage, err := storage.NewLocalStorage(t.TempDir())
					require.NoError(t, err)
					cfg := &task.RestoreConfig{Config: task.Config{TableFilter: filter.All()}, RepairSQLStorage: sqlStorage}
					require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, gluetidb.New(), dom, cfg, recordedMaterializedViewSchemas(tables)))
					script, err := sqlStorage.ReadFile(ctx, "materialized-views.sql")
					require.NoError(t, err)
					for _, sql := range strings.Split(string(script), ";\n") {
						if strings.TrimSpace(sql) != "" {
							tk.MustExec(sql)
						}
					}
				} else {
					if phase == "after create" {
						// Resume with CREATE complete, before dropping its implicit index.
						tk.MustExec(createLog)
						tk.MustExec(createMV)
					}
					require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
				}
				check := func() {
					tk.MustQuery("select INDEX_NAME,NON_UNIQUE,COLUMN_NAME from information_schema.statistics where TABLE_SCHEMA='test' and TABLE_NAME='dropped_group_mv'").Check(testkit.Rows(name + " 1 s"))
				}
				check()
				rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("dropped_group_mv"))
				require.NoError(t, err)
				require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
				retried, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("dropped_group_mv"))
				require.NoError(t, err)
				require.Equal(t, rebuilt.Meta().ID, retried.Meta().ID)
				check()
			}
		})
	}
}

func TestRestoreMaterializedViewCommentSQLMode(t *testing.T) {
	for _, mode := range []string{"", "NO_BACKSLASH_ESCAPES"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, dom := testkit.CreateMockStoreAndDomain(t)
			tk := testkit.NewTestKit(t, store)
			tk.MustExec("use test")
			tk.MustExec("set sql_mode = '" + mode + "'")
			tk.MustExec("create table comment_base(a int primary key,b int not null)")
			tk.MustExec("insert into comment_base values(1,10)")
			tk.MustExec("create materialized view log on comment_base(a,b)")
			comment := "line1\nline2\rquote'\\tail"
			escaped := comment
			if mode == "" {
				escaped = strings.ReplaceAll(escaped, "\\", "\\\\")
			}
			escaped = strings.ReplaceAll(escaped, "'", "''")
			tk.MustExec("create materialized view comment_mv(a,s,cnt) comment = '" + escaped + "' as select a,sum(b),count(*) from comment_base group by a")
			original, err := dom.InfoSchema().TableByName(ctx, pmodel.NewCIStr("test"), pmodel.NewCIStr("comment_mv"))
			require.NoError(t, err)
			require.Equal(t, comment, original.Meta().Comment)
			db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
			require.True(t, ok)
			infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
			require.NoError(t, err)
			var tables []*metautil.Table
			for _, info := range infos {
				tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
			}
			for _, export := range []bool{false, true} {
				tk.MustExec("drop materialized view comment_mv")
				tk.MustExec("drop materialized view log on comment_base")
				if export {
					sqlStorage, err := storage.NewLocalStorage(t.TempDir())
					require.NoError(t, err)
					cfg := &task.RestoreConfig{Config: task.Config{TableFilter: filter.All()}, RepairSQLStorage: sqlStorage}
					require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, gluetidb.New(), dom, cfg, recordedMaterializedViewSchemas(tables)))
					script, err := sqlStorage.ReadFile(ctx, "materialized-views.sql")
					require.NoError(t, err)
					for _, sql := range strings.Split(string(script), ";\n") {
						if strings.TrimSpace(sql) != "" {
							tk.MustExec(sql)
						}
					}
				} else {
					require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
				}
				rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("comment_mv"))
				require.NoError(t, err)
				require.Equal(t, comment, rebuilt.Meta().Comment)
			}
		})
	}
}

func TestRestoreMaterializedViewQueryBackslash(t *testing.T) {
	for _, mode := range []string{"", "NO_BACKSLASH_ESCAPES"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, dom := testkit.CreateMockStoreAndDomain(t)
			tk := testkit.NewTestKit(t, store)
			tk.MustExec("use test")
			tk.MustExec("set sql_mode = '" + mode + "'")
			tk.MustExec("create table rb(a int primary key,b varchar(30) not null)")
			backslash := "\\"
			for n := 0; n <= 4; n++ {
				tk.MustExec("insert into rb values(?,?)", n, "x"+strings.Repeat(backslash, n)+"'y")
			}
			schedule := "TIMESTAMP('2030-01-01') + INTERVAL LENGTH('x" + backslash + "y') DAY"
			tk.MustExec("set sql_mode = 'NO_BACKSLASH_ESCAPES'")
			tk.MustExec("create materialized view log on rb(a,b) purge next " + schedule)
			tk.MustExec("set sql_mode = '" + mode + "'")
			literal := "x" + backslash + "''y"
			if mode == "" {
				literal = strings.ReplaceAll(literal, backslash, strings.Repeat(backslash, 2))
			}
			tk.MustExec("create materialized view rm(a,cnt) as select a,count(*) from rb where b='" + literal + "' group by a")
			tk.MustQuery("select * from rm").Check(testkit.Rows("1 1"))
			// The default-mode definition exercises ALTER with a different schedule mode;
			// the other case embeds that same schedule directly in CREATE.
			tk.MustExec("set sql_mode = 'NO_BACKSLASH_ESCAPES'")
			tk.MustExec("alter materialized view rm refresh next " + schedule)
			original, err := dom.InfoSchema().TableByName(ctx, pmodel.NewCIStr("test"), pmodel.NewCIStr("rm"))
			require.NoError(t, err)
			query := original.Meta().MaterializedView.SQLContent
			refresh := original.Meta().MaterializedView.RefreshNext
			db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
			require.True(t, ok)
			infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
			require.NoError(t, err)
			var tables []*metautil.Table
			for _, info := range infos {
				tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
			}
			for _, export := range []bool{false, true} {
				tk.MustExec("drop materialized view rm")
				tk.MustExec("drop materialized view log on rb")
				if export {
					sqlStorage, err := storage.NewLocalStorage(t.TempDir())
					require.NoError(t, err)
					cfg := &task.RestoreConfig{Config: task.Config{TableFilter: filter.All()}, RepairSQLStorage: sqlStorage}
					require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, gluetidb.New(), dom, cfg, recordedMaterializedViewSchemas(tables)))
					script, err := sqlStorage.ReadFile(ctx, "materialized-views.sql")
					require.NoError(t, err)
					for _, sql := range strings.Split(string(script), ";\n") {
						if strings.TrimSpace(sql) != "" {
							tk.MustExec(sql)
						}
					}
				} else {
					require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
				}
				tk.MustQuery("select * from rm").Check(testkit.Rows("1 1"))
				rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("rm"))
				require.NoError(t, err)
				require.Equal(t, query, rebuilt.Meta().MaterializedView.SQLContent)
				require.Equal(t, refresh, rebuilt.Meta().MaterializedView.RefreshNext)
				log, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("$mlog$rb"))
				require.NoError(t, err)
				for _, table := range tables {
					if table.Info.MaterializedViewLog != nil {
						require.Equal(t, table.Info.MaterializedViewLog.PurgeNext, log.Meta().MaterializedViewLog.PurgeNext)
					}
				}
				id := rebuilt.Meta().ID
				require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
				rebuilt, err = dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("rm"))
				require.NoError(t, err)
				require.Equal(t, id, rebuilt.Meta().ID)
				tk.MustQuery("select * from rm").Check(testkit.Rows("1 1"))
			}
		})
	}
}

func TestRestoreMaterializedViewRenamedPrimaryIndex(t *testing.T) {
	ctx := context.Background()
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set tidb_enable_clustered_index=OFF")
	tk.MustExec("create table rb(a int not null,b int not null)")
	tk.MustExec("insert into rb values(1,10)")
	tk.MustExec("create materialized view log on rb(a,b)")
	tk.MustExec("create materialized view rm(a,s,cnt) as select a,sum(b),count(*) from rb group by a")
	tk.MustExec("alter table rm rename index `PRIMARY` to group_key")
	original, err := dom.InfoSchema().TableByName(ctx, pmodel.NewCIStr("test"), pmodel.NewCIStr("rm"))
	require.NoError(t, err)
	require.Len(t, original.Meta().Indices, 1)
	require.True(t, original.Meta().Indices[0].Primary)
	require.Equal(t, "group_key", original.Meta().Indices[0].Name.O)
	tk.MustExec("create materialized view log on rm(a,s,cnt)")
	tk.MustExec("create materialized view rc(a,ss,cs,cnt) as select a,sum(s),count(s),count(*) from rm use index(group_key) group by a")
	tk.MustQuery("select * from rc").Check(testkit.Rows("1 10 1 1"))
	db, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr("test"))
	require.True(t, ok)
	infos, err := dom.InfoSchema().SchemaTableInfos(ctx, db.Name)
	require.NoError(t, err)
	var tables []*metautil.Table
	for _, info := range infos {
		tables = append(tables, &metautil.Table{DB: db, Info: info.Clone()})
	}
	for _, export := range []bool{false, true} {
		tk.MustExec("drop materialized view rc")
		tk.MustExec("drop materialized view log on rm")
		tk.MustExec("drop materialized view rm")
		tk.MustExec("drop materialized view log on rb")
		if export {
			sqlStorage, err := storage.NewLocalStorage(t.TempDir())
			require.NoError(t, err)
			cfg := &task.RestoreConfig{Config: task.Config{TableFilter: filter.All()}, RepairSQLStorage: sqlStorage}
			require.NoError(t, task.RebuildRecordedMaterializedViewsForTest(ctx, gluetidb.New(), dom, cfg, recordedMaterializedViewSchemas(tables)))
			script, err := sqlStorage.ReadFile(ctx, "materialized-views.sql")
			require.NoError(t, err)
			for _, sql := range strings.Split(string(script), ";\n") {
				if strings.TrimSpace(sql) != "" {
					tk.MustExec(sql)
				}
			}
		} else {
			require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
		}
		tk.MustQuery("select * from rc").Check(testkit.Rows("1 10 1 1"))
		rebuilt, err := dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("rm"))
		require.NoError(t, err)
		require.True(t, rebuilt.Meta().Indices[0].Primary)
		require.Equal(t, "group_key", rebuilt.Meta().Indices[0].Name.O)
		id := rebuilt.Meta().ID
		require.NoError(t, task.RestoreMaterializedViewsForTest(ctx, gluetidb.New(), dom, tables))
		rebuilt, err = dom.InfoSchema().TableByName(ctx, db.Name, pmodel.NewCIStr("rm"))
		require.NoError(t, err)
		require.Equal(t, id, rebuilt.Meta().ID)
	}
}
