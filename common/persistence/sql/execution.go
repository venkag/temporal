// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package sql

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"go.temporal.io/api/serviceerror"

	enumsspb "go.temporal.io/server/api/enums/v1"
	"go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/log"
	p "go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/sql/sqlplugin"
	"go.temporal.io/server/common/primitives"
)

type sqlWorkflowStore struct {
	SqlStore
	shardID int32
}

var _ p.WorkflowStore = (*sqlWorkflowStore)(nil)

// NewSQLWorkflowStore creates an instance of WorkflowStore
func NewSQLWorkflowStore(
	db sqlplugin.DB,
	logger log.Logger,
	shardID int32,
) (p.WorkflowStore, error) {

	return &sqlWorkflowStore{
		shardID:  shardID,
		SqlStore: NewSqlStore(db, logger),
	}, nil
}

// txExecuteShardLocked executes f under transaction and with read lock on shard row
func (m *sqlWorkflowStore) txExecuteShardLocked(
	ctx context.Context,
	operation string,
	rangeID int64,
	fn func(tx sqlplugin.Tx) error,
) error {

	return m.txExecute(ctx, operation, func(tx sqlplugin.Tx) error {
		if err := readLockShard(ctx, tx, m.shardID, rangeID); err != nil {
			return err
		}
		err := fn(tx)
		if err != nil {
			return err
		}
		return nil
	})
}

func (m *sqlWorkflowStore) GetShardID() int32 {
	return m.shardID
}

func (m *sqlWorkflowStore) CreateWorkflowExecution(
	request *p.InternalCreateWorkflowExecutionRequest,
) (response *p.CreateWorkflowExecutionResponse, err error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	err = m.txExecuteShardLocked(ctx,
		"CreateWorkflowExecution",
		request.RangeID,
		func(tx sqlplugin.Tx) error {
			response, err = m.createWorkflowExecutionTx(ctx, tx, request)
			return err
		})
	return
}

func (m *sqlWorkflowStore) createWorkflowExecutionTx(
	ctx context.Context,
	tx sqlplugin.Tx,
	request *p.InternalCreateWorkflowExecutionRequest,
) (*p.CreateWorkflowExecutionResponse, error) {

	newWorkflow := request.NewWorkflowSnapshot
	lastWriteVersion := newWorkflow.LastWriteVersion
	shardID := m.shardID
	namespaceID := primitives.MustParseUUID(newWorkflow.NamespaceID)
	workflowID := newWorkflow.WorkflowID
	runID := primitives.MustParseUUID(newWorkflow.RunID)

	var err error
	var row *sqlplugin.CurrentExecutionsRow
	if row, err = lockCurrentExecutionIfExists(ctx,
		tx,
		m.shardID,
		namespaceID,
		workflowID,
	); err != nil {
		return nil, err
	}

	// current workflow record check
	if row != nil {
		// current run ID, last write version, current workflow state check
		switch request.Mode {
		case p.CreateWorkflowModeBrandNew:
			return nil, &p.WorkflowExecutionAlreadyStartedError{
				Msg:              fmt.Sprintf("Workflow execution already running. WorkflowId: %v", row.WorkflowID),
				StartRequestID:   row.CreateRequestID,
				RunID:            row.RunID.String(),
				State:            row.State,
				Status:           row.Status,
				LastWriteVersion: row.LastWriteVersion,
			}

		case p.CreateWorkflowModeWorkflowIDReuse:
			if request.PreviousLastWriteVersion != row.LastWriteVersion {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"LastWriteVersion: %v, PreviousLastWriteVersion: %v",
						workflowID, row.LastWriteVersion, request.PreviousLastWriteVersion),
					RequestID:        row.CreateRequestID,
					RunID:            row.RunID.String(),
					State:            row.State,
					LastWriteVersion: row.LastWriteVersion,
				}
			}
			if row.State != enumsspb.WORKFLOW_EXECUTION_STATE_COMPLETED {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"State: %v, Expected: %v",
						workflowID, row.State, enumsspb.WORKFLOW_EXECUTION_STATE_COMPLETED),
					RequestID:        row.CreateRequestID,
					RunID:            row.RunID.String(),
					State:            row.State,
					LastWriteVersion: row.LastWriteVersion,
				}
			}
			runIDStr := row.RunID.String()
			if runIDStr != request.PreviousRunID {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"RunId: %v, PreviousRunId: %v",
						workflowID, runIDStr, request.PreviousRunID),
					RequestID:        row.CreateRequestID,
					RunID:            row.RunID.String(),
					State:            row.State,
					LastWriteVersion: row.LastWriteVersion,
				}
			}

		case p.CreateWorkflowModeZombie:
			// zombie workflow creation with existence of current record, this is a noop
			if err := assertRunIDMismatch(primitives.MustParseUUID(newWorkflow.ExecutionState.RunId), row.RunID); err != nil {
				return nil, err
			}

		case p.CreateWorkflowModeContinueAsNew:
			runIDStr := row.RunID.String()
			if runIDStr != request.PreviousRunID {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"RunId: %v, PreviousRunId: %v",
						workflowID, runIDStr, request.PreviousRunID),
					RequestID:        row.CreateRequestID,
					RunID:            row.RunID.String(),
					State:            row.State,
					LastWriteVersion: row.LastWriteVersion,
				}
			}

		default:
			return nil, serviceerror.NewInternal(fmt.Sprintf("CreteWorkflowExecution: unknown mode: %v", request.Mode))
		}
	}

	if err := createOrUpdateCurrentExecution(ctx,
		tx,
		request.Mode,
		m.shardID,
		namespaceID,
		workflowID,
		runID,
		newWorkflow.ExecutionState.State,
		newWorkflow.ExecutionState.Status,
		newWorkflow.ExecutionState.CreateRequestId,
		newWorkflow.StartVersion,
		lastWriteVersion); err != nil {
		return nil, err
	}

	if err := m.applyWorkflowSnapshotTxAsNew(ctx,
		tx,
		shardID,
		&request.NewWorkflowSnapshot,
	); err != nil {
		return nil, err
	}

	return &p.CreateWorkflowExecutionResponse{}, nil
}

