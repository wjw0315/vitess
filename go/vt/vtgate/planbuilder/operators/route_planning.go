/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package operators

import (
	"bytes"
	"io"

	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
	"vitess.io/vitess/go/vt/vtgate/semantics"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
)

type (
	tableSetPair struct {
		left, right semantics.TableSet
	}

	opCacheMap map[tableSetPair]Operator
)

func pushDerived(ctx *plancontext.PlanningContext, op *Horizon) (Operator, *ApplyResult) {
	innerRoute, ok := op.Source.(*Route)
	if !ok {
		return op, NoRewrite
	}

	if !(innerRoute.Routing.OpCode() == engine.EqualUnique) && !op.IsMergeable(ctx) {
		// no need to check anything if we are sure that we will only hit a single shard
		return op, NoRewrite
	}

	return Swap(op, op.Source, "push derived under route")
}

func optimizeJoin(ctx *plancontext.PlanningContext, op *Join) (Operator, *ApplyResult) {
	if newOp := op.tryCompact(ctx); newOp != nil {
		return newOp, Rewrote("merged query graphs")
	}
	return mergeOrJoin(ctx, op.LHS, op.RHS, sqlparser.SplitAndExpression(nil, op.Predicate), op.JoinType)
}

func optimizeQueryGraph(ctx *plancontext.PlanningContext, op *QueryGraph) (result Operator, changed *ApplyResult) {
	switch ctx.PlannerVersion {
	case querypb.ExecuteOptions_Gen4Left2Right:
		result = leftToRightSolve(ctx, op)
	default:
		result = greedySolve(ctx, op)
	}

	unresolved := op.UnsolvedPredicates(ctx.SemTable)
	if len(unresolved) > 0 {
		// if we have any predicates that none of the joins or tables took care of,
		// we add a single filter on top, so we don't lose it. This is used for sub-query planning
		result = newFilter(result, ctx.SemTable.AndExpressions(unresolved...))
	}

	changed = Rewrote("solved query graph")
	return
}

func buildVindexTableForDML(
	ctx *plancontext.PlanningContext,
	tableInfo semantics.TableInfo,
	table *QueryTable,
	ins *sqlparser.Insert,
	dmlType string,
) (*vindexes.BaseTable, Routing) {
	vindexTable := tableInfo.GetVindexTable()
	if tableInfo.GetVindexTable().Type == vindexes.TypeReference && vindexTable.Source != nil {
		sourceTable, _, _, _, _, err := ctx.VSchema.FindTableOrVindex(vindexTable.Source.TableName)
		if err != nil {
			panic(err)
		}
		vindexTable = sourceTable
		refTbl := sqlparser.NewAliasedTableExpr(vindexTable.GetTableName(), "")
		ins.Table.Expr = refTbl.Expr
		// We don't need to process the alias because you cannot define aliases for inserts.
	}

	if !vindexTable.Keyspace.Sharded {
		return vindexTable, &AnyShardRouting{keyspace: vindexTable.Keyspace}
	}

	tblName, ok := table.Alias.Expr.(sqlparser.TableName)
	if !ok {
		panic(vterrors.VT12001("multi shard UPDATE with LIMIT"))
	}

	_, _, _, typ, dest, err := ctx.VSchema.FindTableOrVindex(tblName)
	if err != nil {
		panic(err)
	}
	if dest == nil {
		routing := &ShardedRouting{
			keyspace:    vindexTable.Keyspace,
			RouteOpCode: engine.Scatter,
		}
		return vindexTable, routing
	}

	if typ != topodatapb.TabletType_PRIMARY {
		panic(vterrors.VT09002(dmlType))
	}

	// we are dealing with an explicitly targeted DML
	routing := &TargetedRouting{
		keyspace:          vindexTable.Keyspace,
		TargetDestination: dest,
	}
	return vindexTable, routing
}

/*
		The greedy planner will plan a query by finding first finding the best route plan for every table.
	    Then, iteratively, it finds the cheapest join that can be produced between the remaining plans,
		and removes the two inputs to this cheapest plan and instead adds the join.
		As an optimization, it first only considers joining tables that have predicates defined between them
*/
func greedySolve(ctx *plancontext.PlanningContext, qg *QueryGraph) Operator {
	routeOps := seedOperatorList(ctx, qg)
	planCache := opCacheMap{}

	return mergeRoutes(ctx, qg, routeOps, planCache, false)
}

