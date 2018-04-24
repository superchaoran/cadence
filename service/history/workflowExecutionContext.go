// Copyright (c) 2017 Uber Technologies, Inc.
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

package history

import (
	"fmt"
	"sync"
	"time"

	h "github.com/uber/cadence/.gen/go/history"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/backoff"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/persistence"

	"github.com/uber-common/bark"
)

const (
	secondsInDay = int32(24 * time.Hour / time.Second)
)

type (
	workflowExecutionContext struct {
		domainID          string
		workflowExecution workflow.WorkflowExecution
		shard             ShardContext
		executionManager  persistence.ExecutionManager
		logger            bark.Logger

		sync.Mutex
		msBuilder       *mutableStateBuilder
		updateCondition int64
		deleteTimerTask persistence.Task
	}
)

var (
	persistenceOperationRetryPolicy = common.CreatePersistanceRetryPolicy()
)

func newWorkflowExecutionContext(domainID string, execution workflow.WorkflowExecution, shard ShardContext,
	executionManager persistence.ExecutionManager, logger bark.Logger) *workflowExecutionContext {
	lg := logger.WithFields(bark.Fields{
		logging.TagWorkflowExecutionID: *execution.WorkflowId,
		logging.TagWorkflowRunID:       *execution.RunId,
	})

	return &workflowExecutionContext{
		domainID:          domainID,
		workflowExecution: execution,
		shard:             shard,
		executionManager:  executionManager,
		logger:            lg,
	}
}