func (m *sqlWorkflowStore) GetWorkflowExecution(
	request *p.GetWorkflowExecutionRequest,
) (*p.InternalGetWorkflowExecutionResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	namespaceID := primitives.MustParseUUID(request.NamespaceID)
	runID := primitives.MustParseUUID(request.Execution.RunId)
	wfID := request.Execution.WorkflowId
	executionsRow, err := m.Db.SelectFromExecutions(ctx, sqlplugin.ExecutionsFilter{
		ShardID:     m.shardID,
		NamespaceID: namespaceID,
		WorkflowID:  wfID,
		RunID:       runID,
	})
	switch err {
	case nil:
		// noop
	case sql.ErrNoRows:
		return nil, serviceerror.NewNotFound(fmt.Sprintf("Workflow executionsRow not found.  WorkflowId: %v, RunId: %v", request.Execution.GetWorkflowId(), request.Execution.GetRunId()))
	default:
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed. Error: %v", err))
	}

	state := &p.InternalWorkflowMutableState{
		ExecutionInfo:  p.NewDataBlob(executionsRow.Data, executionsRow.DataEncoding),
		ExecutionState: p.NewDataBlob(executionsRow.State, executionsRow.StateEncoding),
		NextEventID:    executionsRow.NextEventID,

		DBRecordVersion: executionsRow.DBRecordVersion,
	}

	state.ActivityInfos, err = getActivityInfoMap(ctx,
		m.Db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get activity info. Error: %v", err))
	}

	state.TimerInfos, err = getTimerInfoMap(ctx,
		m.Db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get timer info. Error: %v", err))
	}

	state.ChildExecutionInfos, err = getChildExecutionInfoMap(ctx,
		m.Db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get child executionsRow info. Error: %v", err))
	}

	state.RequestCancelInfos, err = getRequestCancelInfoMap(ctx,
		m.Db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get request cancel info. Error: %v", err))
	}

	state.SignalInfos, err = getSignalInfoMap(ctx,
		m.Db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get signal info. Error: %v", err))
	}

	state.BufferedEvents, err = getBufferedEvents(ctx,
		m.Db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get buffered events. Error: %v", err))
	}

	state.SignalRequestedIDs, err = getSignalsRequested(ctx,
		m.Db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get signals requested. Error: %v", err))
	}

	return &p.InternalGetWorkflowExecutionResponse{
		State:           state,
		DBRecordVersion: executionsRow.DBRecordVersion,
	}, nil
}