func leftToRightSolve(ctx *plancontext.PlanningContext, qg *QueryGraph) Operator {
	plans := seedOperatorList(ctx, qg)

	var acc Operator
	for _, plan := range plans {
		if acc == nil {
			acc = plan
			continue
		}
		joinPredicates := qg.GetPredicates(TableID(acc), TableID(plan))
		acc, _ = mergeOrJoin(ctx, acc, plan, joinPredicates, sqlparser.NormalJoinType)
	}

	return acc
}

// seedOperatorList returns a route for each table in the qg
func seedOperatorList(ctx *plancontext.PlanningContext, qg *QueryGraph) []Operator {
	plans := make([]Operator, len(qg.Tables))

	// we start by seeding the table with the single routes
	for i, table := range qg.Tables {
		plan := createRoute(ctx, table)
		if qg.NoDeps != nil {
			plan = plan.AddPredicate(ctx, qg.NoDeps)
		}
		plans[i] = plan
	}
	return plans
}

func createInfSchemaRoute(ctx *plancontext.PlanningContext, table *QueryTable) Operator {
	ks, err := ctx.VSchema.AnyKeyspace()
	if err != nil {
		panic(err)
	}
	var src Operator = &Table{
		QTable: table,
		VTable: &vindexes.BaseTable{
			Name:     table.Table.Name,
			Keyspace: ks,
		},
	}
	var routing Routing = &InfoSchemaRouting{}
	for _, pred := range table.Predicates {
		routing = UpdateRoutingLogic(ctx, pred, routing)
	}
	return &Route{
		unaryOperator: newUnaryOp(src),
		Routing:       routing,
	}
}

func mergeRoutes(ctx *plancontext.PlanningContext, qg *QueryGraph, physicalOps []Operator, planCache opCacheMap, crossJoinsOK bool) Operator {
	if len(physicalOps) == 0 {
		return nil
	}
	for len(physicalOps) > 1 {
		bestTree, lIdx, rIdx := findBestJoin(ctx, qg, physicalOps, planCache, crossJoinsOK)
		// if we found a plan, we'll replace the two plans that were joined with the join plan created
		if bestTree != nil {
			// we remove one plan, and replace the other
			if rIdx > lIdx {
				physicalOps = removeAt(physicalOps, rIdx)
				physicalOps = removeAt(physicalOps, lIdx)
			} else {
				physicalOps = removeAt(physicalOps, lIdx)
				physicalOps = removeAt(physicalOps, rIdx)
			}
			physicalOps = append(physicalOps, bestTree)
		} else {
			if crossJoinsOK {
				panic(vterrors.VT13001("should not happen: we should be able to merge cross joins"))
			}
			// we will only fail to find a join plan when there are only cross joins left
			// when that happens, we switch over to allow cross joins as well.
			// this way we prioritize joining physicalOps with predicates first
			crossJoinsOK = true
		}
	}
	return physicalOps[0]
}

func removeAt(plans []Operator, idx int) []Operator {
	return append(plans[:idx], plans[idx+1:]...)
}

func findBestJoin(
	ctx *plancontext.PlanningContext,
	qg *QueryGraph,
	plans []Operator,
	planCache opCacheMap,
	crossJoinsOK bool,
) (bestPlan Operator, lIdx int, rIdx int) {
	for i, lhs := range plans {
		for j, rhs := range plans {
			if i == j {
				continue
			}
			joinPredicates := qg.GetPredicates(TableID(lhs), TableID(rhs))
			if len(joinPredicates) == 0 && !crossJoinsOK {
				// if there are no predicates joining the two tables,
				// creating a join between them would produce a
				// cartesian product, which is almost always a bad idea
				continue
			}
			plan := getJoinFor(ctx, planCache, lhs, rhs, joinPredicates)
			if bestPlan == nil || CostOf(plan) < CostOf(bestPlan) {
				bestPlan = plan
				// remember which plans we based on, so we can remove them later
				lIdx = i
				rIdx = j
			}
		}
	}
	return bestPlan, lIdx, rIdx
}

func getJoinFor(ctx *plancontext.PlanningContext, cm opCacheMap, lhs, rhs Operator, joinPredicates []sqlparser.Expr) Operator {
	solves := tableSetPair{left: TableID(lhs), right: TableID(rhs)}
	cachedPlan := cm[solves]
	if cachedPlan != nil {
		return cachedPlan
	}

	join, _ := mergeOrJoin(ctx, lhs, rhs, joinPredicates, sqlparser.NormalJoinType)
	cm[solves] = join
	return join
}

