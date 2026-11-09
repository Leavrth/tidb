// Copyright 2026 PingCAP, Inc. Licensed under Apache-2.0.

package task

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	pmodel "github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/util/sqlescape"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"go.uber.org/zap"
)

// materializedViewRestorePlan retains source definitions separately from the
// tables passed to CreateTables. Objects are ordered by dependency.
type materializedViewRestorePlan struct {
	tables  map[int64]*metautil.Table
	objects []*metautil.Table
}

func hasMaterializedViewDefinition(info *model.TableInfo) bool {
	return info != nil && (info.MaterializedView != nil || info.MaterializedViewLog != nil || info.MaterializedViewBase != nil)
}

func skipMaterializedViewRestore(info *model.TableInfo) bool {
	return info.MaterializedViewShadow != nil
}

func clearMaterializedViewInfo(info *model.TableInfo) {
	info.MaterializedView = nil
	info.MaterializedViewLog = nil
	info.MaterializedViewBase = nil
	info.MaterializedViewShadow = nil
	info.State = model.StatePublic
}

// indexMaterializedViewTables retains source definitions and identifies derived
// objects, including those whose own MV fields disappeared during DDL.
func indexMaterializedViewTables(tables []*metautil.Table) (*materializedViewRestorePlan, map[int64]bool) {
	p := &materializedViewRestorePlan{tables: make(map[int64]*metautil.Table)}
	derivedTables := make(map[int64]bool)
	for _, table := range tables {
		info := table.Info
		if info == nil || skipMaterializedViewRestore(info) {
			continue
		}
		p.tables[info.ID] = table
		// A reverse reference still identifies an MLog/MV whose own fields
		// disappeared during DDL. It must not become an ordinary base.
		if info.MaterializedView != nil || info.MaterializedViewLog != nil {
			derivedTables[info.ID] = true
		}
		if base := info.MaterializedViewBase; base != nil {
			if base.MLogID != 0 {
				derivedTables[base.MLogID] = true
			}
			for _, id := range base.MViewIDs {
				derivedTables[id] = true
			}
		}
		if log := info.MaterializedViewLog; log != nil {
			for _, id := range log.DependentMViewIDs {
				derivedTables[id] = true
			}
		}
	}
	return p, derivedTables
}

// newMaterializedViewRestorePlan selects the schemas/SSTs to import now and
// retains source definitions for later logical rebuild. Invalid derived
// objects are skipped; their ordinary bases can still be restored.
func newMaterializedViewRestorePlan(tables []*metautil.Table, pitr bool) (*materializedViewRestorePlan, []*metautil.Table, error) {
	p, derivedTables := indexMaterializedViewTables(tables)
	// PITR rebuilds from the recorder after replay, which may change or drop
	// these definitions. Only snapshot-only restore needs their dependencies.
	if !pitr {
		if err := p.collectRestorableObjects(tables, derivedTables); err != nil {
			return nil, nil, errors.Trace(err)
		}
	}
	// A base whose own MV fields disappeared can still be identified by its
	// dependents. Restore it as an ordinary public table as well.
	baseIDs := make(map[int64]bool)
	for _, table := range p.tables {
		if log := table.Info.MaterializedViewLog; log != nil {
			baseIDs[log.BaseTableID] = true
		}
		if mv := table.Info.MaterializedView; mv != nil {
			for _, id := range mv.BaseTableIDs {
				baseIDs[id] = true
			}
		}
	}
	newTables := make([]*metautil.Table, 0, len(tables))
	for _, source := range tables {
		table := p.tables[source.Info.ID]
		if table == nil || derivedTables[source.Info.ID] {
			continue
		}
		info := table.Info
		// Preserve the plan's definitions and schedules when preparing schemas.
		clone := *table
		clone.Info = info.Clone()
		if hasMaterializedViewDefinition(info) || baseIDs[info.ID] {
			clearMaterializedViewInfo(clone.Info)
		}
		newTables = append(newTables, &clone)
	}
	return p, newTables, nil
}