func (m *sqlWorkflowStore) UpdateWorkflowExecution(
	request *p.InternalUpdateWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	return m.txExecuteShardLocked(ctx,
		"UpdateWorkflowExecution",
		request.RangeID,
		func(tx sqlplugin.Tx) error {
			return m.updateWorkflowExecutionTx(ctx, tx, request)
		})
}

func (m *sqlWorkflowStore) updateWorkflowExecutionTx(
	ctx context.Context,
	tx sqlplugin.Tx,
	request *p.InternalUpdateWorkflowExecutionRequest,
) error {

	updateWorkflow := request.UpdateWorkflowMutation
	newWorkflow := request.NewWorkflowSnapshot

	namespaceID := primitives.MustParseUUID(updateWorkflow.NamespaceID)
	workflowID := updateWorkflow.WorkflowID
	runID := primitives.MustParseUUID(updateWorkflow.ExecutionState.RunId)
	shardID := m.shardID

	switch request.Mode {
	case p.UpdateWorkflowModeBypassCurrent:
		if err := assertNotCurrentExecution(ctx,
			tx,
			shardID,
			namespaceID,
			workflowID,
			runID,
		); err != nil {
			return err
		}

	case p.UpdateWorkflowModeUpdateCurrent:
		if newWorkflow != nil {
			lastWriteVersion := newWorkflow.LastWriteVersion
			newNamespaceID := primitives.MustParseUUID(newWorkflow.NamespaceID)
			newRunID := primitives.MustParseUUID(newWorkflow.ExecutionState.RunId)

			if !bytes.Equal(namespaceID, newNamespaceID) {
				return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: cannot continue as new to another namespace"))
			}

			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				shardID,
				namespaceID,
				workflowID,
				newRunID,
				runID,
				newWorkflow.ExecutionState.CreateRequestId,
				newWorkflow.ExecutionState.State,
				newWorkflow.ExecutionState.Status,
				newWorkflow.StartVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: failed to continue as new current execution. Error: %v", err))
			}
		} else {
			lastWriteVersion := updateWorkflow.LastWriteVersion
			// this is only to update the current record
			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				shardID,
				namespaceID,
				workflowID,
				runID,
				runID,
				updateWorkflow.ExecutionState.CreateRequestId,
				updateWorkflow.ExecutionState.State,
				updateWorkflow.ExecutionState.Status,
				updateWorkflow.StartVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: failed to update current execution. Error: %v", err))
			}
		}

	default:
		return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: unknown mode: %v", request.Mode))
	}

	if err := applyWorkflowMutationTx(ctx,
		tx,
		shardID,
		&updateWorkflow,
	); err != nil {
		return err
	}

	if newWorkflow != nil {
		if err := m.applyWorkflowSnapshotTxAsNew(ctx,
			tx,
			shardID,
			newWorkflow,
		); err != nil {
			return err
		}
	}
	return nil
}

func (m *sqlWorkflowStore) ConflictResolveWorkflowExecution(
	request *p.InternalConflictResolveWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	return m.txExecuteShardLocked(ctx,
		"ConflictResolveWorkflowExecution",
		request.RangeID,
		func(tx sqlplugin.Tx) error {
			return m.conflictResolveWorkflowExecutionTx(ctx, tx, request)
		})
}