// requiresSwitchingSides will return true if any of the operators with the root from the given operator tree
// is of the type that should not be on the RHS of a join
func requiresSwitchingSides(ctx *plancontext.PlanningContext, op Operator) (required bool) {
	_ = Visit(op, func(current Operator) error {
		horizon, isHorizon := current.(*Horizon)

		if isHorizon && !horizon.IsMergeable(ctx) {
			required = true
			return io.EOF
		}

		return nil
	})
	return
}

func mergeOrJoin(ctx *plancontext.PlanningContext, lhs, rhs Operator, joinPredicates []sqlparser.Expr, joinType sqlparser.JoinType) (Operator, *ApplyResult) {
	jm := newJoinMerge(joinPredicates, joinType)
	newPlan := jm.mergeJoinInputs(ctx, lhs, rhs)
	if newPlan != nil {
		return newPlan, Rewrote("merge routes into single operator")
	}

	if len(joinPredicates) > 0 && requiresSwitchingSides(ctx, rhs) {
		if !joinType.IsCommutative() || requiresSwitchingSides(ctx, lhs) {
			// we can't switch sides, so let's see if we can use a HashJoin to solve it
			join := NewHashJoin(lhs, rhs, !joinType.IsInner())
			for _, pred := range joinPredicates {
				join.AddJoinPredicate(ctx, pred, true)
			}
			ctx.SemTable.QuerySignature.HashJoin = true
			return join, Rewrote("use a hash join because we have LIMIT on the LHS")
		}

		join := NewApplyJoin(ctx, Clone(rhs), Clone(lhs), nil, joinType, false)
		for _, pred := range joinPredicates {
			join.AddJoinPredicate(ctx, pred, true)
		}
		return join, Rewrote("logical join to applyJoin, switching side because LIMIT")
	}

	join := NewApplyJoin(ctx, Clone(lhs), Clone(rhs), nil, joinType, false)
	for _, pred := range joinPredicates {
		join.AddJoinPredicate(ctx, pred, true)
	}

	return join, Rewrote("logical join to applyJoin ")
}

func operatorsToRoutes(a, b Operator) (*Route, *Route) {
	aRoute, ok := a.(*Route)
	if !ok {
		return nil, nil
	}
	bRoute, ok := b.(*Route)
	if !ok {
		return nil, nil
	}
	return aRoute, bRoute
}

func canMergeOnFilter(ctx *plancontext.PlanningContext, a, b *Route, predicate sqlparser.Expr) bool {
	comparison, ok := predicate.(*sqlparser.ComparisonExpr)
	if !ok {
		return false
	}
	if comparison.Operator != sqlparser.EqualOp {
		return false
	}
	left := comparison.Left
	right := comparison.Right

	lVindex := findColumnVindex(ctx, a, left)
	if lVindex == nil {
		left, right = right, left
		lVindex = findColumnVindex(ctx, a, left)
	}
	if lVindex == nil || !lVindex.IsUnique() {
		return false
	}
	rVindex := findColumnVindex(ctx, b, right)
	if rVindex == nil {
		return false
	}
	return rVindex == lVindex
}

func findColumnVindex(ctx *plancontext.PlanningContext, a Operator, exp sqlparser.Expr) vindexes.SingleColumn {
	_, isCol := exp.(*sqlparser.ColName)
	if !isCol {
		return nil
	}

	exp = unwrapDerivedTables(ctx, exp)
	if exp == nil {
		return nil
	}

	var singCol vindexes.SingleColumn

	// for each equality expression that exp has with other column name, we check if it
	// can be solved by any table in our routeTree. If an equality expression can be solved,
	// we check if the equality expression and our table share the same vindex, if they do:
	// the method will return the associated vindexes.SingleColumn.
	_ = ctx.SemTable.ForeachExprEquality(exp, func(expr sqlparser.Expr) error {
		col, isCol := expr.(*sqlparser.ColName)
		if !isCol {
			return nil
		}
		deps := ctx.SemTable.RecursiveDeps(expr)
		_ = Visit(a, func(rel Operator) error {
			to, isTableOp := rel.(tableIDIntroducer)
			if !isTableOp {
				return nil
			}
			id := to.introducesTableID()
			if deps.IsSolvedBy(id) {
				tableInfo, err := ctx.SemTable.TableInfoFor(id)
				if err != nil {
					// an error here is OK, we just can't ask this operator about its column vindexes
					return nil
				}
				vtable := tableInfo.GetVindexTable()
				if vtable != nil {
					for _, vindex := range vtable.ColumnVindexes {
						sC, isSingle := vindex.Vindex.(vindexes.SingleColumn)
						if isSingle && vindex.Columns[0].Equal(col.Name) {
							singCol = sC
							return io.EOF
						}
					}
				}
			}
			return nil
		})
		if singCol != nil {
			return io.EOF
		}
		return nil
	})

	return singCol
}