// collectRestorableObjects accepts each public MLog and ready MV only after
// its base is accepted. Appending accepted MVs as bases also handles cascades.
func (p *materializedViewRestorePlan) collectRestorableObjects(tables []*metautil.Table, derivedTables map[int64]bool) error {
	var pendingBases []*metautil.Table
	for _, source := range tables {
		table := p.tables[source.Info.ID]
		if table == nil || table.Info.State != model.StatePublic {
			continue
		}
		info := table.Info
		if !derivedTables[info.ID] && info.MaterializedViewBase != nil {
			pendingBases = append(pendingBases, table)
		}
	}
	for i := 0; i < len(pendingBases); i++ {
		base := pendingBases[i].Info
		log := p.tables[base.MaterializedViewBase.MLogID]
		if log == nil || log.Info.State != model.StatePublic || log.Info.MaterializedViewLog == nil ||
			log.Info.MaterializedViewLog.BaseTableID != base.ID {
			continue
		}
		p.objects = append(p.objects, log)
		for _, id := range base.MaterializedViewBase.MViewIDs {
			view := p.tables[id]
			if view == nil || view.Info.State != model.StatePublic {
				continue
			}
			// Both sides must agree on the dependency. DDL or filtering may
			// leave a missing view, a plain table, or a link to another base's MV.
			mv := view.Info.MaterializedView
			if mv == nil || !mv.GetInitBuildState().IsReady() {
				continue
			}
			if len(mv.BaseTableIDs) != 1 {
				return errors.Errorf("materialized view %s must have exactly one base table", view.Info.Name.O)
			}
			if mv.BaseTableIDs[0] != base.ID {
				continue
			}
			p.objects = append(p.objects, view)
			if view.Info.MaterializedViewBase != nil {
				pendingBases = append(pendingBases, view)
			}
		}
	}
	return nil
}

// Drain utility statements as their errors and effects can be deferred to Next.
func executeMaterializedViewRestoreSQL(ctx context.Context, se glue.Session, sql string, args ...any) error {
	ctx = kv.WithInternalSourceType(ctx, kv.InternalTxnBR)
	if len(args) > 0 {
		var err error
		sql, err = sqlescape.EscapeSQL(sql, args...)
		if err != nil {
			return err
		}
	}
	vars := se.GetSessionCtx().GetSessionVars()
	// ExecuteInternal's parser disables NO_BACKSLASH_ESCAPES. Parse with the
	// source SQL mode so native restore and exported scripts interpret literals alike.
	p := parser.New()
	p.SetSQLMode(vars.SQLMode)
	p.SetParserConfig(vars.BuildParserConfig())
	charset, collation := vars.GetCharsetInfo()
	stmt, err := p.ParseOneStmt(sql, charset, collation)
	if err != nil {
		return errors.Trace(err)
	}
	internal := vars.InRestrictedSQL
	vars.InRestrictedSQL = true
	defer func() { vars.InRestrictedSQL = internal }()
	rs, err := se.GetSessionCtx().GetSQLExecutor().ExecuteStmt(ctx, stmt)
	if err != nil || rs == nil {
		return errors.Trace(err)
	}
	_, err = sqlexec.DrainRecordSet(ctx, rs, 1)
	closeErr := rs.Close()
	if err != nil {
		return errors.Trace(err)
	}
	return errors.Trace(closeErr)
}

const materializedViewRepairSQLFile = "materialized-views.sql"

