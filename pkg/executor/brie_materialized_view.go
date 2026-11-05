// Copyright 2026 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"bytes"
	"context"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	pmodel "github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/sqlescape"
)

// BRIEMaterializedViewSession shares MV reconstruction between BR and SQL RESTORE.
type BRIEMaterializedViewSession struct{ sessionctx.Context }

var _ glue.MaterializedViewSession = (*BRIEMaterializedViewSession)(nil)

// GetCreateMaterializedView implements glue.MaterializedViewSession.
func (gs *BRIEMaterializedViewSession) GetCreateMaterializedView(table, base *model.TableInfo) (string, error) {
	table = table.Clone()
	mode := gs.GetSessionVars().SQLMode
	var err error
	var buf bytes.Buffer
	if mv := table.MaterializedView; mv != nil {
		mv.SQLContent, err = gs.materializedViewCanonicalSQL(mv.SQLContent, mode, false)
		if err != nil {
			return "", err
		}
		mv.RefreshStartWith, mv.RefreshNext, err = gs.materializedViewScheduleSQL(mv.RefreshStartWith, mv.RefreshNext, mode)
		if err != nil {
			return "", err
		}
		fetchShowCreateTable4MaterializedView(gs.Context, table, &buf)
	} else {
		log := table.MaterializedViewLog
		log.PurgeStartWith, log.PurgeNext, err = gs.materializedViewScheduleSQL(log.PurgeStartWith, log.PurgeNext, mode)
		if err != nil {
			return "", err
		}
		fetchShowCreateTable4MaterializedViewLog(gs.Context, base, table, &buf)
	}
	return buf.String(), nil
}

// GetMaterializedViewRefreshSQL renders an ALTER schedule under its own SQL mode.
func (gs *BRIEMaterializedViewSession) GetMaterializedViewRefreshSQL(table *model.TableInfo) (string, error) {
	mv := table.MaterializedView
	start, next, err := gs.materializedViewScheduleSQL(mv.RefreshStartWith, mv.RefreshNext, mv.RefreshScheduleSQLMode)
	if err != nil {
		return "", err
	}
	var sql strings.Builder
	if err := sqlescape.FormatSQL(&sql, "ALTER MATERIALIZED VIEW %n REFRESH", table.Name.O); err != nil {
		return "", err
	}
	if start != "" {
		sql.WriteString(" START WITH " + start)
	}
	if next != "" {
		sql.WriteString(" NEXT " + next)
	}
	return sql.String(), nil
}

func (gs *BRIEMaterializedViewSession) materializedViewScheduleSQL(start, next string, mode mysql.SQLMode) (string, string, error) {
	start, err := gs.materializedViewCanonicalSQL(start, mode, true)
	if err != nil {
		return "", "", err
	}
	next, err = gs.materializedViewCanonicalSQL(next, mode, true)
	return start, next, err
}

// Metadata stores canonical SQL with backslashes escaped even in
// NO_BACKSLASH_ESCAPES mode. Decode that representation before emitting SQL for
// the original mode; otherwise every rebuild changes the literal values.
func (gs *BRIEMaterializedViewSession) materializedViewCanonicalSQL(sql string, mode mysql.SQLMode, expression bool) (string, error) {
	if sql == "" || !mode.HasNoBackslashEscapesMode() {
		return sql, nil
	}
	p := parser.New()
	p.SetSQLMode(mode &^ mysql.ModeNoBackslashEscapes)
	p.SetParserConfig(gs.GetSessionVars().BuildParserConfig())
	if expression {
		sql = "SELECT " + sql
	}
	charset, collation := gs.GetSessionVars().GetCharsetInfo()
	stmt, err := p.ParseOneStmt(sql, charset, collation)
	if err != nil {
		return "", err
	}
	var node ast.Node = stmt
	if expression {
		node = stmt.(*ast.SelectStmt).Fields.Fields[0].Expr
	}
	node, _ = node.Accept(materializedViewLiteralVisitor{})
	var out strings.Builder
	if err := node.Restore(format.NewRestoreCtx(format.DefaultRestoreFlags|format.RestoreStringWithoutCharset, &out)); err != nil {
		return "", err
	}
	return out.String(), nil
}

type materializedViewLiteralVisitor struct{}

func (materializedViewLiteralVisitor) Enter(node ast.Node) (ast.Node, bool) {
	if value, ok := node.(ast.ValueExpr); ok {
		if _, isString := value.GetValue().(string); isString {
			return &materializedViewLiteral{ValueExpr: value}, true
		}
	}
	return node, false
}

func (materializedViewLiteralVisitor) Leave(node ast.Node) (ast.Node, bool) { return node, true }