func (c *workflowExecutionContext) loadWorkflowExecution() (*mutableStateBuilder, error) {
	if c.msBuilder != nil {
		return c.msBuilder, nil
	}

	response, err := c.getWorkflowExecutionWithRetry(&persistence.GetWorkflowExecutionRequest{
		DomainID:  c.domainID,
		Execution: c.workflowExecution,
	})
	if err != nil {
		if common.IsPersistenceTransientError(err) {
			logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationGetWorkflowExecution, err, "")
		}
		return nil, err
	}

	msBuilder := newMutableStateBuilder(c.shard.GetConfig(), c.logger)
	if response != nil && response.State != nil {
		state := response.State
		msBuilder.Load(state)
		info := state.ExecutionInfo
		c.updateCondition = info.NextEventID
	}

	c.msBuilder = msBuilder
	return msBuilder, nil
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithContext(context []byte, transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64) error {
	c.msBuilder.executionInfo.ExecutionContext = context

	return c.updateWorkflowExecution(transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithDeleteTask(transferTasks []persistence.Task,
	timerTasks []persistence.Task, deleteTimerTask persistence.Task, transactionID int64) error {
	c.deleteTimerTask = deleteTimerTask

	return c.updateWorkflowExecution(transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) replicateWorkflowExecution(request *h.ReplicateEventsRequest,
	lastEventID, transactionID int64) error {

	nextEventID := lastEventID + 1
	c.msBuilder.ApplyReplicationStateUpdates(request.GetVersion(), lastEventID)
	c.msBuilder.executionInfo.NextEventID = nextEventID

	builder := newHistoryBuilderFromEvents(request.History.Events, c.logger)
	return c.updateHelper(builder, nil, nil, false, true, transactionID)
}

func (c *workflowExecutionContext) updateWorkflowExecution(transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64) error {

	crossDCEnabled := c.shard.GetService().GetClusterMetadata().IsGlobalDomainEnabled()
	if crossDCEnabled {
		// Support for global domains is enabled and we are performing an update for global domain
		domainEntry, err := c.shard.GetDomainCache().GetDomainByID(c.msBuilder.executionInfo.DomainID)
		if err != nil {
			return err
		}

		lastEventID := c.msBuilder.GetNextEventID() - 1
		c.msBuilder.ApplyReplicationStateUpdates(domainEntry.GetFailoverVersion(), lastEventID)
	}

	return c.updateHelper(nil, transferTasks, timerTasks, crossDCEnabled, crossDCEnabled, transactionID)
}

func (c *workflowExecutionContext) updateHelper(builder *historyBuilder, transferTasks []persistence.Task,
	timerTasks []persistence.Task, createReplicationTask, updateReplicationState bool,
	transactionID int64) (errRet error) {

	defer func() {
		if errRet != nil {
			// Clear all cached state in case of error
			c.clear()
		}
	}()

	// Take a snapshot of all updates we have accumulated for this execution
	updates, err := c.msBuilder.CloseUpdateSession()
	if err != nil {
		return err
	}

	// Replicator passes in a custom builder as it already has the events
	if builder == nil {
		// If no builder is passed in then use the one as part of the updates
		builder = updates.newEventsBuilder
	}

	if builder.history != nil && len(builder.history) > 0 {
		// Some operations only update the mutable state. For example RecordActivityTaskHeartbeat.
		firstEvent := builder.history[0]
		serializedHistory, err := builder.Serialize()
		if err != nil {
			logging.LogHistorySerializationErrorEvent(c.logger, err, "Unable to serialize execution history for update.")
			return err
		}

		if err0 := c.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
			DomainID:      c.domainID,
			Execution:     c.workflowExecution,
			TransactionID: transactionID,
			FirstEventID:  *firstEvent.EventId,
			Events:        serializedHistory,
		}); err0 != nil {
			switch err0.(type) {
			case *persistence.ConditionFailedError:
				return ErrConflict
			}

			logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationUpdateWorkflowExecution, err0,
				fmt.Sprintf("{updateCondition: %v}", c.updateCondition))
			return err0
		}
		c.msBuilder.executionInfo.LastFirstEventID = *firstEvent.EventId
	}

	continueAsNew := updates.continueAsNew
	if continueAsNew != nil && updateReplicationState {
		currentVersion := c.msBuilder.replicationState.CurrentVersion
		continueAsNew.ReplicationState = &persistence.ReplicationState{
			CurrentVersion:   currentVersion,
			StartVersion:     currentVersion,
			LastWriteVersion: currentVersion,
			LastWriteEventID: firstEventID + 1,
		}
	}
	finishExecution := false
	var finishExecutionTTL int32
	if c.msBuilder.executionInfo.State == persistence.WorkflowStateCompleted {
		// Workflow execution completed as part of this transaction.
		// Also transactionally delete workflow execution representing
		// current run for the execution using cassandra TTL
		finishExecution = true
		domainEntry, err := c.shard.GetDomainCache().GetDomainByID(c.msBuilder.executionInfo.DomainID)
		if err != nil {
			return err
		}
		// NOTE: domain retention is in days, so we need to do a conversion
		finishExecutionTTL = domainEntry.GetConfig().Retention * secondsInDay
	}

	var replicationTasks []persistence.Task
	if createReplicationTask {
		// Let's create a replication task as part of this update
		replicationTasks = append(replicationTasks, c.msBuilder.createReplicationTask())
	}

	// this is the current failover version
	var version int64
	if c.msBuilder.replicationState != nil {
		version = c.msBuilder.replicationState.CurrentVersion
	}
	for _, task := range transferTasks {
		task.SetVersion(version)
	}
	for _, task := range timerTasks {
		task.SetVersion(version)
	}

	if err1 := c.updateWorkflowExecutionWithRetry(&persistence.UpdateWorkflowExecutionRequest{
		ExecutionInfo:             c.msBuilder.executionInfo,
		ReplicationState:          c.msBuilder.replicationState,
		TransferTasks:             transferTasks,
		ReplicationTasks:          replicationTasks,
		TimerTasks:                timerTasks,
		Condition:                 c.updateCondition,
		DeleteTimerTask:           c.deleteTimerTask,
		UpsertActivityInfos:       updates.updateActivityInfos,
		DeleteActivityInfos:       updates.deleteActivityInfos,
		UpserTimerInfos:           updates.updateTimerInfos,
		DeleteTimerInfos:          updates.deleteTimerInfos,
		UpsertChildExecutionInfos: updates.updateChildExecutionInfos,
		DeleteChildExecutionInfo:  updates.deleteChildExecutionInfo,
		UpsertRequestCancelInfos:  updates.updateCancelExecutionInfos,
		DeleteRequestCancelInfo:   updates.deleteCancelExecutionInfo,
		UpsertSignalInfos:         updates.updateSignalInfos,
		DeleteSignalInfo:          updates.deleteSignalInfo,
		UpsertSignalRequestedIDs:  updates.updateSignalRequestedIDs,
		DeleteSignalRequestedID:   updates.deleteSignalRequestedID,
		NewBufferedEvents:         updates.newBufferedEvents,
		ClearBufferedEvents:       updates.clearBufferedEvents,
		ContinueAsNew:             continueAsNew,
		FinishExecution:           finishExecution,
		FinishedExecutionTTL:      finishExecutionTTL,
	}); err1 != nil {
		switch err1.(type) {
		case *persistence.ConditionFailedError:
			return ErrConflict
		}

		logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationUpdateWorkflowExecution, err1,
			fmt.Sprintf("{updateCondition: %v}", c.updateCondition))
		return err1
	}

	// Update went through so update the condition for new updates
	c.updateCondition = c.msBuilder.GetNextEventID()
	c.msBuilder.executionInfo.LastUpdatedTimestamp = time.Now()

	// for any change in the workflow, send a event
	c.shard.NotifyNewHistoryEvent(newHistoryEventNotification(
		c.domainID,
		&c.workflowExecution,
		c.msBuilder.GetLastFirstEventID(),
		c.msBuilder.GetNextEventID(),
		c.msBuilder.isWorkflowExecutionRunning(),
	))

	return nil
}