func (m *sqlWorkflowStore) conflictResolveWorkflowExecutionTx(
	ctx context.Context,
	tx sqlplugin.Tx,
	request *p.InternalConflictResolveWorkflowExecutionRequest,
) error {

	currentWorkflow := request.CurrentWorkflowMutation
	resetWorkflow := request.ResetWorkflowSnapshot
	newWorkflow := request.NewWorkflowSnapshot

	shardID := m.shardID

	namespaceID := primitives.MustParseUUID(resetWorkflow.NamespaceID)
	workflowID := resetWorkflow.WorkflowID

	switch request.Mode {
	case p.ConflictResolveWorkflowModeBypassCurrent:
		if err := assertNotCurrentExecution(ctx,
			tx,
			shardID,
			namespaceID,
			workflowID,
			primitives.MustParseUUID(resetWorkflow.ExecutionState.RunId),
		); err != nil {
			return err
		}

	case p.ConflictResolveWorkflowModeUpdateCurrent:
		executionState := resetWorkflow.ExecutionState
		lastWriteVersion := resetWorkflow.LastWriteVersion
		startVersion := resetWorkflow.StartVersion
		if newWorkflow != nil {
			executionState = newWorkflow.ExecutionState
			lastWriteVersion = newWorkflow.LastWriteVersion
			startVersion = newWorkflow.StartVersion
		}
		runID := primitives.MustParseUUID(executionState.RunId)
		createRequestID := executionState.CreateRequestId
		state := executionState.State
		status := executionState.Status

		if currentWorkflow != nil {
			prevRunID := primitives.MustParseUUID(currentWorkflow.ExecutionState.RunId)

			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				m.shardID,
				namespaceID,
				workflowID,
				runID,
				prevRunID,
				createRequestID,
				state,
				status,
				startVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("ConflictResolveWorkflowExecution. Failed to comare and swap the current record. Error: %v", err))
			}
		} else {
			// reset workflow is current
			prevRunID := primitives.MustParseUUID(resetWorkflow.ExecutionState.RunId)

			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				m.shardID,
				namespaceID,
				workflowID,
				runID,
				prevRunID,
				createRequestID,
				state,
				status,
				startVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("ConflictResolveWorkflowExecution. Failed to comare and swap the current record. Error: %v", err))
			}
		}

	default:
		return serviceerror.NewInternal(fmt.Sprintf("ConflictResolveWorkflowExecution: unknown mode: %v", request.Mode))
	}

	if err := applyWorkflowSnapshotTxAsReset(ctx,
		tx,
		shardID,
		&resetWorkflow,
	); err != nil {
		return err
	}

	if currentWorkflow != nil {
		if err := applyWorkflowMutationTx(ctx,
			tx,
			shardID,
			currentWorkflow,
		); err != nil {
			return err
		}
	}

	if newWorkflow != nil {
		if err := m.applyWorkflowSnapshotTxAsNew(ctx,
			tx,
			shardID,
			newWorkflow,
		); err != nil {
			return err
		}
	}
	return nil
}

func (m *sqlWorkflowStore) DeleteWorkflowExecution(
	request *p.DeleteWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	namespaceID := primitives.MustParseUUID(request.NamespaceID)
	runID := primitives.MustParseUUID(request.RunID)
	_, err := m.Db.DeleteFromExecutions(ctx, sqlplugin.ExecutionsFilter{
		ShardID:     m.shardID,
		NamespaceID: namespaceID,
		WorkflowID:  request.WorkflowID,
		RunID:       runID,
	})
	return err
}

// its possible for a new run of the same workflow to have started after the run we are deleting
// here was finished. In that case, current_executions table will have the same workflowID but different
// runID. The following code will delete the row from current_executions if and only if the runID is
// same as the one we are trying to delete here
func (m *sqlWorkflowStore) DeleteCurrentWorkflowExecution(
	request *p.DeleteCurrentWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	namespaceID := primitives.MustParseUUID(request.NamespaceID)
	runID := primitives.MustParseUUID(request.RunID)
	_, err := m.Db.DeleteFromCurrentExecutions(ctx, sqlplugin.CurrentExecutionsFilter{
		ShardID:     m.shardID,
		NamespaceID: namespaceID,
		WorkflowID:  request.WorkflowID,
		RunID:       runID,
	})
	return err
}

func (m *sqlWorkflowStore) GetCurrentExecution(
	request *p.GetCurrentExecutionRequest,
) (*p.InternalGetCurrentExecutionResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	row, err := m.Db.SelectFromCurrentExecutions(ctx, sqlplugin.CurrentExecutionsFilter{
		ShardID:     m.shardID,
		NamespaceID: primitives.MustParseUUID(request.NamespaceID),
		WorkflowID:  request.WorkflowID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, serviceerror.NewNotFound(err.Error())
		}
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetCurrentExecution operation failed. Error: %v", err))
	}

	return &p.InternalGetCurrentExecutionResponse{
		RunID: row.RunID.String(),
		ExecutionState: &persistence.WorkflowExecutionState{
			CreateRequestId: row.CreateRequestID,
			State:           row.State,
			Status:          row.Status,
		},
		LastWriteVersion: row.LastWriteVersion,
	}, nil
}

func (m *sqlWorkflowStore) ListConcreteExecutions(
	_ *p.ListConcreteExecutionsRequest,
) (*p.InternalListConcreteExecutionsResponse, error) {
	return nil, serviceerror.NewUnimplemented("ListConcreteExecutions is not implemented")
}