func (p *materializedViewRestorePlan) restore(ctx context.Context, g glue.Glue, dom *domain.Domain, sqlStorage storage.ExternalStorage) error {
	ctx = kv.WithInternalSourceType(ctx, kv.InternalTxnBR)
	if len(p.objects) == 0 {
		return nil
	}
	se, err := g.CreateSession(dom.Store())
	if err != nil {
		return errors.Trace(err)
	}
	defer se.Close()
	// The first statement loads common global variables into a new session.
	// Capture that actual context before rendering either executable SQL or a file.
	if err := executeMaterializedViewRestoreSQL(ctx, se, "USE %n", p.objects[0].DB.Name.O); err != nil {
		return err
	}
	vars := se.GetSessionCtx().GetSessionVars()
	initialMode, initialZone, initialPrecision := vars.SQLMode, vars.TimeZone, vars.DivPrecisionIncrement
	initialTimeZone, ok := vars.GetSystemVar(variable.TimeZone)
	if !ok {
		return errors.New("restore session has no time_zone setting")
	}
	var output strings.Builder
	output.WriteString("-- Rebuild materialized views and logs skipped by PITR.\n")
	output.WriteString("-- Execute add-index.sql first if present, then this file in dependency order.\n")
	var tiFlashStoreCount *uint64
	for _, table := range p.objects {
		if replica := table.Info.TiFlashReplica; sqlStorage == nil && replica != nil {
			if tiFlashStoreCount == nil {
				count, err := infoschema.GetTiFlashStoreCount(dom.Store())
				if err != nil {
					return errors.Trace(err)
				}
				tiFlashStoreCount = &count
			}
			if replica.Count > *tiFlashStoreCount {
				// Match ordinary snapshot restore when the target lacks TiFlash capacity.
				log.Warn("skip materialized object TiFlash replica: insufficient stores",
					zap.String("table", table.DB.Name.O+"."+table.Info.Name.O),
					zap.Uint64("replicas", replica.Count), zap.Uint64("stores", *tiFlashStoreCount))
				clone := *table
				clone.Info = table.Info.Clone()
				clone.Info.TiFlashReplica = nil
				table = &clone
			}
		}
		vars.SQLMode, vars.TimeZone, vars.DivPrecisionIncrement = initialMode, initialZone, initialPrecision
		// Native CREATE MV/MLog has no IF NOT EXISTS syntax. Skip existing
		// objects here; secondary indexes use CREATE INDEX IF NOT EXISTS.
		_, err := dom.InfoSchema().TableByName(ctx, table.DB.Name, table.Info.Name)
		if err != nil && !infoschema.ErrTableNotExists.Equal(err) {
			return err
		}
		sqls, err := p.logicalObjectSQLs(ctx, se, table, infoschema.ErrTableNotExists.Equal(err), initialTimeZone)
		if err != nil {
			return errors.Annotatef(err, "failed to reconstruct materialized object %s.%s", table.DB.Name.O, table.Info.Name.O)
		}
		for _, sql := range sqls {
			if sqlStorage != nil {
				output.WriteString(sql)
				output.WriteString(";\n")
			} else if err := executeMaterializedViewRestoreSQL(ctx, se, sql); err != nil {
				return errors.Annotatef(err, "failed to restore materialized object %s.%s", table.DB.Name.O, table.Info.Name.O)
			}
		}
	}
	if sqlStorage != nil {
		return sqlStorage.WriteFile(ctx, materializedViewRepairSQLFile, []byte(output.String()))
	}
	return nil
}