// ValueExpr.Restore always doubles backslashes; SQL parsed in
// NO_BACKSLASH_ESCAPES mode needs the decoded string without that extra escaping.
type materializedViewLiteral struct{ ast.ValueExpr }

func (v *materializedViewLiteral) Restore(ctx *format.RestoreCtx) error {
	ctx.WriteString(v.GetString())
	return nil
}

// GetMaterializedViewIndexSQLs restores group-index settings before rebuilding
// secondary indexes added after CREATE MV.
func (gs *BRIEMaterializedViewSession) GetMaterializedViewIndexSQLs(ctx context.Context, dbName pmodel.CIStr, table *model.TableInfo) ([]string, error) {
	if table.MaterializedView == nil {
		return nil, nil
	}
	ctx = kv.WithInternalSourceType(ctx, kv.InternalTxnBR)
	vars := gs.Context.GetSessionVars()
	originalMode := vars.SQLMode
	vars.SQLMode = table.MaterializedView.DefinitionSQLMode
	defer func() { vars.SQLMode = originalMode }()
	var definition bytes.Buffer
	if err := ConstructResultOfShowCreateTable(gs.Context, table, autoid.Allocators{}, &definition); err != nil {
		return nil, err
	}
	node, err := gs.Context.GetRestrictedSQLExecutor().ParseWithParams(ctx, definition.String())
	if err != nil {
		return nil, err
	}
	definitionStmt := node.(*ast.CreateTableStmt)
	var sqls []string
	flags := format.DefaultRestoreFlags
	if !vars.SQLMode.HasNoBackslashEscapesMode() {
		flags |= format.RestoreStringEscapeBackslash
	}
	// Reconcile the implicit grouping index even if the source dropped it.
	// A secondary index may reuse its default name.
	groupStmts, err := gs.materializedViewGroupIndexStatements(ctx, dbName, table, definitionStmt)
	if err != nil {
		return nil, err
	}
	for _, stmt := range groupStmts {
		var sql bytes.Buffer
		if err := stmt.Restore(format.NewRestoreCtx(flags, &sql)); err != nil {
			return nil, err
		}
		sqls = append(sqls, sql.String())
	}
	for _, constraint := range definitionStmt.Constraints {
		// Additional unique indexes and indexes that introduce writable columns
		// are forbidden on MVs.
		if constraint.Tp != ast.ConstraintKey && constraint.Tp != ast.ConstraintIndex {
			continue
		}
		if constraint.Option != nil {
			// SHOW CREATE escapes control characters independently of SQL mode.
			// Use the raw comment, then escape it for the execution SQL mode below.
			constraint.Option.Comment = table.FindIndexByName(pmodel.NewCIStr(constraint.Name).L).Comment
		}
		stmt := &ast.CreateIndexStmt{
			IfNotExists:             true,
			Table:                   &ast.TableName{Schema: dbName, Name: table.Name},
			IndexName:               constraint.Name,
			IndexPartSpecifications: constraint.Keys,
			IndexOption:             constraint.Option,
		}
		var sql bytes.Buffer
		if err := stmt.Restore(format.NewRestoreCtx(flags, &sql)); err != nil {
			return nil, err
		}
		sqls = append(sqls, sql.String())
	}
	return sqls, nil
}

