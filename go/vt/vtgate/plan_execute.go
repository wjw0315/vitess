/*
Copyright 2019 The Vitess Authors.

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

package vtgate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/log"
	querypb "vitess.io/vitess/go/vt/proto/query"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	econtext "vitess.io/vitess/go/vt/vtgate/executorcontext"
	"vitess.io/vitess/go/vt/vtgate/logstats"
	"vitess.io/vitess/go/vt/vtgate/vtgateservice"
)

type planExec func(ctx context.Context, plan *engine.Plan, vc *econtext.VCursorImpl, bindVars map[string]*querypb.BindVariable, startTime time.Time) error
type txResult func(sqlparser.StatementType, *sqltypes.Result) error

var vschemaWaitTimeout = 30 * time.Second

func waitForNewerVSchema(ctx context.Context, e *Executor, lastVSchemaCreated time.Time, timeout time.Duration) bool {
	pollingInterval := 10 * time.Millisecond
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	ticker := time.NewTicker(pollingInterval)
	defer ticker.Stop()
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			return false
		case <-ticker.C:
			if e.VSchema() != nil && e.VSchema().GetCreated().After(lastVSchemaCreated) {
				return true
			}
		}
	}
}

const MaxBufferingRetries = 3

func (e *Executor) newExecute(
	ctx context.Context,
	mysqlCtx vtgateservice.MySQLConnection,
	safeSession *econtext.SafeSession,
	sql string,
	bindVars map[string]*querypb.BindVariable,
	prepared bool,
	logStats *logstats.LogStats,
	execPlan planExec, // used when there is a plan to execute
	recResult txResult, // used when it's something simple like begin/commit/rollback/savepoint
) (err error) {
	// Start an implicit transaction if necessary.
	err = e.startTxIfNecessary(ctx, safeSession)
	if err != nil {
		return err
	}

	if bindVars == nil {
		bindVars = make(map[string]*querypb.BindVariable)
	}

	var (
		vs                 = e.VSchema()
		lastVSchemaCreated = vs.GetCreated()
		result             *sqltypes.Result
		plan               *engine.Plan
		vcursor            *econtext.VCursorImpl
		stmt               sqlparser.Statement
		cancel             context.CancelFunc
	)

	for try := 0; try < MaxBufferingRetries; try++ {
		if try > 0 && !vs.GetCreated().After(lastVSchemaCreated) { // We need to wait for a vschema update
			// Without a wait we fail non-deterministically since the previous vschema will not have
			// the updated routing rules.
			// We retry MaxBufferingRetries-1 (2) times before giving up. How long we wait before each retry
			// -- IF we don't see a newer vschema come in -- affects how long we retry in total and how quickly
			// we retry the query and (should) succeed when the traffic switch fails or we otherwise hit the
			// max buffer failover time without resolving the keyspace event and marking it as consistent.
			// This calculation attemps to ensure that we retry at a sensible interval and number of times
			// based on the buffering configuration. This way we should be able to perform the max retries
			// within the given window of time for most queries and we should not end up waiting too long
			// after the traffic switch fails or the buffer window has ended, retrying old queries.
			timeout := vschemaWaitTimeout
			if e.resolver.scatterConn.gateway.buffer != nil && e.resolver.scatterConn.gateway.buffer.GetConfig() != nil {
				timeout = e.resolver.scatterConn.gateway.buffer.GetConfig().MaxFailoverDuration / (MaxBufferingRetries - 1)
			}
			if waitForNewerVSchema(ctx, e, lastVSchemaCreated, timeout) {
				vs = e.VSchema()
				lastVSchemaCreated = vs.GetCreated()
			}
		}

		// Enable parameterization if normalization is enabled and the query is not prepared statement.
		parameterize := e.config.Normalize && !prepared

		// Create a plan for the query.
		// If we are retrying, it is likely that the routing rules have changed and hence we need to
		// replan the query since the target keyspace of the resolved shards may have changed as a
		// result of MoveTables SwitchTraffic which does a RebuildSrvVSchema which in turn causes
		// the vtgate to clear the cached plans when processing the new serving vschema.
		// When buffering ends, many queries might be getting planned at the same time and we then
		// take full advatange of the cached plan.
		plan, vcursor, stmt, err = e.fetchOrCreatePlan(ctx, safeSession, sql, bindVars, parameterize, prepared, logStats, true)
		execStart := e.logPlanningFinished(logStats, plan)

		if err != nil {
			safeSession.ClearWarnings()
			return err
		}

		if plan.QueryType != sqlparser.StmtShow {
			safeSession.ClearWarnings()
		}

		// Add any warnings that the planner wants to add.
		for _, warning := range plan.Warnings {
			safeSession.RecordWarning(warning)
		}

		// set the overall query timeout if it is not already set
		ctx, cancel = vcursor.GetContextWithTimeOut(ctx)
		defer cancel()

		// If we have previously issued a VT15001 error, we block any new queries on this session until we receive a ROLLBACK or "show warnings".
		if shouldBlockQueries(plan, safeSession) {
			return vterrors.VT09032()
		}

		result, err = e.handleTransactions(ctx, mysqlCtx, safeSession, plan, logStats, vcursor, stmt)
		if err != nil {
			return err
		}
		if result != nil {
			return recResult(plan.QueryType, result)
		}

		// Prepare for execution.
		err = e.addNeededBindVars(vcursor, plan.BindVarNeeds, bindVars, safeSession)
		if err != nil {
			logStats.Error = err
			return err
		}

		// Execute the plan.
		if plan.Instructions.NeedsTransaction() {
			err = e.insideTransaction(ctx, safeSession, logStats,
				func() error {
					return execPlan(ctx, plan, vcursor, bindVars, execStart)
				})
		} else {
			err = execPlan(ctx, plan, vcursor, bindVars, execStart)
		}

		if err == nil || safeSession.InTransaction() {
			return err
		}

		// Retry if needed.
		rootCause := vterrors.RootCause(err)
		if rootCause != nil && strings.Contains(rootCause.Error(), "enforce denied tables") {
			log.V(2).Infof("Retry: %d, will retry query %s due to %v", try, sql, err)
			if try == 0 { // We are going to retry at least once
				defer func() {
					// Prevent any plan cache pollution from queries planned against the wrong keyspace during a MoveTables
					// traffic switching operation.
					if err != nil { // The error we're checking here is the return value from the newExecute function
						cause := vterrors.RootCause(err)
						if cause != nil && strings.Contains(cause.Error(), "enforce denied tables") {
							// The executor's VSchemaManager clears the plan cache when it receives a new vschema via its
							// SrvVSchema watcher (it calls executor.SaveVSchema() in its watch's subscriber callback). This
							// happens concurrently with the KeyspaceEventWatcher also receiving the new vschema in its
							// SrvVSchema watcher and in its subscriber callback processing it (which includes getting info
							// on all shards from the topo), and eventually determining that the keyspace is consistent and
							// ending the buffering window. So there's race with query retries such that a query could be
							// planned against the wrong side just as the keyspace event is getting resolved and the buffers
							// drained. Then that bad plan is the cached plan for the query until you do another
							// topo.RebuildSrvVSchema/vtctldclient RebuildVSchemaGraph which then causes the VSchemaManager
							// to clear the plan cache. It's essentially a race between the two SrvVSchema watchers and the
							// work they do when a new one is received. If we DID a retry AND the last time we retried
							// still encountered the error, we know that the plan used was 1) not valid/correct and going to
							// the wrong side of the traffic switch as it failed with the denied tables error and 2) it will
							// remain the plan in the cache if we do not clear the plans after it was added to to the cache.
							// So here we clear the plan cache in order to prevent this scenario where the bad plan is
							// cached indefinitely and re-used after the buffering window ends and the keyspace event is
							// resolved.
							e.ClearPlans()
						}
					}
				}()
			}
			continue
		}

		return err
	}
	return vterrors.New(vtrpcpb.Code_INTERNAL, fmt.Sprintf("query %s failed after retries: %v ", sql, err))
}

// handleTransactions deals with transactional queries: begin, commit, rollback and savepoint management
func (e *Executor) handleTransactions(
	ctx context.Context,
	mysqlCtx vtgateservice.MySQLConnection,
	safeSession *econtext.SafeSession,
	plan *engine.Plan,
	logStats *logstats.LogStats,
	vcursor *econtext.VCursorImpl,
	stmt sqlparser.Statement,
) (*sqltypes.Result, error) {
	// We need to explicitly handle errors, and begin/commit/rollback, since these control transactions. Everything else
	// will fall through and be handled through planning
	switch plan.QueryType {
	case sqlparser.StmtBegin:
		qr, err := e.handleBegin(ctx, vcursor, safeSession, logStats, stmt)
		return qr, err
	case sqlparser.StmtCommit:
		qr, err := e.handleCommit(ctx, vcursor, safeSession, logStats)
		return qr, err
	case sqlparser.StmtRollback:
		qr, err := e.handleRollback(ctx, vcursor, safeSession, logStats)
		return qr, err
	case sqlparser.StmtSavepoint:
		qr, err := e.handleSavepoint(ctx, vcursor, safeSession, plan.Original, plan.QueryType.String(), logStats, func(_ string) (*sqltypes.Result, error) {
			// Safely to ignore as there is no transaction.
			return &sqltypes.Result{}, nil
		}, vcursor.IgnoreMaxMemoryRows())
		return qr, err
	case sqlparser.StmtSRollback:
		qr, err := e.handleSavepoint(ctx, vcursor, safeSession, plan.Original, plan.QueryType.String(), logStats, func(query string) (*sqltypes.Result, error) {
			// Error as there is no transaction, so there is no savepoint that exists.
			return nil, vterrors.NewErrorf(vtrpcpb.Code_NOT_FOUND, vterrors.SPDoesNotExist, "SAVEPOINT does not exist: %s", query)
		}, vcursor.IgnoreMaxMemoryRows())
		return qr, err
	case sqlparser.StmtRelease:
		qr, err := e.handleSavepoint(ctx, vcursor, safeSession, plan.Original, plan.QueryType.String(), logStats, func(query string) (*sqltypes.Result, error) {
			// Error as there is no transaction, so there is no savepoint that exists.
			return nil, vterrors.NewErrorf(vtrpcpb.Code_NOT_FOUND, vterrors.SPDoesNotExist, "SAVEPOINT does not exist: %s", query)
		}, vcursor.IgnoreMaxMemoryRows())
		return qr, err
	case sqlparser.StmtKill:
		return e.handleKill(ctx, mysqlCtx, vcursor, stmt, logStats)
	}
	return nil, nil
}

func (e *Executor) startTxIfNecessary(ctx context.Context, safeSession *econtext.SafeSession) error {
	if !safeSession.Autocommit && !safeSession.InTransaction() {
		if err := e.txConn.Begin(ctx, safeSession, nil); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) insideTransaction(ctx context.Context, safeSession *econtext.SafeSession, logStats *logstats.LogStats, execPlan func() error) error {
	mustCommit := false
	if safeSession.Autocommit && !safeSession.InTransaction() {
		mustCommit = true
		if err := e.txConn.Begin(ctx, safeSession, nil); err != nil {
			return err
		}
		// The defer acts as a failsafe. If commit was successful,
		// the rollback will be a no-op.
		defer e.txConn.Rollback(ctx, safeSession) // nolint:errcheck
	}

	// The SetAutocommitable flag should be same as mustCommit.
	// If we started a transaction because of autocommit, then mustCommit
	// will be true, which means that we can autocommit. If we were already
	// in a transaction, it means that the app started it, or we are being
	// called recursively. If so, we cannot autocommit because whatever we
	// do is likely not final.
	// The control flow is such that autocommitable can only be turned on
	// at the beginning, but never after.
	safeSession.SetAutocommittable(mustCommit)

	// If we want to instantly commit the query, then there is no need to add savepoints.
	// Any partial failure of the query will be taken care by rollback.
	safeSession.SetSavepointState(!mustCommit)

	// Execute!
	err := execPlan()
	if err != nil {
		return err
	}

	if mustCommit {
		commitStart := time.Now()
		if err := e.txConn.Commit(ctx, safeSession); err != nil {
			return err
		}
		logStats.CommitTime = time.Since(commitStart)
	}
	return nil
}

func (e *Executor) executePlan(
	ctx context.Context,
	safeSession *econtext.SafeSession,
	plan *engine.Plan,
	vcursor *econtext.VCursorImpl,
	bindVars map[string]*querypb.BindVariable,
	logStats *logstats.LogStats,
	execStart time.Time,
) (*sqltypes.Result, error) {

	// 4: Execute!
	qr, err := vcursor.ExecutePrimitive(ctx, plan.Instructions, bindVars, true)

	// 5: Log and add statistics
	e.setLogStats(logStats, plan, vcursor, execStart, err, qr)

	// Check if there was partial DML execution. If so, rollback the effect of the partially executed query.
	if err != nil {
		return nil, e.rollbackExecIfNeeded(ctx, safeSession, bindVars, logStats, err)
	}
	return qr, nil
}

// rollbackExecIfNeeded rollbacks the partial execution if earlier it was detected that it needs partial query execution to be rolled back.
func (e *Executor) rollbackExecIfNeeded(ctx context.Context, safeSession *econtext.SafeSession, bindVars map[string]*querypb.BindVariable, logStats *logstats.LogStats, err error) error {
	if !safeSession.InTransaction() {
		return err
	}
	if e.rollbackOnFatalTxError(ctx, safeSession, err) {
		return err
	}

	if safeSession.IsRollbackSet() {
		rErr := e.rollbackPartialExec(ctx, safeSession, bindVars, logStats)
		return vterrors.Wrap(err, rErr.Error())
	}
	return err
}

func (e *Executor) rollbackOnFatalTxError(ctx context.Context, safeSession *econtext.SafeSession, err error) bool {
	if !vterrors.IsError(err, vterrors.VT15001(0).ID) {
		return false
	}
	// we already know one or more shards are going to fail rolling back, the error can be discarded
	_ = e.txConn.Rollback(ctx, safeSession)
	safeSession.SetErrorUntilRollback(true)
	return true
}

// rollbackPartialExec rollbacks to the savepoint or rollbacks transaction based on the value set on SafeSession.rollbackOnPartialExec.
// Once, it is used the variable is reset.
// If it fails to rollback to the previous savepoint then, the transaction is forced to be rolled back.
func (e *Executor) rollbackPartialExec(ctx context.Context, safeSession *econtext.SafeSession, bindVars map[string]*querypb.BindVariable, logStats *logstats.LogStats) error {
	var err error
	var errMsg strings.Builder

	// If the context got cancelled we still have to revert the partial DML execution.
	// We cannot use the parent context here anymore.
	if ctx.Err() != nil {
		errMsg.WriteString("context canceled: ")
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
	}

	// needs to rollback only once.
	rQuery := safeSession.GetRollbackOnPartialExec()
	if rQuery != econtext.TxRollback {
		safeSession.SavepointRollback()
		_, _, err = e.execute(ctx, nil, safeSession, rQuery, bindVars, false, logStats)
		// If no error, the revert is successful with the savepoint. Notify the reason as error to the client.
		if err == nil {
			errMsg.WriteString(vterrors.RevertedPartialExec)
			return vterrors.New(vtrpcpb.Code_ABORTED, errMsg.String())
		}
		// not able to rollback changes of the failed query, so have to abort the complete transaction.
	}

	// abort the transaction.
	_ = e.txConn.Rollback(ctx, safeSession)

	errMsg.WriteString(vterrors.TxRollbackOnPartialExec)
	if err != nil {
		return vterrors.Wrap(err, errMsg.String())
	}
	return vterrors.New(vtrpcpb.Code_ABORTED, errMsg.String())
}

func (e *Executor) setLogStats(logStats *logstats.LogStats, plan *engine.Plan, vcursor *econtext.VCursorImpl, execStart time.Time, err error, qr *sqltypes.Result) {
	logStats.StmtType = plan.QueryType.String()
	logStats.ActiveKeyspace = vcursor.GetKeyspace()
	logStats.TablesUsed = plan.TablesUsed
	logStats.TabletType = vcursor.TabletType().String()
	errCount := e.logExecutionEnd(logStats, execStart, plan, vcursor, err, qr)
	plan.AddStats(1, time.Since(logStats.StartTime), logStats.ShardQueries, logStats.RowsAffected, logStats.RowsReturned, errCount)
}

func (e *Executor) logExecutionEnd(logStats *logstats.LogStats, execStart time.Time, plan *engine.Plan, vcursor *econtext.VCursorImpl, err error, qr *sqltypes.Result) uint64 {
	logStats.ExecuteTime = time.Since(execStart)

	e.updateQueryStats(plan.QueryType.String(), plan.Type.String(), vcursor.TabletType().String(), int64(logStats.ShardQueries), plan.TablesUsed)

	var errCount uint64
	if err != nil {
		logStats.Error = err
		errCount = 1
	} else {
		logStats.RowsAffected = qr.RowsAffected
		logStats.RowsReturned = uint64(len(qr.Rows))
	}
	return errCount
}

func (e *Executor) logPlanningFinished(logStats *logstats.LogStats, plan *engine.Plan) time.Time {
	execStart := time.Now()
	if plan != nil {
		logStats.StmtType = plan.QueryType.String()
	}
	logStats.PlanTime = execStart.Sub(logStats.StartTime)
	return execStart
}

func shouldBlockQueries(plan *engine.Plan, safeSession *econtext.SafeSession) bool {
	block := safeSession.IsErrorUntilRollback()
	if plan.QueryType != sqlparser.StmtRollback && block {
		return true
	}
	if block {
		safeSession.SetErrorUntilRollback(false)
	}
	return false
}