func (c *workflowExecutionContext) replicateContinueAsNewWorkflowExecution(newStateBuilder *mutableStateBuilder,
	transferTasks []persistence.Task, timerTasks []persistence.Task, transactionID int64) error {
	return c.continueAsNewWorkflowExecutionHelper(nil, newStateBuilder, transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) continueAsNewWorkflowExecution(context []byte, newStateBuilder *mutableStateBuilder,
	transferTasks []persistence.Task, timerTasks []persistence.Task, transactionID int64) error {

	err1 := c.continueAsNewWorkflowExecutionHelper(context, newStateBuilder, transferTasks, timerTasks, transactionID)
	if err1 != nil {
		return err1
	}

	err2 := c.updateWorkflowExecutionWithContext(context, transferTasks, timerTasks, transactionID)

	if err2 != nil {
		// TODO: Delete new execution if update fails due to conflict or shard being lost
	}

	return err2
}

func (c *workflowExecutionContext) continueAsNewWorkflowExecutionHelper(context []byte, newStateBuilder *mutableStateBuilder,
	transferTasks []persistence.Task, timerTasks []persistence.Task, transactionID int64) error {

	domainID := newStateBuilder.executionInfo.DomainID
	newExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(newStateBuilder.executionInfo.WorkflowID),
		RunId:      common.StringPtr(newStateBuilder.executionInfo.RunID),
	}
	firstEvent := newStateBuilder.hBuilder.history[0]

	// Serialize the history
	serializedHistory, serializedError := newStateBuilder.hBuilder.Serialize()
	if serializedError != nil {
		logging.LogHistorySerializationErrorEvent(c.logger, serializedError, fmt.Sprintf(
			"HistoryEventBatch serialization error on start workflow.  WorkflowID: %v, RunID: %v", *newExecution.WorkflowId,
			*newExecution.RunId))
		return serializedError
	}

	return c.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
		DomainID:      domainID,
		Execution:     newExecution,
		TransactionID: transactionID,
		FirstEventID:  *firstEvent.EventId,
		Events:        serializedHistory,
	})
}

func (c *workflowExecutionContext) getWorkflowExecutionWithRetry(
	request *persistence.GetWorkflowExecutionRequest) (*persistence.GetWorkflowExecutionResponse, error) {
	var response *persistence.GetWorkflowExecutionResponse
	op := func() error {
		var err error
		response, err = c.executionManager.GetWorkflowExecution(request)

		return err
	}

	err := backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithRetry(
	request *persistence.UpdateWorkflowExecutionRequest) error {
	op := func() error {
		return c.shard.UpdateWorkflowExecution(request)
	}

	return backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
}

func (c *workflowExecutionContext) clear() {
	c.msBuilder = nil
}