func (gs *BRIEMaterializedViewSession) materializedViewGroupIndexStatements(ctx context.Context, dbName pmodel.CIStr,
	table *model.TableInfo, definition *ast.CreateTableStmt,
) ([]*ast.AlterTableStmt, error) {
	var sourceConstraint *ast.Constraint
	for _, constraint := range definition.Constraints {
		switch constraint.Tp {
		case ast.ConstraintPrimaryKey, ast.ConstraintUniq, ast.ConstraintUniqKey, ast.ConstraintUniqIndex:
			sourceConstraint = constraint
		}
	}
	// SHOW CREATE renders PRIMARY KEY without its possibly renamed index name.
	// Additional unique indexes are forbidden, so the raw unique index is the
	// source grouping index. Integer clustered primary keys have no IndexInfo.
	var source *model.IndexInfo
	for _, index := range table.Indices {
		if index.Unique {
			source = index
			break
		}
	}
	if sourceConstraint != nil && sourceConstraint.Tp == ast.ConstraintPrimaryKey && source == nil {
		return nil, nil
	}
	var current *model.TableInfo
	actual, err := domain.GetDomain(gs.Context).InfoSchema().TableByName(ctx, dbName, table.Name)
	switch {
	case err == nil:
		// A checkpoint retry may already have renamed or dropped the grouping index.
		current = actual.Meta()
	case infoschema.ErrTableNotExists.Equal(err):
		// Reuse native DDL naming, including special names such as PRIMARY.
		// Only the implicit grouping constraint exists when CREATE MV runs.
		var implicit *ast.Constraint
		if sourceConstraint != nil {
			clone := *sourceConstraint
			clone.Name, clone.Option = "", nil
			if clone.Tp == ast.ConstraintPrimaryKey {
				pkType := pmodel.PrimaryKeyTypeNonClustered
				if table.HasClusteredIndex() {
					pkType = pmodel.PrimaryKeyTypeClustered
				}
				clone.Option = &ast.IndexOption{PrimaryKeyTp: pkType}
			}
			implicit = &clone
		} else {
			// With no source grouping index, recover its original keys from the
			// query so the native DDL builder can identify the index to remove.
			implicit, err = gs.materializedViewGroupConstraint(ctx, table)
			if err != nil {
				return nil, err
			}
		}
		current, err = ddl.BuildTableInfoWithStmt(ddl.NewMetaBuildContextWithSctx(gs.Context), &ast.CreateTableStmt{
			Table: definition.Table, Cols: definition.Cols, Constraints: []*ast.Constraint{implicit},
		}, table.Charset, table.Collate, nil)
		if err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	var groupIndex *model.IndexInfo
	for _, index := range current.Indices {
		if index.Unique {
			groupIndex = index
			break
		}
	}
	if groupIndex == nil {
		if sourceConstraint == nil {
			return nil, nil // A previous attempt already dropped the implicit index.
		}
		return nil, errors.Errorf("materialized view %s.%s has no grouping index", dbName.O, table.Name.O)
	}
	var stmts []*ast.AlterTableStmt
	appendSpec := func(spec *ast.AlterTableSpec) {
		stmts = append(stmts, &ast.AlterTableStmt{
			Table: &ast.TableName{Schema: dbName, Name: table.Name}, Specs: []*ast.AlterTableSpec{spec},
		})
	}
	if sourceConstraint == nil {
		spec := &ast.AlterTableSpec{Tp: ast.AlterTableDropIndex, Name: groupIndex.Name.O}
		if groupIndex.Primary {
			spec.Tp = ast.AlterTableDropPrimaryKey
		}
		appendSpec(spec)
		return stmts, nil
	}
	if groupIndex.Name.O != source.Name.O {
		appendSpec(&ast.AlterTableSpec{Tp: ast.AlterTableRenameIndex, FromKey: groupIndex.Name, ToKey: source.Name})
	}
	if groupIndex.Invisible != source.Invisible {
		visibility := ast.IndexVisibilityVisible
		if source.Invisible {
			visibility = ast.IndexVisibilityInvisible
		}
		appendSpec(&ast.AlterTableSpec{Tp: ast.AlterTableIndexInvisible, IndexName: source.Name, Visibility: visibility})
	}
	return stmts, nil
}

// materializedViewGroupConstraint reconstructs the implicit index keys when the
// source dropped that index. MV grouping keys are direct columns, each selected
// once; their SELECT offsets identify the corresponding physical output columns.
func (gs *BRIEMaterializedViewSession) materializedViewGroupConstraint(ctx context.Context, table *model.TableInfo) (*ast.Constraint, error) {
	node, err := gs.Context.GetRestrictedSQLExecutor().ParseWithParams(ctx, table.MaterializedView.SQLContent)
	if err != nil {
		return nil, err
	}
	query, ok := node.(*ast.SelectStmt)
	if !ok || query.GroupBy == nil || query.Fields == nil {
		return nil, errors.Errorf("invalid materialized view grouping query for %s", table.Name.O)
	}
	columns := make(map[string]*model.ColumnInfo)
	for i, field := range query.Fields.Fields {
		if col, ok := field.Expr.(*ast.ColumnNameExpr); ok && i < len(table.Columns) {
			columns[col.Name.Name.L] = table.Columns[i]
		}
	}
	constraint := &ast.Constraint{Tp: ast.ConstraintPrimaryKey}
	for _, item := range query.GroupBy.Items {
		col, ok := item.Expr.(*ast.ColumnNameExpr)
		if !ok || columns[col.Name.Name.L] == nil {
			return nil, errors.Errorf("invalid materialized view grouping column for %s", table.Name.O)
		}
		output := columns[col.Name.Name.L]
		constraint.Keys = append(constraint.Keys, &ast.IndexPartSpecification{
			Column: &ast.ColumnName{Name: output.Name}, Length: types.UnspecifiedLength,
		})
		if !mysql.HasNotNullFlag(output.GetFlag()) {
			constraint.Tp = ast.ConstraintUniq
		}
	}
	// A dropped primary index must have been nonclustered. Match the restored
	// object's OFF setting even if this rendering session has a different default.
	if constraint.Tp == ast.ConstraintPrimaryKey {
		constraint.Option = &ast.IndexOption{PrimaryKeyTp: pmodel.PrimaryKeyTypeNonClustered}
	}
	return constraint, nil
}