// unwrapDerivedTables we want to find the bottom layer of derived tables
// nolint
func unwrapDerivedTables(ctx *plancontext.PlanningContext, exp sqlparser.Expr) sqlparser.Expr {
	for {
		// if we are dealing with derived tables in derived tables
		tbl, err := ctx.SemTable.TableInfoForExpr(exp)
		if err != nil {
			return nil
		}
		_, ok := tbl.(*semantics.DerivedTable)
		if !ok {
			break
		}

		exp = semantics.RewriteDerivedTableExpression(exp, tbl)
		if col := getColName(exp); col != nil {
			exp = col
		} else {
			return nil
		}
	}
	return exp
}

func getColName(exp sqlparser.Expr) *sqlparser.ColName {
	switch exp := exp.(type) {
	case *sqlparser.ColName:
		return exp
	case *sqlparser.Max, *sqlparser.Min:
		aggr := exp.(sqlparser.AggrFunc).GetArg()
		return getColName(aggr)
	}
	// for any other expression than a column, or the extremum of a column, we return nil
	return nil
}

func canMergeOnFilters(ctx *plancontext.PlanningContext, a, b *Route, joinPredicates []sqlparser.Expr) bool {
	for _, predicate := range joinPredicates {
		for _, expr := range sqlparser.SplitAndExpression(nil, predicate) {
			if canMergeOnFilter(ctx, a, b, expr) {
				return true
			}
		}
	}
	return false
}

func gen4ValuesEqual(ctx *plancontext.PlanningContext, a, b []sqlparser.Expr) (bool, []engine.Condition) {
	if len(a) != len(b) {
		return false, nil
	}

	// TODO: check SemTable's columnEqualities for better plan
	var conditions []engine.Condition
	for i, aExpr := range a {
		bExpr := b[i]
		equal, c := gen4ValEqual(ctx, aExpr, bExpr)
		if !equal {
			return false, nil
		}
		if c != nil {
			conditions = append(conditions, *c)
		}
	}
	return true, conditions
}

func gen4ValEqual(ctx *plancontext.PlanningContext, a, b sqlparser.Expr) (bool, *engine.Condition) {
	switch a := a.(type) {
	case *sqlparser.ColName:
		if b, ok := b.(*sqlparser.ColName); ok {
			if !a.Name.Equal(b.Name) {
				return false, nil
			}

			return ctx.SemTable.DirectDeps(a) == ctx.SemTable.DirectDeps(b), nil
		}
	case *sqlparser.Argument:
		b, ok := b.(*sqlparser.Argument)
		if !ok {
			return false, nil
		}
		if a.Name == b.Name {
			return true, nil
		}

		bindVars := ctx.VSchema.GetBindVars()
		if bindVars == nil {
			return false, nil
		}

		aVal, ok := bindVars[a.Name]
		if !ok {
			return false, nil
		}
		bVal, ok := bindVars[b.Name]
		if !ok {
			return false, nil
		}

		return aVal.Type == bVal.Type && bytes.Equal(aVal.Value, bVal.Value),
			&engine.Condition{A: a.Name, B: b.Name}

	case *sqlparser.Literal:
		b, ok := b.(*sqlparser.Literal)
		if !ok {
			return false, nil
		}
		switch a.Type {
		case sqlparser.StrVal:
			switch b.Type {
			case sqlparser.StrVal:
				return a.Val == b.Val, nil
			case sqlparser.HexVal:
				return hexEqual(b, a), nil
			}
		case sqlparser.HexVal:
			return hexEqual(a, b), nil
		case sqlparser.IntVal:
			if b.Type == (sqlparser.IntVal) {
				return a.Val == b.Val, nil
			}
		}
	}
	return false, nil
}

func hexEqual(a, b *sqlparser.Literal) bool {
	v, err := a.HexDecode()
	if err != nil {
		return false
	}
	switch b.Type {
	case sqlparser.StrVal:
		return bytes.Equal(v, b.Bytes())
	case sqlparser.HexVal:
		v2, err := b.HexDecode()
		if err != nil {
			return false
		}
		return bytes.Equal(v, v2)
	}
	return false
}