// logicalObjectSQLs preserves definition-time session settings in both executed
// DDL and exported scripts. The same statements build each object and its indexes.
func (p *materializedViewRestorePlan) logicalObjectSQLs(ctx context.Context, se glue.Session, table *metautil.Table, create bool, timeZone string) ([]string, error) {
	vars := se.GetSessionCtx().GetSessionVars()
	oldMode, oldZone, oldPrecision := vars.SQLMode, vars.TimeZone, vars.DivPrecisionIncrement
	defer func() { vars.SQLMode, vars.TimeZone, vars.DivPrecisionIncrement = oldMode, oldZone, oldPrecision }()
	definitionSession, ok := se.(glue.MaterializedViewSession)
	if !ok {
		return nil, errors.New("restore session does not support materialized view definitions")
	}
	info := table.Info
	var base *model.TableInfo
	if mv := info.MaterializedView; mv != nil {
		vars.SQLMode = mv.DefinitionSQLMode
		vars.DivPrecisionIncrement = mv.DefinitionDivPrecisionIncrement
		location, err := mv.DefinitionTimeZone.GetLocation()
		if err != nil {
			return nil, errors.Trace(err)
		}
		vars.TimeZone = location
		timeZone = mv.DefinitionTimeZone.Name
		if offset := mv.DefinitionTimeZone.Offset; offset != 0 {
			sign := "+"
			if offset < 0 {
				sign, offset = "-", -offset
			}
			timeZone = fmt.Sprintf("%s%02d:%02d", sign, offset/3600, offset%3600/60)
		} else if timeZone == "" {
			timeZone = "+00:00"
		}
	} else {
		vars.SQLMode = info.MaterializedViewLog.PurgeScheduleSQLMode
		base = p.tables[info.MaterializedViewLog.BaseTableID].Info
	}
	var sqls []string
	appendSQL := func(sql string, args ...any) error {
		query, err := sqlescape.EscapeSQL(sql, args...)
		if err == nil {
			sqls = append(sqls, query)
		}
		return err
	}
	appendSQLMode := func(sqlMode mysql.SQLMode) error {
		var modes []string
		for name, mode := range mysql.Str2SQLMode {
			if mode != 0 && sqlMode&mode == mode {
				modes = append(modes, name)
			}
		}
		slices.Sort(modes)
		return appendSQL("SET SESSION sql_mode = %?", strings.Join(modes, ","))
	}
	if err := appendSQL("USE %n", table.DB.Name.O); err != nil {
		return nil, err
	}
	if err := appendSQLMode(vars.SQLMode); err != nil {
		return nil, err
	}
	if err := appendSQL("SET SESSION time_zone = %?", timeZone); err != nil {
		return nil, err
	}
	if err := appendSQL("SET SESSION div_precision_increment = %?", vars.DivPrecisionIncrement); err != nil {
		return nil, err
	}
	clusteredIndex := variable.Off
	if info.HasClusteredIndex() {
		clusteredIndex = variable.On
	}
	if err := appendSQL("SET SESSION tidb_enable_clustered_index = %?", clusteredIndex); err != nil {
		return nil, err
	}
	mv := info.MaterializedView
	// ALTER REFRESH can capture a different SQL mode from the query definition.
	// Such a schedule must be applied separately, including on a retry after CREATE.
	separateSchedule := mv != nil && mv.RefreshScheduleSQLMode != mv.DefinitionSQLMode &&
		(mv.RefreshStartWith != "" || mv.RefreshNext != "")
	if create {
		definition := info
		if separateSchedule {
			definition = info.Clone()
			definition.MaterializedView.RefreshStartWith = ""
			definition.MaterializedView.RefreshNext = ""
		}
		sql, err := definitionSession.GetCreateMaterializedView(definition, base)
		if err != nil {
			return nil, err
		}
		sqls = append(sqls, sql)
	}
	if replica := info.TiFlashReplica; replica != nil {
		// Derived objects are absent from the ordinary-table TiFlash recorder.
		// Apply their settings after CREATE, before building downstream MVs.
		stmt := &ast.AlterTableStmt{
			Table: &ast.TableName{Schema: table.DB.Name, Name: info.Name},
			Specs: []*ast.AlterTableSpec{{
				Tp: ast.AlterTableSetTiFlashReplica,
				TiFlashReplica: &ast.TiFlashReplicaSpec{
					Count: replica.Count, Labels: replica.LocationLabels,
				},
			}},
		}
		flags := format.DefaultRestoreFlags
		if !vars.SQLMode.HasNoBackslashEscapesMode() {
			flags |= format.RestoreStringEscapeBackslash
		}
		var sql strings.Builder
		if err := stmt.Restore(format.NewRestoreCtx(flags, &sql)); err != nil {
			return nil, err
		}
		sqls = append(sqls, sql.String())
	}
	if info.MaterializedView != nil {
		indexes, err := definitionSession.GetMaterializedViewIndexSQLs(ctx, table.DB.Name, info)
		if err != nil {
			return nil, err
		}
		sqls = append(sqls, indexes...)
	}
	if separateSchedule {
		if err := appendSQLMode(mv.RefreshScheduleSQLMode); err != nil {
			return nil, err
		}
		sql, err := definitionSession.GetMaterializedViewRefreshSQL(info)
		if err != nil {
			return nil, err
		}
		sqls = append(sqls, sql)
	}
	return sqls, nil
}

// repairMaterializedViews runs after data replay, index repair and TiFlash restoration. Native
// DDL builds the data and establishes all target IDs in dependency order.
func repairMaterializedViews(ctx context.Context, g glue.Glue, dom *domain.Domain, cfg *RestoreConfig, sr *stream.SchemasReplace) error {
	recorder := sr.MaterializedViewRecorder
	tables := make([]*metautil.Table, 0, len(recorder.Tables))
	for _, table := range recorder.Tables {
		db := sr.DbMap[table.DBID]
		if db == nil || !cfg.TableFilter.MatchTable(db.Name, table.Info.Name.O) {
			continue
		}
		actualDB, ok := dom.InfoSchema().SchemaByName(pmodel.NewCIStr(db.Name))
		if !ok || actualDB.ID != db.DbID {
			continue
		}
		if table.Info.MaterializedView == nil && table.Info.MaterializedViewLog == nil {
			replacement := db.TableMap[table.Info.ID]
			if replacement == nil {
				continue
			}
			actual, exists := dom.InfoSchema().TableByID(ctx, replacement.TableID)
			if !exists {
				continue
			}
			// Use the final base name after replay, including any rename.
			info := table.Info.Clone()
			info.Name = actual.Meta().Name
			tables = append(tables, &metautil.Table{DB: actualDB, Info: info})
		} else {
			tables = append(tables, &metautil.Table{DB: actualDB, Info: table.Info})
		}
	}
	slices.SortFunc(tables, func(a, b *metautil.Table) int { return cmp.Compare(a.Info.ID, b.Info.ID) })
	plan, derivedTables := indexMaterializedViewTables(tables)
	if err := plan.collectRestorableObjects(tables, derivedTables); err != nil {
		return err
	}
	return plan.restore(ctx, g, dom, cfg.RepairSQLStorage)
}
