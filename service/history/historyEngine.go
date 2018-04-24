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
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/yarpc"

	"github.com/pborman/uuid"
	"github.com/uber-common/bark"
	h "github.com/uber/cadence/.gen/go/history"
	workflow "github.com/uber/cadence/.gen/go/shared"
	hc "github.com/uber/cadence/client/history"
	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/messaging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
)

const (
	conditionalRetryCount                    = 5
	activityCancelationMsgActivityIDUnknown  = "ACTIVITY_ID_UNKNOWN"
	activityCancelationMsgActivityNotStarted = "ACTIVITY_ID_NOT_STARTED"
	timerCancelationMsgTimerIDUnknown        = "TIMER_ID_UNKNOWN"
)

type (
	historyEngineImpl struct {
		currentClusterName   string
		shard                ShardContext
		historyMgr           persistence.HistoryManager
		executionManager     persistence.ExecutionManager
		txProcessor          transferQueueProcessor
		timerProcessor       timerQueueProcessor
		replicator           *historyReplicator
		replicatorProcessor  queueProcessor
		historyEventNotifier historyEventNotifier
		tokenSerializer      common.TaskTokenSerializer
		hSerializerFactory   persistence.HistorySerializerFactory
		historyCache         *historyCache
		metricsClient        metrics.Client
		logger               bark.Logger
	}

	// shardContextWrapper wraps ShardContext to notify transferQueueProcessor on new tasks.
	// TODO: use to notify timerQueueProcessor as well.
	shardContextWrapper struct {
		currentClusterName string
		ShardContext
		txProcessor          transferQueueProcessor
		replcatorProcessor   queueProcessor
		historyEventNotifier historyEventNotifier
	}
)

var _ Engine = (*historyEngineImpl)(nil)

var (
	// ErrTaskRetry is the error indicating that the timer / transfer task should be retried.
	ErrTaskRetry = errors.New("passive task should retry due to condition in mutable state is not met")
	// ErrDuplicate is exported temporarily for integration test
	ErrDuplicate = errors.New("Duplicate task, completing it")
	// ErrConflict is exported temporarily for integration test
	ErrConflict = errors.New("Conditional update failed")
	// ErrMaxAttemptsExceeded is exported temporarily for integration test
	ErrMaxAttemptsExceeded = errors.New("Maximum attempts exceeded to update history")
	// ErrStaleState is the error returned during state update indicating that cached mutable state could be stale
	ErrStaleState = errors.New("Cache mutable state could potentially be stale")
	// ErrActivityTaskNotFound is the error to indicate activity task could be duplicate and activity already completed
	ErrActivityTaskNotFound = &workflow.EntityNotExistsError{Message: "Activity task not found."}
	// ErrWorkflowCompleted is the error to indicate workflow execution already completed
	ErrWorkflowCompleted = &workflow.EntityNotExistsError{Message: "Workflow execution already completed."}
	// ErrWorkflowParent is the error to parent execution is given and mismatch
	ErrWorkflowParent = &workflow.EntityNotExistsError{Message: "Workflow parent does not match."}
	// ErrDeserializingToken is the error to indicate task token is invalid
	ErrDeserializingToken = &workflow.BadRequestError{Message: "Error deserializing task token."}
	// ErrCancellationAlreadyRequested is the error indicating cancellation for target workflow is already requested
	ErrCancellationAlreadyRequested = &workflow.CancellationAlreadyRequestedError{Message: "Cancellation already requested for this workflow execution."}
	// FailedWorkflowCloseState is a set of failed workflow close states, used for start workflow policy
	// for start workflow execution API
	FailedWorkflowCloseState = map[int]bool{
		persistence.WorkflowCloseStatusFailed:     true,
		persistence.WorkflowCloseStatusCanceled:   true,
		persistence.WorkflowCloseStatusTerminated: true,
		persistence.WorkflowCloseStatusTimedOut:   true,
	}
)

// NewEngineWithShardContext creates an instance of history engine
func NewEngineWithShardContext(shard ShardContext, visibilityMgr persistence.VisibilityManager,
	matching matching.Client, historyClient hc.Client, historyEventNotifier historyEventNotifier, publisher messaging.Producer) Engine {
	currentClusterName := shard.GetService().GetClusterMetadata().GetCurrentClusterName()
	shardWrapper := &shardContextWrapper{
		currentClusterName:   currentClusterName,
		ShardContext:         shard,
		historyEventNotifier: historyEventNotifier,
	}
	shard = shardWrapper
	logger := shard.GetLogger()
	executionManager := shard.GetExecutionManager()
	historyManager := shard.GetHistoryManager()
	historyCache := newHistoryCache(shard, logger)
	historySerializerFactory := persistence.NewHistorySerializerFactory()
	historyEngImpl := &historyEngineImpl{
		currentClusterName: currentClusterName,
		shard:              shard,
		historyMgr:         historyManager,
		executionManager:   executionManager,
		tokenSerializer:    common.NewJSONTaskTokenSerializer(),
		hSerializerFactory: historySerializerFactory,
		historyCache:       historyCache,
		logger: logger.WithFields(bark.Fields{
			logging.TagWorkflowComponent: logging.TagValueHistoryEngineComponent,
		}),
		metricsClient:        shard.GetMetricsClient(),
		historyEventNotifier: historyEventNotifier,
	}
	txProcessor := newTransferQueueProcessor(shard, historyEngImpl, visibilityMgr, matching, historyClient, logger)
	historyEngImpl.timerProcessor = newTimerQueueProcessor(shard, historyEngImpl, logger)
	historyEngImpl.txProcessor = txProcessor
	shardWrapper.txProcessor = txProcessor

	// Only start the replicator processor if valid publisher is passed in
	if publisher != nil {
		replicatorProcessor := newReplicatorQueueProcessor(shard, publisher, executionManager, historyManager,
			historySerializerFactory, logger)
		historyEngImpl.replicatorProcessor = replicatorProcessor
		shardWrapper.replcatorProcessor = replicatorProcessor
		historyEngImpl.replicator = newHistoryReplicator(shard, historyCache, shard.GetDomainCache(), historyManager,
			logger)
	}

	return historyEngImpl
}

// Start will spin up all the components needed to start serving this shard.
// Make sure all the components are loaded lazily so start can return immediately.  This is important because
// ShardController calls start sequentially for all the shards for a given host during startup.
func (e *historyEngineImpl) Start() {
	logging.LogHistoryEngineStartingEvent(e.logger)
	defer logging.LogHistoryEngineStartedEvent(e.logger)

	e.txProcessor.Start()
	e.timerProcessor.Start()
	if e.replicatorProcessor != nil {
		e.replicatorProcessor.Start()
	}
}

// Stop the service.
func (e *historyEngineImpl) Stop() {
	logging.LogHistoryEngineShuttingDownEvent(e.logger)
	defer logging.LogHistoryEngineShutdownEvent(e.logger)

	e.txProcessor.Stop()
	e.timerProcessor.Stop()
	if e.replicatorProcessor != nil {
		e.replicatorProcessor.Stop()
	}
}

// StartWorkflowExecution starts a workflow execution
func (e *historyEngineImpl) StartWorkflowExecution(startRequest *h.StartWorkflowExecutionRequest) (
	*workflow.StartWorkflowExecutionResponse, error) {
	domainID, err := getDomainUUID(startRequest.DomainUUID)
	if err != nil {
		return nil, err
	}

	request := startRequest.StartRequest
	err = validateStartWorkflowExecutionRequest(request)
	if err != nil {
		return nil, err
	}

	execution := workflow.WorkflowExecution{
		WorkflowId: request.WorkflowId,
		RunId:      common.StringPtr(uuid.New()),
	}

	var parentExecution *workflow.WorkflowExecution
	initiatedID := emptyEventID
	parentDomainID := ""
	parentInfo := startRequest.ParentExecutionInfo
	if parentInfo != nil {
		parentDomainID = *parentInfo.DomainUUID
		parentExecution = parentInfo.Execution
		initiatedID = *parentInfo.InitiatedId
	}

	// Generate first decision task event.
	taskList := request.TaskList.GetName()
	msBuilder := newMutableStateBuilder(e.shard.GetConfig(), e.logger)
	startedEvent := msBuilder.AddWorkflowExecutionStartedEvent(execution, startRequest)
	if startedEvent == nil {
		return nil, &workflow.InternalServiceError{Message: "Failed to add workflow execution started event."}
	}

	var transferTasks []persistence.Task
	decisionScheduleID := emptyEventID
	decisionStartID := emptyEventID
	decisionTimeout := int32(0)
	if parentInfo == nil {
		// DecisionTask is only created when it is not a Child Workflow Execution
		di := msBuilder.AddDecisionTaskScheduledEvent()
		if di == nil {
			return nil, &workflow.InternalServiceError{Message: "Failed to add decision scheduled event."}
		}

		transferTasks = []persistence.Task{&persistence.DecisionTask{
			DomainID: domainID, TaskList: taskList, ScheduleID: di.ScheduleID,
		}}
		decisionScheduleID = di.ScheduleID
		decisionStartID = di.StartedID
		decisionTimeout = di.DecisionTimeout
	}

	duration := time.Duration(*request.ExecutionStartToCloseTimeoutSeconds) * time.Second
	timerTasks := []persistence.Task{&persistence.WorkflowTimeoutTask{
		VisibilityTimestamp: e.shard.GetTimeSource().Now().Add(duration),
	}}
	// Serialize the history
	serializedHistory, serializedError := msBuilder.hBuilder.Serialize()
	if serializedError != nil {
		logging.LogHistorySerializationErrorEvent(e.logger, serializedError, fmt.Sprintf(
			"HistoryEventBatch serialization error on start workflow.  WorkflowID: %v, RunID: %v",
			execution.GetWorkflowId(), execution.GetRunId()))
		return nil, serializedError
	}

	err = e.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
		DomainID:  domainID,
		Execution: execution,
		// It is ok to use 0 for TransactionID because RunID is unique so there are
		// no potential duplicates to override.
		TransactionID: 0,
		FirstEventID:  startedEvent.GetEventId(),
		Events:        serializedHistory,
	})
	if err != nil {
		return nil, err
	}
	msBuilder.executionInfo.LastFirstEventID = startedEvent.GetEventId()

	createReplicationTask := e.shard.GetService().GetClusterMetadata().IsGlobalDomainEnabled()
	var replicationState *persistence.ReplicationState
	var replicationTasks []persistence.Task
	if createReplicationTask {
		domainEntry, err := e.shard.GetDomainCache().GetDomainByID(domainID)
		if err != nil {
			return nil, err
		}

		failoverVersion := domainEntry.GetFailoverVersion()
		replicationState = &persistence.ReplicationState{
			CurrentVersion:   failoverVersion,
			StartVersion:     failoverVersion,
			LastWriteVersion: failoverVersion,
			LastWriteEventID: decisionScheduleID,
		}

		replicationTask := &persistence.HistoryReplicationTask{
			FirstEventID:        firstEventID,
			NextEventID:         msBuilder.GetNextEventID(),
			Version:             failoverVersion,
			LastReplicationInfo: nil,
		}
		replicationTasks = append(replicationTasks, replicationTask)
	}

	createWorkflow := func(isBrandNew bool, prevRunID string) (string, error) {
		_, err = e.shard.CreateWorkflowExecution(&persistence.CreateWorkflowExecutionRequest{
			RequestID:                   common.StringDefault(request.RequestId),
			DomainID:                    domainID,
			Execution:                   execution,
			ParentDomainID:              parentDomainID,
			ParentExecution:             parentExecution,
			InitiatedID:                 initiatedID,
			TaskList:                    *request.TaskList.Name,
			WorkflowTypeName:            *request.WorkflowType.Name,
			WorkflowTimeout:             *request.ExecutionStartToCloseTimeoutSeconds,
			DecisionTimeoutValue:        *request.TaskStartToCloseTimeoutSeconds,
			ExecutionContext:            nil,
			NextEventID:                 msBuilder.GetNextEventID(),
			LastProcessedEvent:          emptyEventID,
			TransferTasks:               transferTasks,
			ReplicationTasks:            replicationTasks,
			DecisionScheduleID:          decisionScheduleID, // TODO add decision version, when doing crodd DC
			DecisionStartedID:           decisionStartID,
			DecisionStartToCloseTimeout: decisionTimeout,
			TimerTasks:                  timerTasks,
			ContinueAsNew:               !isBrandNew,
			PreviousRunID:               prevRunID,
			ReplicationState:            replicationState,
		})

		if err != nil {
			switch t := err.(type) {
			case *persistence.WorkflowExecutionAlreadyStartedError:
				if t.StartRequestID == common.StringDefault(request.RequestId) {
					e.deleteEvents(domainID, execution)
					return t.RunID, nil
				}
			case *persistence.ShardOwnershipLostError:
				e.deleteEvents(domainID, execution)
			}
			return "", err
		}
		return execution.GetRunId(), nil
	}

	workflowExistsErrHandler := func(err *persistence.WorkflowExecutionAlreadyStartedError) error {
		// set the prev run ID for database conditional update
		prevStartRequestID := err.StartRequestID
		prevRunID := err.RunID
		prevState := err.State
		prevCloseState := err.CloseStatus

		errFn := func(errMsg string, createRequestID string, workflowID string, runID string) error {
			msg := fmt.Sprintf(errMsg, workflowID, runID)
			return &workflow.WorkflowExecutionAlreadyStartedError{
				Message:        common.StringPtr(msg),
				StartRequestId: common.StringPtr(fmt.Sprintf("%v", createRequestID)),
				RunId:          common.StringPtr(fmt.Sprintf("%v", runID)),
			}
		}

		// here we know there is some information about the prev workflow, i.e. either running right now
		// or has history check if the this workflow is finished
		if prevState != persistence.WorkflowStateCompleted {
			e.deleteEvents(domainID, execution)
			msg := "Workflow execution is already running. WorkflowId: %v, RunId: %v."
			return errFn(msg, prevStartRequestID, execution.GetWorkflowId(), prevRunID)
		}
		switch startRequest.StartRequest.GetWorkflowIdReusePolicy() {
		case workflow.WorkflowIdReusePolicyAllowDuplicateFailedOnly:
			if _, ok := FailedWorkflowCloseState[prevCloseState]; !ok {
				e.deleteEvents(domainID, execution)
				msg := "Workflow execution already finished successfully. WorkflowId: %v, RunId: %v. Workflow ID reuse policy: allow duplicate workflow ID if last run failed."
				return errFn(msg, prevStartRequestID, execution.GetWorkflowId(), prevRunID)
			}
		case workflow.WorkflowIdReusePolicyAllowDuplicate:
			// as long as workflow not running, so this case has no check
		case workflow.WorkflowIdReusePolicyRejectDuplicate:
			e.deleteEvents(domainID, execution)
			msg := "Workflow execution already finished. WorkflowId: %v, RunId: %v. Workflow ID reuse policy: reject duplicate workflow ID."
			return errFn(msg, prevStartRequestID, execution.GetWorkflowId(), prevRunID)
		default:
			e.deleteEvents(domainID, execution)
			return &workflow.InternalServiceError{Message: "Failed to process start workflow reuse policy."}
		}

		return nil
	}

	// try to create the workflow execution
	isBrandNew := true
	resultRunID := ""
	resultRunID, err = createWorkflow(isBrandNew, "")
	// if err still non nil, see if retry
	if errExist, ok := err.(*persistence.WorkflowExecutionAlreadyStartedError); ok {
		if err = workflowExistsErrHandler(errExist); err == nil {
			isBrandNew = false
			resultRunID, err = createWorkflow(isBrandNew, errExist.RunID)
		}
	}

	if err == nil {
		e.timerProcessor.NotifyNewTimers(e.currentClusterName, timerTasks)

		return &workflow.StartWorkflowExecutionResponse{
			RunId: common.StringPtr(resultRunID),
		}, nil
	}
	return nil, err
}

// GetMutableState retrieves the mutable state of the workflow execution
func (e *historyEngineImpl) GetMutableState(ctx context.Context,
	request *h.GetMutableStateRequest) (*h.GetMutableStateResponse, error) {

	domainID, err := getDomainUUID(request.DomainUUID)
	if err != nil {
		return nil, err
	}

	execution := workflow.WorkflowExecution{
		WorkflowId: request.Execution.WorkflowId,
		RunId:      request.Execution.RunId,
	}

	response, err := e.getMutableState(domainID, execution)
	if err != nil {
		return nil, err
	}
	// set the run id in case query the current running workflow
	execution.RunId = response.Execution.RunId

	// expectedNextEventID is 0 when caller want to get the current next event ID without blocking
	expectedNextEventID := common.FirstEventID
	if request.ExpectedNextEventId != nil {
		expectedNextEventID = request.GetExpectedNextEventId()
	}

	// if caller decide to long poll on workflow execution
	// and the event ID we are looking for is smaller than current next event ID
	if expectedNextEventID >= response.GetNextEventId() && response.GetIsWorkflowRunning() {
		subscriberID, channel, err := e.historyEventNotifier.WatchHistoryEvent(newWorkflowIdentifier(domainID, &execution))
		if err != nil {
			return nil, err
		}
		defer e.historyEventNotifier.UnwatchHistoryEvent(newWorkflowIdentifier(domainID, &execution), subscriberID)

		// check again in case the next event ID is updated
		response, err = e.getMutableState(domainID, execution)
		if err != nil {
			return nil, err
		}

		if expectedNextEventID < response.GetNextEventId() || !response.GetIsWorkflowRunning() {
			return response, nil
		}

		timer := time.NewTimer(e.shard.GetConfig().LongPollExpirationInterval())
		defer timer.Stop()
		for {
			select {
			case event := <-channel:
				response.LastFirstEventId = common.Int64Ptr(event.lastFirstEventID)
				response.NextEventId = common.Int64Ptr(event.nextEventID)
				response.IsWorkflowRunning = common.BoolPtr(event.isWorkflowRunning)
				if expectedNextEventID < response.GetNextEventId() || !response.GetIsWorkflowRunning() {
					return response, nil
				}
			case <-timer.C:
				return response, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	return response, nil
}

func (e *historyEngineImpl) getMutableState(
	domainID string, execution workflow.WorkflowExecution) (retResp *h.GetMutableStateResponse, retError error) {

	context, release, err0 := e.historyCache.getOrCreateWorkflowExecution(domainID, execution)
	if err0 != nil {
		return nil, err0
	}
	defer func() { release(retError) }()

	msBuilder, err1 := context.loadWorkflowExecution()
	if err1 != nil {
		return nil, err1
	}

	execution.RunId = context.workflowExecution.RunId
	result := &h.GetMutableStateResponse{
		Execution:                            &execution,
		WorkflowType:                         &workflow.WorkflowType{Name: common.StringPtr(msBuilder.executionInfo.WorkflowTypeName)},
		LastFirstEventId:                     common.Int64Ptr(msBuilder.GetLastFirstEventID()),
		NextEventId:                          common.Int64Ptr(msBuilder.GetNextEventID()),
		TaskList:                             &workflow.TaskList{Name: common.StringPtr(context.msBuilder.executionInfo.TaskList)},
		StickyTaskList:                       &workflow.TaskList{Name: common.StringPtr(msBuilder.executionInfo.StickyTaskList)},
		ClientLibraryVersion:                 common.StringPtr(msBuilder.executionInfo.ClientLibraryVersion),
		ClientFeatureVersion:                 common.StringPtr(msBuilder.executionInfo.ClientFeatureVersion),
		ClientImpl:                           common.StringPtr(msBuilder.executionInfo.ClientImpl),
		IsWorkflowRunning:                    common.BoolPtr(msBuilder.isWorkflowExecutionRunning()),
		StickyTaskListScheduleToStartTimeout: common.Int32Ptr(msBuilder.executionInfo.StickyScheduleToStartTimeout),
	}

	return result, nil
}

// ResetStickyTaskList reset the volatile information in mutable state of a given workflow.
// Volatile information are the information related to client, such as:
// 1. StickyTaskList
// 2. StickyScheduleToStartTimeout
// 3. ClientLibraryVersion
// 4. ClientFeatureVersion
// 5. ClientImpl
func (e *historyEngineImpl) ResetStickyTaskList(resetRequest *h.ResetStickyTaskListRequest) (*h.ResetStickyTaskListResponse, error) {
	domainID, err := getDomainUUID(resetRequest.DomainUUID)
	if err != nil {
		return nil, err
	}

	err = e.updateWorkflowExecution(domainID, *resetRequest.Execution, false, false,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, nil
			}
			msBuilder.clearStickyness()
			return nil, nil
		},
	)

	if err != nil {
		return nil, err
	}
	return &h.ResetStickyTaskListResponse{}, nil
}

// DescribeWorkflowExecution returns information about the specified workflow execution.
func (e *historyEngineImpl) DescribeWorkflowExecution(
	request *h.DescribeWorkflowExecutionRequest) (retResp *workflow.DescribeWorkflowExecutionResponse, retError error) {
	domainID, err := getDomainUUID(request.DomainUUID)
	if err != nil {
		return nil, err
	}

	execution := *request.Request.Execution

	context, release, err0 := e.historyCache.getOrCreateWorkflowExecution(domainID, execution)
	if err0 != nil {
		return nil, err0
	}
	defer func() { release(retError) }()

	msBuilder, err1 := context.loadWorkflowExecution()
	if err1 != nil {
		return nil, err1
	}

	result := &workflow.DescribeWorkflowExecutionResponse{
		ExecutionConfiguration: &workflow.WorkflowExecutionConfiguration{
			TaskList: &workflow.TaskList{Name: common.StringPtr(msBuilder.executionInfo.TaskList)},
			ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(msBuilder.executionInfo.WorkflowTimeout),
			TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(msBuilder.executionInfo.DecisionTimeoutValue),
			ChildPolicy:                         common.ChildPolicyPtr(workflow.ChildPolicyTerminate),
		},
		WorkflowExecutionInfo: &workflow.WorkflowExecutionInfo{
			Execution:     request.Request.Execution,
			Type:          &workflow.WorkflowType{Name: common.StringPtr(msBuilder.executionInfo.WorkflowTypeName)},
			StartTime:     common.Int64Ptr(msBuilder.executionInfo.StartTimestamp.UnixNano()),
			HistoryLength: common.Int64Ptr(msBuilder.GetNextEventID() - common.FirstEventID),
		},
	}
	if msBuilder.executionInfo.State == persistence.WorkflowStateCompleted {
		// for closed workflow
		closeStatus := getWorkflowExecutionCloseStatus(msBuilder.executionInfo.CloseStatus)
		result.WorkflowExecutionInfo.CloseStatus = &closeStatus
		result.WorkflowExecutionInfo.CloseTime = common.Int64Ptr(msBuilder.getLastUpdatedTimestamp())
	}

	if len(msBuilder.pendingActivityInfoIDs) > 0 {
		for _, pi := range msBuilder.pendingActivityInfoIDs {
			ai := &workflow.PendingActivityInfo{
				ActivityID: common.StringPtr(pi.ActivityID),
			}
			state := workflow.PendingActivityStateScheduled
			if pi.CancelRequested {
				state = workflow.PendingActivityStateCancelRequested
			} else if pi.StartedID != emptyEventID {
				state = workflow.PendingActivityStateStarted
			}
			ai.State = &state
			lastHeartbeatUnixNano := pi.LastHeartBeatUpdatedTime.UnixNano()
			if lastHeartbeatUnixNano > 0 {
				ai.LastHeartbeatTimestamp = common.Int64Ptr(lastHeartbeatUnixNano)
				ai.HeartbeatDetails = pi.Details
			}
			if scheduledEvent, ok := msBuilder.getHistoryEvent(pi.ScheduledEvent); ok {
				ai.ActivityType = scheduledEvent.ActivityTaskScheduledEventAttributes.ActivityType
			}
			result.PendingActivities = append(result.PendingActivities, ai)
		}
	}

	return result, nil
}

func (e *historyEngineImpl) RecordDecisionTaskStarted(
	request *h.RecordDecisionTaskStartedRequest) (retResp *h.RecordDecisionTaskStartedResponse, retError error) {
	domainID, err := getDomainUUID(request.DomainUUID)
	if err != nil {
		return nil, err
	}

	context, release, err0 := e.historyCache.getOrCreateWorkflowExecution(domainID, *request.WorkflowExecution)
	if err0 != nil {
		return nil, err0
	}
	defer func() { release(retError) }()

	scheduleID := request.GetScheduleId()
	requestID := request.GetRequestId()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err0 := context.loadWorkflowExecution()
		if err0 != nil {
			return nil, err0
		}
		tBuilder := e.getTimerBuilder(&context.workflowExecution)

		di, isRunning := msBuilder.GetPendingDecision(scheduleID)

		// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
		// some extreme cassandra failure cases.
		if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
			e.metricsClient.IncCounter(metrics.HistoryRecordDecisionTaskStartedScope, metrics.StaleMutableStateCounter)
			// Reload workflow execution history
			context.clear()
			continue Update_History_Loop
		}

		// Check execution state to make sure task is in the list of outstanding tasks and it is not yet started.  If
		// task is not outstanding than it is most probably a duplicate and complete the task.
		if !msBuilder.isWorkflowExecutionRunning() || !isRunning {
			// Looks like DecisionTask already completed as a result of another call.
			// It is OK to drop the task at this point.
			logging.LogDuplicateTaskEvent(context.logger, persistence.TransferTaskTypeDecisionTask, common.Int64Default(request.TaskId), requestID,
				scheduleID, emptyEventID, isRunning)

			return nil, &workflow.EntityNotExistsError{Message: "Decision task not found."}
		}

		if di.StartedID != emptyEventID {
			// If decision is started as part of the current request scope then return a positive response
			if di.RequestID == requestID {
				return e.createRecordDecisionTaskStartedResponse(domainID, msBuilder, di, request.PollRequest.GetIdentity()), nil
			}

			// Looks like DecisionTask already started as a result of another call.
			// It is OK to drop the task at this point.
			logging.LogDuplicateTaskEvent(context.logger, persistence.TaskListTypeDecision, common.Int64Default(request.TaskId), requestID,
				scheduleID, di.StartedID, isRunning)

			return nil, &h.EventAlreadyStartedError{Message: "Decision task already started."}
		}

		_, di = msBuilder.AddDecisionTaskStartedEvent(scheduleID, requestID, request.PollRequest)
		if di == nil {
			// Unable to add DecisionTaskStarted event to history
			return nil, &workflow.InternalServiceError{Message: "Unable to add DecisionTaskStarted event to history."}
		}

		// Start a timer for the decision task.
		timeOutTask := tBuilder.AddDecisionTimoutTask(scheduleID, di.Attempt, di.DecisionTimeout)
		timerTasks := []persistence.Task{timeOutTask}
		defer e.timerProcessor.NotifyNewTimers(e.currentClusterName, timerTasks)

		// Generate a transaction ID for appending events to history
		transactionID, err2 := e.shard.GetNextTransferTaskID()
		if err2 != nil {
			return nil, err2
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
		// the history and try the operation again.
		if err3 := context.updateWorkflowExecution(nil, timerTasks, transactionID); err3 != nil {
			if err3 == ErrConflict {
				e.metricsClient.IncCounter(metrics.HistoryRecordDecisionTaskStartedScope,
					metrics.ConcurrencyUpdateFailureCounter)
				continue Update_History_Loop
			}
			return nil, err3
		}

		return e.createRecordDecisionTaskStartedResponse(domainID, msBuilder, di, request.PollRequest.GetIdentity()), nil
	}

	return nil, ErrMaxAttemptsExceeded
}

func (e *historyEngineImpl) RecordActivityTaskStarted(
	request *h.RecordActivityTaskStartedRequest) (*h.RecordActivityTaskStartedResponse, error) {
	domainID, err := getDomainUUID(request.DomainUUID)
	if err != nil {
		return nil, err
	}

	execution := workflow.WorkflowExecution{
		WorkflowId: request.WorkflowExecution.WorkflowId,
		RunId:      request.WorkflowExecution.RunId,
	}

	response := &h.RecordActivityTaskStartedResponse{}
	err = e.updateWorkflowExecution(domainID, execution, false, false,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			scheduleID := request.GetScheduleId()
			requestID := request.GetRequestId()
			ai, isRunning := msBuilder.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRecordActivityTaskStartedScope, metrics.StaleMutableStateCounter)
				return nil, ErrStaleState
			}

			// Check execution state to make sure task is in the list of outstanding tasks and it is not yet started.  If
			// task is not outstanding than it is most probably a duplicate and complete the task.
			if !isRunning {
				// Looks like ActivityTask already completed as a result of another call.
				// It is OK to drop the task at this point.
				logging.LogDuplicateTaskEvent(e.logger, persistence.TransferTaskTypeActivityTask,
					common.Int64Default(request.TaskId), requestID, scheduleID, emptyEventID, isRunning)

				return nil, ErrActivityTaskNotFound
			}

			scheduledEvent, exists := msBuilder.GetActivityScheduledEvent(scheduleID)
			if !exists {
				return nil, &workflow.InternalServiceError{Message: "Corrupted workflow execution state."}
			}
			response.ScheduledEvent = scheduledEvent

			if ai.StartedID != emptyEventID {
				// If activity is started as part of the current request scope then return a positive response
				if ai.RequestID == requestID {
					startedEvent, exists := msBuilder.GetActivityStartedEvent(scheduleID)
					if !exists {
						return nil, &workflow.InternalServiceError{Message: "Corrupted workflow execution state."}
					}
					response.StartedEvent = startedEvent
					return nil, nil
				}

				// Looks like ActivityTask already started as a result of another call.
				// It is OK to drop the task at this point.
				logging.LogDuplicateTaskEvent(e.logger, persistence.TransferTaskTypeActivityTask,
					common.Int64Default(request.TaskId), requestID, scheduleID, ai.StartedID, isRunning)

				return nil, &h.EventAlreadyStartedError{Message: "Activity task already started."}
			}

			startedEvent := msBuilder.AddActivityTaskStartedEvent(ai, scheduleID, requestID, request.PollRequest)
			if startedEvent == nil {
				// Unable to add ActivityTaskStarted event to history
				return nil, &workflow.InternalServiceError{Message: "Unable to add ActivityTaskStarted event to history."}
			}
			response.StartedEvent = startedEvent

			// Start a timer for the activity task.
			timerTasks := []persistence.Task{}
			if tt := tBuilder.GetActivityTimerTaskIfNeeded(msBuilder); tt != nil {
				timerTasks = append(timerTasks, tt)
			}

			return timerTasks, nil
		})

	if err != nil {
		return nil, err
	}

	return response, err
}

// RespondDecisionTaskCompleted completes a decision task
func (e *historyEngineImpl) RespondDecisionTaskCompleted(ctx context.Context, req *h.RespondDecisionTaskCompletedRequest) (retError error) {
	domainID, err := getDomainUUID(req.DomainUUID)
	if err != nil {
		return err
	}
	request := req.CompleteRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return ErrDeserializingToken
	}

	workflowExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(token.WorkflowID),
		RunId:      common.StringPtr(token.RunID),
	}

	call := yarpc.CallFromContext(ctx)
	clientLibVersion := call.Header(common.LibraryVersionHeaderName)
	clientFeatureVersion := call.Header(common.FeatureVersionHeaderName)
	clientImpl := call.Header(common.ClientImplHeaderName)

	context, release, err0 := e.historyCache.getOrCreateWorkflowExecution(domainID, workflowExecution)
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err1 := context.loadWorkflowExecution()
		if err1 != nil {
			return err1
		}
		tBuilder := e.getTimerBuilder(&context.workflowExecution)

		scheduleID := token.ScheduleID
		di, isRunning := msBuilder.GetPendingDecision(scheduleID)

		// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
		// some extreme cassandra failure cases.
		if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
			e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.StaleMutableStateCounter)
			// Reload workflow execution history
			context.clear()
			continue Update_History_Loop
		}

		if !msBuilder.isWorkflowExecutionRunning() || !isRunning || di.Attempt != token.ScheduleAttempt ||
			di.StartedID == emptyEventID {
			return &workflow.EntityNotExistsError{Message: "Decision task not found."}
		}

		startedID := di.StartedID
		completedEvent := msBuilder.AddDecisionTaskCompletedEvent(scheduleID, startedID, request)
		if completedEvent == nil {
			return &workflow.InternalServiceError{Message: "Unable to add DecisionTaskCompleted event to history."}
		}

		failDecision := false
		var failCause workflow.DecisionTaskFailedCause
		var err error
		completedID := *completedEvent.EventId
		hasUnhandledEvents := msBuilder.HasBufferedEvents()
		isComplete := false
		transferTasks := []persistence.Task{}
		timerTasks := []persistence.Task{}
		var continueAsNewBuilder *mutableStateBuilder
		var continueAsNewTimerTasks []persistence.Task
		hasDecisionScheduleActivityTask := false

		if request.StickyAttributes == nil || request.StickyAttributes.WorkerTaskList == nil {
			e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.CompleteDecisionWithStickyDisabledCounter)
			msBuilder.executionInfo.StickyTaskList = ""
			msBuilder.executionInfo.StickyScheduleToStartTimeout = 0
		} else {
			e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.CompleteDecisionWithStickyEnabledCounter)
			msBuilder.executionInfo.StickyTaskList = request.StickyAttributes.WorkerTaskList.GetName()
			msBuilder.executionInfo.StickyScheduleToStartTimeout = request.StickyAttributes.GetScheduleToStartTimeoutSeconds()
		}
		msBuilder.executionInfo.ClientLibraryVersion = clientLibVersion
		msBuilder.executionInfo.ClientFeatureVersion = clientFeatureVersion
		msBuilder.executionInfo.ClientImpl = clientImpl

	Process_Decision_Loop:
		for _, d := range request.Decisions {
			switch *d.DecisionType {
			case workflow.DecisionTypeScheduleActivityTask:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeScheduleActivityCounter)
				targetDomainID := domainID
				attributes := d.ScheduleActivityTaskDecisionAttributes
				// First check if we need to use a different target domain to schedule activity
				if attributes.Domain != nil {
					// TODO: Error handling for ActivitySchedule failed when domain lookup fails
					domainEntry, err := e.shard.GetDomainCache().GetDomain(*attributes.Domain)
					if err != nil {
						return &workflow.InternalServiceError{Message: "Unable to schedule activity across domain."}
					}
					targetDomainID = domainEntry.GetInfo().ID
				}

				if err = validateActivityScheduleAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadScheduleActivityAttributes
					break Process_Decision_Loop
				}

				scheduleEvent, _ := msBuilder.AddActivityTaskScheduledEvent(completedID, attributes)
				transferTasks = append(transferTasks, &persistence.ActivityTask{
					DomainID:   targetDomainID,
					TaskList:   *attributes.TaskList.Name,
					ScheduleID: *scheduleEvent.EventId,
				})
				hasDecisionScheduleActivityTask = true

			case workflow.DecisionTypeCompleteWorkflowExecution:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeCompleteWorkflowCounter)
				if hasUnhandledEvents {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseUnhandledDecision
					break Process_Decision_Loop
				}

				// If the decision has more than one completion event than just pick the first one
				if isComplete {
					e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
						metrics.MultipleCompletionDecisionsCounter)
					logging.LogMultipleCompletionDecisionsEvent(e.logger, *d.DecisionType)
					continue Process_Decision_Loop
				}
				attributes := d.CompleteWorkflowExecutionDecisionAttributes
				if err = validateCompleteWorkflowExecutionAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadCompleteWorkflowExecutionAttributes
					break Process_Decision_Loop
				}
				if e := msBuilder.AddCompletedWorkflowEvent(completedID, attributes); e == nil {
					return &workflow.InternalServiceError{Message: "Unable to add complete workflow event."}
				}
				isComplete = true
			case workflow.DecisionTypeFailWorkflowExecution:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeFailWorkflowCounter)
				if hasUnhandledEvents {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseUnhandledDecision
					break Process_Decision_Loop
				}

				// If the decision has more than one completion event than just pick the first one
				if isComplete {
					e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
						metrics.MultipleCompletionDecisionsCounter)
					logging.LogMultipleCompletionDecisionsEvent(e.logger, *d.DecisionType)
					continue Process_Decision_Loop
				}
				attributes := d.FailWorkflowExecutionDecisionAttributes
				if err = validateFailWorkflowExecutionAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadFailWorkflowExecutionAttributes
					break Process_Decision_Loop
				}
				if e := msBuilder.AddFailWorkflowEvent(completedID, attributes); e == nil {
					return &workflow.InternalServiceError{Message: "Unable to add fail workflow event."}
				}
				isComplete = true
			case workflow.DecisionTypeCancelWorkflowExecution:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeCancelWorkflowCounter)
				// If new events came while we are processing the decision, we would fail this and give a chance to client
				// to process the new event.
				if hasUnhandledEvents {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseUnhandledDecision
					break Process_Decision_Loop
				}

				// If the decision has more than one completion event than just pick the first one
				if isComplete {
					e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
						metrics.MultipleCompletionDecisionsCounter)
					logging.LogMultipleCompletionDecisionsEvent(e.logger, *d.DecisionType)
					continue Process_Decision_Loop
				}
				attributes := d.CancelWorkflowExecutionDecisionAttributes
				if err = validateCancelWorkflowExecutionAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadCancelWorkflowExecutionAttributes
					break Process_Decision_Loop
				}
				msBuilder.AddWorkflowExecutionCanceledEvent(completedID, attributes)
				isComplete = true

			case workflow.DecisionTypeStartTimer:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeStartTimerCounter)
				attributes := d.StartTimerDecisionAttributes
				if err = validateTimerScheduleAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadStartTimerAttributes
					break Process_Decision_Loop
				}
				_, ti := msBuilder.AddTimerStartedEvent(completedID, attributes)
				if ti == nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseStartTimerDuplicateID
					break Process_Decision_Loop
				}
				tBuilder.AddUserTimer(ti, context.msBuilder)

			case workflow.DecisionTypeRequestCancelActivityTask:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeCancelActivityCounter)
				attributes := d.RequestCancelActivityTaskDecisionAttributes
				if err = validateActivityCancelAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadRequestCancelActivityAttributes
					break Process_Decision_Loop
				}
				activityID := *attributes.ActivityId
				actCancelReqEvent, ai, isRunning := msBuilder.AddActivityTaskCancelRequestedEvent(completedID, activityID,
					common.StringDefault(request.Identity))
				if !isRunning {
					msBuilder.AddRequestCancelActivityTaskFailedEvent(completedID, activityID,
						activityCancelationMsgActivityIDUnknown)
					continue Process_Decision_Loop
				}

				if ai.StartedID == emptyEventID {
					// We haven't started the activity yet, we can cancel the activity right away.
					msBuilder.AddActivityTaskCanceledEvent(ai.ScheduleID, ai.StartedID, *actCancelReqEvent.EventId,
						[]byte(activityCancelationMsgActivityNotStarted), common.StringDefault(request.Identity))
				}

			case workflow.DecisionTypeCancelTimer:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeCancelTimerCounter)
				attributes := d.CancelTimerDecisionAttributes
				if err = validateTimerCancelAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadCancelTimerAttributes
					break Process_Decision_Loop
				}
				if msBuilder.AddTimerCanceledEvent(completedID, attributes, common.StringDefault(request.Identity)) == nil {
					msBuilder.AddCancelTimerFailedEvent(completedID, attributes, common.StringDefault(request.Identity))
				}

			case workflow.DecisionTypeRecordMarker:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeRecordMarkerCounter)
				attributes := d.RecordMarkerDecisionAttributes
				if err = validateRecordMarkerAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadRecordMarkerAttributes
					break Process_Decision_Loop
				}
				msBuilder.AddRecordMarkerEvent(completedID, attributes)

			case workflow.DecisionTypeRequestCancelExternalWorkflowExecution:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeCancelExternalWorkflowCounter)
				attributes := d.RequestCancelExternalWorkflowExecutionDecisionAttributes
				if err = validateCancelExternalWorkflowExecutionAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadRequestCancelExternalWorkflowExecutionAttributes
					break Process_Decision_Loop
				}

				foreignDomainID := ""
				if attributes.GetDomain() == "" {
					foreignDomainID = msBuilder.executionInfo.DomainID
				} else {
					foreignDomainEntry, err := e.shard.GetDomainCache().GetDomain(attributes.GetDomain())
					if err != nil {
						return &workflow.InternalServiceError{
							Message: fmt.Sprintf("Unable to cancel workflow across domain: %v.", attributes.GetDomain())}
					}
					foreignDomainID = foreignDomainEntry.GetInfo().ID
				}

				cancelRequestID := uuid.New()
				wfCancelReqEvent, _ := msBuilder.AddRequestCancelExternalWorkflowExecutionInitiatedEvent(completedID,
					cancelRequestID, attributes)
				if wfCancelReqEvent == nil {
					return &workflow.InternalServiceError{Message: "Unable to add external cancel workflow request."}
				}

				transferTasks = append(transferTasks, &persistence.CancelExecutionTask{
					TargetDomainID:          foreignDomainID,
					TargetWorkflowID:        attributes.GetWorkflowId(),
					TargetRunID:             attributes.GetRunId(),
					TargetChildWorkflowOnly: attributes.GetChildWorkflowOnly(),
					InitiatedID:             wfCancelReqEvent.GetEventId(),
				})

			case workflow.DecisionTypeSignalExternalWorkflowExecution:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeSignalExternalWorkflowCounter)

				attributes := d.SignalExternalWorkflowExecutionDecisionAttributes
				if err = validateSignalExternalWorkflowExecutionAttributes(attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadSignalWorkflowExecutionAttributes
					break Process_Decision_Loop
				}

				foreignDomainID := ""
				if attributes.GetDomain() == "" {
					foreignDomainID = msBuilder.executionInfo.DomainID
				} else {
					foreignDomainEntry, err := e.shard.GetDomainCache().GetDomain(attributes.GetDomain())
					if err != nil {
						return &workflow.InternalServiceError{
							Message: fmt.Sprintf("Unable to signal workflow across domain: %v.", attributes.GetDomain())}
					}
					foreignDomainID = foreignDomainEntry.GetInfo().ID
				}

				signalRequestID := uuid.New() // for deduplicate
				wfSignalReqEvent := msBuilder.AddSignalExternalWorkflowExecutionInitiatedEvent(completedID,
					signalRequestID, attributes)
				if wfSignalReqEvent == nil {
					return &workflow.InternalServiceError{Message: "Unable to add external signal workflow request."}
				}

				transferTasks = append(transferTasks, &persistence.SignalExecutionTask{
					TargetDomainID:          foreignDomainID,
					TargetWorkflowID:        attributes.Execution.GetWorkflowId(),
					TargetRunID:             attributes.Execution.GetRunId(),
					TargetChildWorkflowOnly: attributes.GetChildWorkflowOnly(),
					InitiatedID:             wfSignalReqEvent.GetEventId(),
				})

			case workflow.DecisionTypeContinueAsNewWorkflowExecution:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeContinueAsNewCounter)
				if hasUnhandledEvents {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseUnhandledDecision
					break Process_Decision_Loop
				}

				// If the decision has more than one completion event than just pick the first one
				if isComplete {
					e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
						metrics.MultipleCompletionDecisionsCounter)
					logging.LogMultipleCompletionDecisionsEvent(e.logger, *d.DecisionType)
					continue Process_Decision_Loop
				}
				attributes := d.ContinueAsNewWorkflowExecutionDecisionAttributes
				if err = validateContinueAsNewWorkflowExecutionAttributes(msBuilder.executionInfo, attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadContinueAsNewAttributes
					break Process_Decision_Loop
				}

				domainEntry, err := e.shard.GetDomainCache().GetDomainByID(domainID)
				if err != nil {
					return err
				}
				domainName := domainEntry.GetInfo().Name

				// Extract parentDomainName so it can be passed down to next run of workflow execution
				var parentDomainName string
				if msBuilder.hasParentExecution() {
					parentDomainID := msBuilder.executionInfo.ParentDomainID
					parentDomainEntry, err := e.shard.GetDomainCache().GetDomainByID(parentDomainID)
					if err != nil {
						return err
					}
					parentDomainName = parentDomainEntry.GetInfo().Name
				}

				runID := uuid.New()
				_, newStateBuilder, err := msBuilder.AddContinueAsNewEvent(completedID, domainID, domainName, runID,
					parentDomainName, attributes)
				if err != nil {
					return err
				}

				// add timer task to new workflow
				duration := time.Duration(*attributes.ExecutionStartToCloseTimeoutSeconds) * time.Second
				continueAsNewTimerTasks = []persistence.Task{&persistence.WorkflowTimeoutTask{
					VisibilityTimestamp: e.shard.GetTimeSource().Now().Add(duration),
				}}
				msBuilder.continueAsNew.TimerTasks = continueAsNewTimerTasks

				isComplete = true
				continueAsNewBuilder = newStateBuilder

			case workflow.DecisionTypeStartChildWorkflowExecution:
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.DecisionTypeChildWorkflowCounter)
				targetDomainID := domainID
				attributes := d.StartChildWorkflowExecutionDecisionAttributes
				if err = validateStartChildExecutionAttributes(msBuilder.executionInfo, attributes); err != nil {
					failDecision = true
					failCause = workflow.DecisionTaskFailedCauseBadStartChildExecutionAttributes
					break Process_Decision_Loop
				}

				// First check if we need to use a different target domain to schedule child execution
				if attributes.Domain != nil {
					// TODO: Error handling for DecisionType_StartChildWorkflowExecution failed when domain lookup fails
					domainEntry, err := e.shard.GetDomainCache().GetDomain(*attributes.Domain)
					if err != nil {
						return &workflow.InternalServiceError{Message: "Unable to schedule child execution across domain."}
					}
					targetDomainID = domainEntry.GetInfo().ID
				}

				requestID := uuid.New()
				initiatedEvent, _ := msBuilder.AddStartChildWorkflowExecutionInitiatedEvent(completedID, requestID, attributes)
				transferTasks = append(transferTasks, &persistence.StartChildExecutionTask{
					TargetDomainID:   targetDomainID,
					TargetWorkflowID: *attributes.WorkflowId,
					InitiatedID:      *initiatedEvent.EventId,
				})

			default:
				return &workflow.BadRequestError{Message: fmt.Sprintf("Unknown decision type: %v", *d.DecisionType)}
			}
		}

		if failDecision {
			e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.FailedDecisionsCounter)
			logging.LogDecisionFailedEvent(e.logger, domainID, token.WorkflowID, token.RunID, failCause)
			var err1 error
			msBuilder, err1 = e.failDecision(context, scheduleID, startedID, failCause, request)
			if err1 != nil {
				return err1
			}
			isComplete = false
			hasUnhandledEvents = true
			continueAsNewBuilder = nil
		}

		if tt := tBuilder.GetUserTimerTaskIfNeeded(msBuilder); tt != nil {
			timerTasks = append(timerTasks, tt)
		}
		if hasDecisionScheduleActivityTask {
			if tt := tBuilder.GetActivityTimerTaskIfNeeded(msBuilder); tt != nil {
				timerTasks = append(timerTasks, tt)
			}
		}

		// Schedule another decision task if new events came in during this decision
		if hasUnhandledEvents {
			di := msBuilder.AddDecisionTaskScheduledEvent()
			transferTasks = append(transferTasks, &persistence.DecisionTask{
				DomainID:   domainID,
				TaskList:   di.Tasklist,
				ScheduleID: di.ScheduleID,
			})
			if msBuilder.isStickyTaskListEnabled() {
				tBuilder := e.getTimerBuilder(&context.workflowExecution)
				stickyTaskTimeoutTimer := tBuilder.AddScheduleToStartDecisionTimoutTask(di.ScheduleID, di.Attempt,
					msBuilder.executionInfo.StickyScheduleToStartTimeout)
				timerTasks = append(timerTasks, stickyTaskTimeoutTimer)
			}
		}

		if isComplete {
			tranT, timerT, err := e.getDeleteWorkflowTasks(domainID, tBuilder)
			if err != nil {
				return err
			}
			transferTasks = append(transferTasks, tranT)
			timerTasks = append(timerTasks, timerT)
		}

		// Generate a transaction ID for appending events to history
		transactionID, err3 := e.shard.GetNextTransferTaskID()
		if err3 != nil {
			return err3
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict then reload
		// the history and try the operation again.
		var updateErr error
		if continueAsNewBuilder != nil {
			updateErr = context.continueAsNewWorkflowExecution(request.ExecutionContext, continueAsNewBuilder,
				transferTasks, timerTasks, transactionID)
		} else {
			updateErr = context.updateWorkflowExecutionWithContext(request.ExecutionContext, transferTasks, timerTasks,
				transactionID)
		}

		if updateErr != nil {
			if updateErr == ErrConflict {
				e.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope,
					metrics.ConcurrencyUpdateFailureCounter)
				continue Update_History_Loop
			}

			return updateErr
		}

		// add continueAsNewTimerTask
		timerTasks = append(timerTasks, continueAsNewTimerTasks...)
		// Inform timer about the new ones.
		e.timerProcessor.NotifyNewTimers(e.currentClusterName, timerTasks)

		return err
	}

	return ErrMaxAttemptsExceeded
}

func (e *historyEngineImpl) RespondDecisionTaskFailed(req *h.RespondDecisionTaskFailedRequest) error {
	domainID, err := getDomainUUID(req.DomainUUID)
	if err != nil {
		return err
	}
	request := req.FailedRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return ErrDeserializingToken
	}

	workflowExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(token.WorkflowID),
		RunId:      common.StringPtr(token.RunID),
	}

	return e.updateWorkflowExecution(domainID, workflowExecution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			scheduleID := token.ScheduleID
			di, isRunning := msBuilder.GetPendingDecision(scheduleID)
			if !isRunning || di.Attempt != token.ScheduleAttempt || di.StartedID == emptyEventID {
				return nil, &workflow.EntityNotExistsError{Message: "Decision task not found."}
			}

			msBuilder.AddDecisionTaskFailedEvent(di.ScheduleID, di.StartedID, request.GetCause(), request.Details,
				request.GetIdentity())

			return nil, nil
		})
}

// RespondActivityTaskCompleted completes an activity task.
func (e *historyEngineImpl) RespondActivityTaskCompleted(req *h.RespondActivityTaskCompletedRequest) error {
	domainID, err := getDomainUUID(req.DomainUUID)
	if err != nil {
		return err
	}
	request := req.CompleteRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return ErrDeserializingToken
	}

	workflowExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(token.WorkflowID),
		RunId:      common.StringPtr(token.RunID),
	}

	return e.updateWorkflowExecution(domainID, workflowExecution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call CompleteActivityById, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, msBuilder)
				if err0 != nil {
					return nil, err0
				}
			}
			ai, isRunning := msBuilder.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRespondActivityTaskCompletedScope, metrics.StaleMutableStateCounter)
				return nil, ErrStaleState
			}

			if !isRunning || ai.StartedID == emptyEventID {
				return nil, ErrActivityTaskNotFound
			}

			startedID := ai.StartedID
			if msBuilder.AddActivityTaskCompletedEvent(scheduleID, startedID, request) == nil {
				// Unable to add ActivityTaskCompleted event to history
				return nil, &workflow.InternalServiceError{Message: "Unable to add ActivityTaskCompleted event to history."}
			}

			return nil, nil
		})
}

// RespondActivityTaskFailed completes an activity task failure.
func (e *historyEngineImpl) RespondActivityTaskFailed(req *h.RespondActivityTaskFailedRequest) error {
	domainID, err := getDomainUUID(req.DomainUUID)
	if err != nil {
		return err
	}
	request := req.FailedRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return ErrDeserializingToken
	}

	workflowExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(token.WorkflowID),
		RunId:      common.StringPtr(token.RunID),
	}

	return e.updateWorkflowExecution(domainID, workflowExecution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call CompleteActivityById, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, msBuilder)
				if err0 != nil {
					return nil, err0
				}
			}
			ai, isRunning := msBuilder.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRespondActivityTaskFailedScope, metrics.StaleMutableStateCounter)
				return nil, ErrStaleState
			}

			if !isRunning || ai.StartedID == emptyEventID {
				return nil, ErrActivityTaskNotFound
			}

			startedID := ai.StartedID
			if msBuilder.AddActivityTaskFailedEvent(scheduleID, startedID, request) == nil {
				// Unable to add ActivityTaskFailed event to history
				return nil, &workflow.InternalServiceError{Message: "Unable to add ActivityTaskFailed event to history."}
			}

			return nil, nil
		})
}

// RespondActivityTaskCanceled completes an activity task failure.
func (e *historyEngineImpl) RespondActivityTaskCanceled(req *h.RespondActivityTaskCanceledRequest) error {
	domainID, err := getDomainUUID(req.DomainUUID)
	if err != nil {
		return err
	}
	request := req.CancelRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return ErrDeserializingToken
	}

	workflowExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(token.WorkflowID),
		RunId:      common.StringPtr(token.RunID),
	}

	return e.updateWorkflowExecution(domainID, workflowExecution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call CompleteActivityById, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, msBuilder)
				if err0 != nil {
					return nil, err0
				}
			}
			ai, isRunning := msBuilder.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRespondActivityTaskCanceledScope, metrics.StaleMutableStateCounter)
				return nil, ErrStaleState
			}

			if !isRunning || ai.StartedID == emptyEventID {
				return nil, ErrActivityTaskNotFound
			}

			if msBuilder.AddActivityTaskCanceledEvent(scheduleID, ai.StartedID, ai.CancelRequestID, request.Details,
				common.StringDefault(request.Identity)) == nil {
				// Unable to add ActivityTaskCanceled event to history
				return nil, &workflow.InternalServiceError{Message: "Unable to add ActivityTaskCanceled event to history."}
			}

			return nil, nil
		})

}

// RecordActivityTaskHeartbeat records an hearbeat for a task.
// This method can be used for two purposes.
// - For reporting liveness of the activity.
// - For reporting progress of the activity, this can be done even if the liveness is not configured.
func (e *historyEngineImpl) RecordActivityTaskHeartbeat(
	req *h.RecordActivityTaskHeartbeatRequest) (*workflow.RecordActivityTaskHeartbeatResponse, error) {
	domainID, err := getDomainUUID(req.DomainUUID)
	if err != nil {
		return nil, err
	}
	request := req.HeartbeatRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return nil, ErrDeserializingToken
	}

	workflowExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(token.WorkflowID),
		RunId:      common.StringPtr(token.RunID),
	}

	var cancelRequested bool
	err = e.updateWorkflowExecution(domainID, workflowExecution, false, false,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				e.logger.Errorf("Heartbeat failed ")
				return nil, ErrWorkflowCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call RecordActivityHeartbeatByID, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, msBuilder)
				if err0 != nil {
					return nil, err0
				}
			}
			ai, isRunning := msBuilder.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRecordActivityTaskHeartbeatScope, metrics.StaleMutableStateCounter)
				return nil, ErrStaleState
			}

			if !isRunning || ai.StartedID == emptyEventID {
				e.logger.Debugf("Activity HeartBeat: scheduleEventID: %v, ActivityInfo: %+v, Exist: %v", scheduleID, ai,
					isRunning)
				return nil, ErrActivityTaskNotFound
			}

			cancelRequested = ai.CancelRequested

			e.logger.Debugf("Activity HeartBeat: scheduleEventID: %v, ActivityInfo: %+v, CancelRequested: %v",
				scheduleID, ai, cancelRequested)

			// Save progress and last HB reported time.
			msBuilder.updateActivityProgress(ai, request)

			return nil, nil
		})

	if err != nil {
		return &workflow.RecordActivityTaskHeartbeatResponse{}, err
	}

	return &workflow.RecordActivityTaskHeartbeatResponse{CancelRequested: common.BoolPtr(cancelRequested)}, nil
}

// RequestCancelWorkflowExecution records request cancellation event for workflow execution
func (e *historyEngineImpl) RequestCancelWorkflowExecution(
	req *h.RequestCancelWorkflowExecutionRequest) error {
	domainID, err := getDomainUUID(req.DomainUUID)
	if err != nil {
		return err
	}
	request := req.CancelRequest
	parentExecution := req.ExternalWorkflowExecution
	childWorkflowOnly := req.GetChildWorkflowOnly()
	execution := workflow.WorkflowExecution{
		WorkflowId: request.WorkflowExecution.WorkflowId,
		RunId:      request.WorkflowExecution.RunId,
	}

	return e.updateWorkflowExecution(domainID, execution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			if childWorkflowOnly {
				parentWorkflowID := msBuilder.executionInfo.ParentWorkflowID
				parentRunID := msBuilder.executionInfo.ParentRunID
				if parentExecution.GetWorkflowId() != parentWorkflowID ||
					parentExecution.GetRunId() != parentRunID {
					return nil, ErrWorkflowParent
				}
			}

			isCancelRequested, cancelRequestID := msBuilder.isCancelRequested()
			if isCancelRequested {
				cancelRequest := req.CancelRequest
				if cancelRequest.RequestId != nil {
					requestID := *cancelRequest.RequestId
					if requestID != "" && cancelRequestID == requestID {
						return nil, nil
					}
				}
				// if we consider workflow cancellation idempotent, then this error is redundant
				// this error maybe useful if this API is invoked by external, not decision from transfer queue
				return nil, ErrCancellationAlreadyRequested
			}

			if msBuilder.AddWorkflowExecutionCancelRequestedEvent("", req) == nil {
				return nil, &workflow.InternalServiceError{Message: "Unable to cancel workflow execution."}
			}

			return nil, nil
		})
}

func (e *historyEngineImpl) SignalWorkflowExecution(signalRequest *h.SignalWorkflowExecutionRequest) error {
	domainID, err := getDomainUUID(signalRequest.DomainUUID)
	if err != nil {
		return err
	}
	request := signalRequest.SignalRequest
	parentExecution := signalRequest.ExternalWorkflowExecution
	childWorkflowOnly := signalRequest.GetChildWorkflowOnly()
	execution := workflow.WorkflowExecution{
		WorkflowId: request.WorkflowExecution.WorkflowId,
		RunId:      request.WorkflowExecution.RunId,
	}

	return e.updateWorkflowExecution(domainID, execution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			if childWorkflowOnly {
				parentWorkflowID := msBuilder.executionInfo.ParentWorkflowID
				parentRunID := msBuilder.executionInfo.ParentRunID
				if parentExecution.GetWorkflowId() != parentWorkflowID ||
					parentExecution.GetRunId() != parentRunID {
					return nil, ErrWorkflowParent
				}
			}

			// deduplicate by request id for signal decision
			if requestID := request.GetRequestId(); requestID != "" {
				if msBuilder.isSignalRequested(requestID) {
					return nil, nil
				}
				msBuilder.addSignalRequested(requestID)
			}

			if msBuilder.AddWorkflowExecutionSignaled(request) == nil {
				return nil, &workflow.InternalServiceError{Message: "Unable to signal workflow execution."}
			}

			return nil, nil
		})
}

func (e *historyEngineImpl) SignalWithStartWorkflowExecution(signalWithStartRequest *h.SignalWithStartWorkflowExecutionRequest) (
	retResp *workflow.StartWorkflowExecutionResponse, retError error) {

	domainID, err := getDomainUUID(signalWithStartRequest.DomainUUID)
	if err != nil {
		return nil, err
	}
	sRequest := signalWithStartRequest.SignalWithStartRequest
	execution := workflow.WorkflowExecution{
		WorkflowId: sRequest.WorkflowId,
	}

	isBrandNew := false
	prevRunID := ""
	attempt := 0

	context, release, err0 := e.historyCache.getOrCreateWorkflowExecution(domainID, execution)

	if err0 == nil {
		defer func() { release(retError) }()
	Just_Signal_Loop:
		for ; attempt < conditionalRetryCount; attempt++ {
			msBuilder, err1 := context.loadWorkflowExecution()
			if err1 != nil {
				if _, ok := err1.(*workflow.EntityNotExistsError); ok {
					isBrandNew = true
					break
				}
				return nil, err1
			}
			if !msBuilder.isWorkflowExecutionRunning() {
				prevRunID = context.workflowExecution.GetRunId()
				break
			}

			if msBuilder.AddWorkflowExecutionSignaled(getSignalRequest(sRequest)) == nil {
				return nil, &workflow.InternalServiceError{Message: "Unable to signal workflow execution."}
			}

			var transferTasks []persistence.Task
			var timerTasks []persistence.Task
			// Create a transfer task to schedule a decision task
			if !msBuilder.HasPendingDecisionTask() {
				di := msBuilder.AddDecisionTaskScheduledEvent()
				transferTasks = append(transferTasks, &persistence.DecisionTask{
					DomainID:   domainID,
					TaskList:   di.Tasklist,
					ScheduleID: di.ScheduleID,
				})
				if msBuilder.isStickyTaskListEnabled() {
					tBuilder := e.getTimerBuilder(&context.workflowExecution)
					stickyTaskTimeoutTimer := tBuilder.AddScheduleToStartDecisionTimoutTask(di.ScheduleID, di.Attempt,
						msBuilder.executionInfo.StickyScheduleToStartTimeout)
					timerTasks = append(timerTasks, stickyTaskTimeoutTimer)
				}
			}
			// Generate a transaction ID for appending events to history
			transactionID, err2 := e.shard.GetNextTransferTaskID()
			if err2 != nil {
				return nil, err2
			}

			// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict then reload
			// the history and try the operation again.
			if err := context.updateWorkflowExecution(transferTasks, timerTasks, transactionID); err != nil {
				if err == ErrConflict {
					continue Just_Signal_Loop
				}
				return nil, err
			}
			e.timerProcessor.NotifyNewTimers(e.currentClusterName, timerTasks)
			return &workflow.StartWorkflowExecutionResponse{RunId: context.workflowExecution.RunId}, nil
		} // end for Just_Signal_Loop
		if attempt == conditionalRetryCount {
			return nil, ErrMaxAttemptsExceeded
		}
	} else {
		if _, ok := err0.(*workflow.EntityNotExistsError); ok {
			isBrandNew = true
		} else {
			return nil, err0
		}
	}

	// Start workflow and signal
	startRequest := getStartRequest(domainID, sRequest)
	request := startRequest.StartRequest
	err = validateStartWorkflowExecutionRequest(request)
	if err != nil {
		return nil, err
	}

	execution = workflow.WorkflowExecution{
		WorkflowId: request.WorkflowId,
		RunId:      common.StringPtr(uuid.New()),
	}

	// Generate first decision task event.
	taskList := request.TaskList.GetName()
	msBuilder := newMutableStateBuilder(e.shard.GetConfig(), e.logger)
	startedEvent := msBuilder.AddWorkflowExecutionStartedEvent(execution, startRequest)
	if startedEvent == nil {
		return nil, &workflow.InternalServiceError{Message: "Failed to add workflow execution started event."}
	}

	if msBuilder.AddWorkflowExecutionSignaled(getSignalRequest(sRequest)) == nil {
		return nil, &workflow.InternalServiceError{Message: "Failed to add workflow execution signaled event."}
	}

	var transferTasks []persistence.Task
	decisionScheduleID := emptyEventID
	decisionStartID := emptyEventID
	decisionTimeout := int32(0)
	di := msBuilder.AddDecisionTaskScheduledEvent()
	if di == nil {
		return nil, &workflow.InternalServiceError{Message: "Failed to add decision scheduled event."}
	}

	transferTasks = []persistence.Task{&persistence.DecisionTask{
		DomainID: domainID, TaskList: taskList, ScheduleID: di.ScheduleID,
	}}
	decisionScheduleID = di.ScheduleID
	decisionStartID = di.StartedID
	decisionTimeout = di.DecisionTimeout

	duration := time.Duration(*request.ExecutionStartToCloseTimeoutSeconds) * time.Second
	timerTasks := []persistence.Task{&persistence.WorkflowTimeoutTask{
		VisibilityTimestamp: e.shard.GetTimeSource().Now().Add(duration),
	}}
	// Serialize the history
	serializedHistory, serializedError := msBuilder.hBuilder.Serialize()
	if serializedError != nil {
		logging.LogHistorySerializationErrorEvent(e.logger, serializedError, fmt.Sprintf(
			"HistoryEventBatch serialization error on start workflow.  WorkflowID: %v, RunID: %v",
			execution.GetWorkflowId(), execution.GetRunId()))
		return nil, serializedError
	}

	err = e.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
		DomainID:  domainID,
		Execution: execution,
		// It is ok to use 0 for TransactionID because RunID is unique so there are
		// no potential duplicates to override.
		TransactionID: 0,
		FirstEventID:  startedEvent.GetEventId(),
		Events:        serializedHistory,
	})
	if err != nil {
		return nil, err
	}
	msBuilder.executionInfo.LastFirstEventID = startedEvent.GetEventId()

	createWorkflow := func(isBrandNew bool, prevRunID string) (string, error) {
		_, err = e.shard.CreateWorkflowExecution(&persistence.CreateWorkflowExecutionRequest{
			RequestID:                   common.StringDefault(request.RequestId),
			DomainID:                    domainID,
			Execution:                   execution,
			InitiatedID:                 emptyEventID,
			TaskList:                    *request.TaskList.Name,
			WorkflowTypeName:            *request.WorkflowType.Name,
			WorkflowTimeout:             *request.ExecutionStartToCloseTimeoutSeconds,
			DecisionTimeoutValue:        *request.TaskStartToCloseTimeoutSeconds,
			ExecutionContext:            nil,
			NextEventID:                 msBuilder.GetNextEventID(),
			LastProcessedEvent:          emptyEventID,
			TransferTasks:               transferTasks,
			DecisionScheduleID:          decisionScheduleID, // TODO add decision version, when doing crodd DC
			DecisionStartedID:           decisionStartID,
			DecisionStartToCloseTimeout: decisionTimeout,
			TimerTasks:                  timerTasks,
			ContinueAsNew:               !isBrandNew,
			PreviousRunID:               prevRunID,
		})

		if err != nil {
			switch t := err.(type) {
			case *persistence.WorkflowExecutionAlreadyStartedError:
				if t.StartRequestID == common.StringDefault(request.RequestId) {
					e.deleteEvents(domainID, execution)
					return t.RunID, nil
				}
			case *persistence.ShardOwnershipLostError:
				e.deleteEvents(domainID, execution)
			}
			return "", err
		}
		return execution.GetRunId(), nil
	}

	// try to create the workflow execution
	resultRunID, err := createWorkflow(isBrandNew, prevRunID) // (true, "") or (false, "prevRunID")
	if err == nil {
		e.timerProcessor.NotifyNewTimers(e.currentClusterName, timerTasks)

		return &workflow.StartWorkflowExecutionResponse{
			RunId: common.StringPtr(resultRunID),
		}, nil
	}
	return nil, err
}

// RemoveSignalMutableState remove the signal request id in signal_requested for deduplicate
func (e *historyEngineImpl) RemoveSignalMutableState(request *h.RemoveSignalMutableStateRequest) error {
	domainID, err := getDomainUUID(request.DomainUUID)
	if err != nil {
		return err
	}
	execution := workflow.WorkflowExecution{
		WorkflowId: request.WorkflowExecution.WorkflowId,
		RunId:      request.WorkflowExecution.RunId,
	}

	return e.updateWorkflowExecution(domainID, execution, false, false,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			msBuilder.deleteSignalRequested(request.GetRequestId())

			return nil, nil
		})
}

func (e *historyEngineImpl) TerminateWorkflowExecution(terminateRequest *h.TerminateWorkflowExecutionRequest) error {
	domainID, err := getDomainUUID(terminateRequest.DomainUUID)
	if err != nil {
		return err
	}
	request := terminateRequest.TerminateRequest
	execution := workflow.WorkflowExecution{
		WorkflowId: request.WorkflowExecution.WorkflowId,
		RunId:      request.WorkflowExecution.RunId,
	}

	return e.updateWorkflowExecution(domainID, execution, true, false,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			if msBuilder.AddWorkflowExecutionTerminatedEvent(request) == nil {
				return nil, &workflow.InternalServiceError{Message: "Unable to terminate workflow execution."}
			}

			return nil, nil
		})
}

// ScheduleDecisionTask schedules a decision if no outstanding decision found
func (e *historyEngineImpl) ScheduleDecisionTask(scheduleRequest *h.ScheduleDecisionTaskRequest) error {
	domainID, err := getDomainUUID(scheduleRequest.DomainUUID)
	if err != nil {
		return err
	}
	execution := workflow.WorkflowExecution{
		WorkflowId: scheduleRequest.WorkflowExecution.WorkflowId,
		RunId:      scheduleRequest.WorkflowExecution.RunId,
	}

	return e.updateWorkflowExecution(domainID, execution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			// Noop

			return nil, nil
		})
}

// RecordChildExecutionCompleted records the completion of child execution into parent execution history
func (e *historyEngineImpl) RecordChildExecutionCompleted(completionRequest *h.RecordChildExecutionCompletedRequest) error {
	domainID, err := getDomainUUID(completionRequest.DomainUUID)
	if err != nil {
		return err
	}
	execution := workflow.WorkflowExecution{
		WorkflowId: completionRequest.WorkflowExecution.WorkflowId,
		RunId:      completionRequest.WorkflowExecution.RunId,
	}

	return e.updateWorkflowExecution(domainID, execution, false, true,
		func(msBuilder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error) {
			if !msBuilder.isWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			initiatedID := *completionRequest.InitiatedId
			completedExecution := completionRequest.CompletedExecution
			completionEvent := completionRequest.CompletionEvent

			// Check mutable state to make sure child execution is in pending child executions
			ci, isRunning := msBuilder.GetChildExecutionInfo(initiatedID)
			if !isRunning || ci.StartedID == emptyEventID {
				return nil, &workflow.EntityNotExistsError{Message: "Pending child execution not found."}
			}

			switch *completionEvent.EventType {
			case workflow.EventTypeWorkflowExecutionCompleted:
				attributes := completionEvent.WorkflowExecutionCompletedEventAttributes
				msBuilder.AddChildWorkflowExecutionCompletedEvent(initiatedID, completedExecution, attributes)
			case workflow.EventTypeWorkflowExecutionFailed:
				attributes := completionEvent.WorkflowExecutionFailedEventAttributes
				msBuilder.AddChildWorkflowExecutionFailedEvent(initiatedID, completedExecution, attributes)
			case workflow.EventTypeWorkflowExecutionCanceled:
				attributes := completionEvent.WorkflowExecutionCanceledEventAttributes
				msBuilder.AddChildWorkflowExecutionCanceledEvent(initiatedID, completedExecution, attributes)
			case workflow.EventTypeWorkflowExecutionTerminated:
				attributes := completionEvent.WorkflowExecutionTerminatedEventAttributes
				msBuilder.AddChildWorkflowExecutionTerminatedEvent(initiatedID, completedExecution, attributes)
			case workflow.EventTypeWorkflowExecutionTimedOut:
				attributes := completionEvent.WorkflowExecutionTimedOutEventAttributes
				msBuilder.AddChildWorkflowExecutionTimedOutEvent(initiatedID, completedExecution, attributes)
			}

			return nil, nil
		})
}

func (e *historyEngineImpl) ReplicateEvents(replicateRequest *h.ReplicateEventsRequest) error {
	return e.replicator.ApplyEvents(replicateRequest)
}

func (e *historyEngineImpl) updateWorkflowExecution(domainID string, execution workflow.WorkflowExecution,
	createDeletionTask, createDecisionTask bool,
	action func(builder *mutableStateBuilder, tBuilder *timerBuilder) ([]persistence.Task, error)) (retError error) {

	context, release, err0 := e.historyCache.getOrCreateWorkflowExecution(domainID, execution)
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err1 := context.loadWorkflowExecution()
		if err1 != nil {
			return err1
		}
		tBuilder := e.getTimerBuilder(&context.workflowExecution)

		var timerTasks []persistence.Task
		var err error
		// conduct caller action
		if timerTasks, err = action(msBuilder, tBuilder); err != nil {
			if err == ErrStaleState {
				// Handler detected that cached workflow mutable could potentially be stale
				// Reload workflow execution history
				context.clear()
				continue Update_History_Loop
			}

			// Returned error back to the caller
			return err
		}

		var transferTasks []persistence.Task

		if createDeletionTask {
			tranT, timerT, err := e.getDeleteWorkflowTasks(domainID, tBuilder)
			if err != nil {
				return err
			}
			transferTasks = append(transferTasks, tranT)
			timerTasks = append(timerTasks, timerT)
		}

		if createDecisionTask {
			// Create a transfer task to schedule a decision task
			if !msBuilder.HasPendingDecisionTask() {
				di := msBuilder.AddDecisionTaskScheduledEvent()
				transferTasks = append(transferTasks, &persistence.DecisionTask{
					DomainID:   domainID,
					TaskList:   di.Tasklist,
					ScheduleID: di.ScheduleID,
				})
				if msBuilder.isStickyTaskListEnabled() {
					tBuilder := e.getTimerBuilder(&context.workflowExecution)
					stickyTaskTimeoutTimer := tBuilder.AddScheduleToStartDecisionTimoutTask(di.ScheduleID, di.Attempt,
						msBuilder.executionInfo.StickyScheduleToStartTimeout)
					timerTasks = append(timerTasks, stickyTaskTimeoutTimer)
				}
			}
		}

		// Generate a transaction ID for appending events to history
		transactionID, err2 := e.shard.GetNextTransferTaskID()
		if err2 != nil {
			return err2
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict then reload
		// the history and try the operation again.
		if err := context.updateWorkflowExecution(transferTasks, timerTasks, transactionID); err != nil {
			if err == ErrConflict {
				continue Update_History_Loop
			}
			return err
		}
		e.timerProcessor.NotifyNewTimers(e.currentClusterName, timerTasks)
		return nil
	}
	return ErrMaxAttemptsExceeded
}

func (e *historyEngineImpl) getDeleteWorkflowTasks(
	domainID string,
	tBuilder *timerBuilder,
) (persistence.Task, persistence.Task, error) {

	// Create a transfer task to close workflow execution
	closeTask := &persistence.CloseExecutionTask{}

	// Generate a timer task to cleanup history events for this workflow execution
	var retentionInDays int32
	domainEntry, err := e.shard.GetDomainCache().GetDomainByID(domainID)
	if err != nil {
		if _, ok := err.(*workflow.EntityNotExistsError); !ok {
			return nil, nil, err
		}
	} else {
		retentionInDays = domainEntry.GetConfig().Retention
	}
	cleanupTask := tBuilder.createDeleteHistoryEventTimerTask(time.Duration(retentionInDays) * time.Hour * 24)

	return closeTask, cleanupTask, nil
}

func (e *historyEngineImpl) createRecordDecisionTaskStartedResponse(domainID string, msBuilder *mutableStateBuilder,
	di *decisionInfo, identity string) *h.RecordDecisionTaskStartedResponse {
	response := &h.RecordDecisionTaskStartedResponse{}
	response.WorkflowType = msBuilder.getWorkflowType()
	if msBuilder.previousDecisionStartedEvent() != emptyEventID {
		response.PreviousStartedEventId = common.Int64Ptr(msBuilder.previousDecisionStartedEvent())
	}

	// Starting decision could result in different scheduleID if decision was transient and new new events came in
	// before it was started.
	response.ScheduledEventId = common.Int64Ptr(di.ScheduleID)
	response.StartedEventId = common.Int64Ptr(di.StartedID)
	response.StickyExecutionEnabled = common.BoolPtr(msBuilder.isStickyTaskListEnabled())
	response.NextEventId = common.Int64Ptr(msBuilder.GetNextEventID())
	response.Attempt = common.Int64Ptr(di.Attempt)
	if di.Attempt > 0 {
		// This decision is retried from mutable state
		// Also return schedule and started which are not written to history yet
		scheduledEvent, startedEvent := msBuilder.createTransientDecisionEvents(di, identity)
		response.DecisionInfo = &workflow.TransientDecisionInfo{}
		response.DecisionInfo.ScheduledEvent = scheduledEvent
		response.DecisionInfo.StartedEvent = startedEvent
	}

	return response
}

func (e *historyEngineImpl) deleteEvents(domainID string, execution workflow.WorkflowExecution) {
	// We created the history events but failed to create workflow execution, so cleanup the history which could cause
	// us to leak history events which are never cleaned up. Cleaning up the events is absolutely safe here as they
	// are always created for a unique run_id which is not visible beyond this call yet.
	// TODO: Handle error on deletion of execution history
	e.historyMgr.DeleteWorkflowExecutionHistory(&persistence.DeleteWorkflowExecutionHistoryRequest{
		DomainID:  domainID,
		Execution: execution,
	})
}

func (e *historyEngineImpl) failDecision(context *workflowExecutionContext, scheduleID, startedID int64,
	cause workflow.DecisionTaskFailedCause, request *workflow.RespondDecisionTaskCompletedRequest) (*mutableStateBuilder,
	error) {
	// Clear any updates we have accumulated so far
	context.clear()

	// Reload workflow execution so we can apply the decision task failure event
	msBuilder, err := context.loadWorkflowExecution()
	if err != nil {
		return nil, err
	}

	msBuilder.AddDecisionTaskFailedEvent(scheduleID, startedID, cause, nil, request.GetIdentity())

	// Return new builder back to the caller for further updates
	return msBuilder, nil
}

func (e *historyEngineImpl) getTimerBuilder(we *workflow.WorkflowExecution) *timerBuilder {
	lg := e.logger.WithFields(bark.Fields{
		logging.TagWorkflowExecutionID: we.WorkflowId,
		logging.TagWorkflowRunID:       we.RunId,
	})
	return newTimerBuilder(e.shard.GetConfig(), lg, common.NewRealTimeSource())
}

func (s *shardContextWrapper) UpdateWorkflowExecution(request *persistence.UpdateWorkflowExecutionRequest) error {
	err := s.ShardContext.UpdateWorkflowExecution(request)
	if err == nil {
		if len(request.TransferTasks) > 0 {
			s.txProcessor.NotifyNewTask(s.currentClusterName, s.GetCurrentTime(s.currentClusterName))
		}
		if len(request.ReplicationTasks) > 0 {
			s.replcatorProcessor.notifyNewTask()
		}
	}
	return err
}

func (s *shardContextWrapper) CreateWorkflowExecution(request *persistence.CreateWorkflowExecutionRequest) (
	*persistence.CreateWorkflowExecutionResponse, error) {
	resp, err := s.ShardContext.CreateWorkflowExecution(request)
	if err == nil {
		if len(request.TransferTasks) > 0 {
			s.txProcessor.NotifyNewTask(s.currentClusterName, s.GetCurrentTime(s.currentClusterName))
		}
		if len(request.ReplicationTasks) > 0 {
			s.replcatorProcessor.notifyNewTask()
		}
	}
	return resp, err
}

func (s *shardContextWrapper) NotifyNewHistoryEvent(event *historyEventNotification) error {
	s.historyEventNotifier.NotifyNewHistoryEvent(event)
	err := s.ShardContext.NotifyNewHistoryEvent(event)
	return err
}

func validateActivityScheduleAttributes(attributes *workflow.ScheduleActivityTaskDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "ScheduleActivityTaskDecisionAttributes is not set on decision."}
	}

	if attributes.TaskList == nil || attributes.TaskList.Name == nil || *attributes.TaskList.Name == "" {
		return &workflow.BadRequestError{Message: "TaskList is not set on decision."}
	}

	if attributes.ActivityId == nil || *attributes.ActivityId == "" {
		return &workflow.BadRequestError{Message: "ActivityId is not set on decision."}
	}

	if attributes.ActivityType == nil || attributes.ActivityType.Name == nil || *attributes.ActivityType.Name == "" {
		return &workflow.BadRequestError{Message: "ActivityType is not set on decision."}
	}

	if attributes.StartToCloseTimeoutSeconds == nil || *attributes.StartToCloseTimeoutSeconds <= 0 {
		return &workflow.BadRequestError{Message: "A valid StartToCloseTimeoutSeconds is not set on decision."}
	}
	if attributes.ScheduleToStartTimeoutSeconds == nil || *attributes.ScheduleToStartTimeoutSeconds <= 0 {
		return &workflow.BadRequestError{Message: "A valid ScheduleToStartTimeoutSeconds is not set on decision."}
	}
	if attributes.ScheduleToCloseTimeoutSeconds == nil || *attributes.ScheduleToCloseTimeoutSeconds <= 0 {
		return &workflow.BadRequestError{Message: "A valid ScheduleToCloseTimeoutSeconds is not set on decision."}
	}
	if attributes.HeartbeatTimeoutSeconds == nil || *attributes.HeartbeatTimeoutSeconds < 0 {
		return &workflow.BadRequestError{Message: "Ac valid HeartbeatTimeoutSeconds is not set on decision."}
	}

	return nil
}

func validateTimerScheduleAttributes(attributes *workflow.StartTimerDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "StartTimerDecisionAttributes is not set on decision."}
	}
	if attributes.TimerId == nil || *attributes.TimerId == "" {
		return &workflow.BadRequestError{Message: "TimerId is not set on decision."}
	}
	if attributes.StartToFireTimeoutSeconds == nil || *attributes.StartToFireTimeoutSeconds <= 0 {
		return &workflow.BadRequestError{Message: "A valid StartToFireTimeoutSeconds is not set on decision."}
	}
	return nil
}

func validateActivityCancelAttributes(attributes *workflow.RequestCancelActivityTaskDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "RequestCancelActivityTaskDecisionAttributes is not set on decision."}
	}
	if attributes.ActivityId == nil || *attributes.ActivityId == "" {
		return &workflow.BadRequestError{Message: "ActivityId is not set on decision."}
	}
	return nil
}

func validateTimerCancelAttributes(attributes *workflow.CancelTimerDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "CancelTimerDecisionAttributes is not set on decision."}
	}
	if attributes.TimerId == nil || *attributes.TimerId == "" {
		return &workflow.BadRequestError{Message: "TimerId is not set on decision."}
	}
	return nil
}

func validateRecordMarkerAttributes(attributes *workflow.RecordMarkerDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "RecordMarkerDecisionAttributes is not set on decision."}
	}
	if attributes.MarkerName == nil || *attributes.MarkerName == "" {
		return &workflow.BadRequestError{Message: "MarkerName is not set on decision."}
	}
	return nil
}

func validateCompleteWorkflowExecutionAttributes(attributes *workflow.CompleteWorkflowExecutionDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "CompleteWorkflowExecutionDecisionAttributes is not set on decision."}
	}
	return nil
}

func validateFailWorkflowExecutionAttributes(attributes *workflow.FailWorkflowExecutionDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "FailWorkflowExecutionDecisionAttributes is not set on decision."}
	}
	if attributes.Reason == nil {
		return &workflow.BadRequestError{Message: "Reason is not set on decision."}
	}
	return nil
}

func validateCancelWorkflowExecutionAttributes(attributes *workflow.CancelWorkflowExecutionDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "CancelWorkflowExecutionDecisionAttributes is not set on decision."}
	}
	return nil
}

func validateCancelExternalWorkflowExecutionAttributes(attributes *workflow.RequestCancelExternalWorkflowExecutionDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "RequestCancelExternalWorkflowExecutionDecisionAttributes is not set on decision."}
	}
	if attributes.WorkflowId == nil {
		return &workflow.BadRequestError{Message: "WorkflowId is not set on decision."}
	}
	runID := attributes.GetRunId()
	if runID != "" && uuid.Parse(runID) == nil {
		return &workflow.BadRequestError{Message: "Invalid RunId set on decision."}
	}

	return nil
}

func validateSignalExternalWorkflowExecutionAttributes(attributes *workflow.SignalExternalWorkflowExecutionDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "SignalExternalWorkflowExecutionDecisionAttributes is not set on decision."}
	}
	if attributes.Execution == nil {
		return &workflow.BadRequestError{Message: "Execution is nil on decision."}
	}
	if attributes.Execution.WorkflowId == nil {
		return &workflow.BadRequestError{Message: "WorkflowId is not set on decision."}
	}
	runID := attributes.Execution.GetRunId()
	if runID != "" && uuid.Parse(runID) == nil {
		return &workflow.BadRequestError{Message: "Invalid RunId set on decision."}
	}
	if attributes.SignalName == nil {
		return &workflow.BadRequestError{Message: "SignalName is not set on decision."}
	}
	if attributes.Input == nil {
		return &workflow.BadRequestError{Message: "Input is not set on decision."}
	}

	return nil
}

func validateContinueAsNewWorkflowExecutionAttributes(executionInfo *persistence.WorkflowExecutionInfo,
	attributes *workflow.ContinueAsNewWorkflowExecutionDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "ContinueAsNewWorkflowExecutionDecisionAttributes is not set on decision."}
	}

	// Inherit workflow type from previous execution if not provided on decision
	if attributes.WorkflowType == nil || attributes.WorkflowType.GetName() == "" {
		attributes.WorkflowType = &workflow.WorkflowType{Name: common.StringPtr(executionInfo.WorkflowTypeName)}
	}

	// Inherit Tasklist from previous execution if not provided on decision
	if attributes.TaskList == nil || attributes.TaskList.GetName() == "" {
		attributes.TaskList = &workflow.TaskList{Name: common.StringPtr(executionInfo.TaskList)}
	}

	// Inherit workflow timeout from previous execution if not provided on decision
	if attributes.GetExecutionStartToCloseTimeoutSeconds() <= 0 {
		attributes.ExecutionStartToCloseTimeoutSeconds = common.Int32Ptr(executionInfo.WorkflowTimeout)
	}

	// Inherit decision task timeout from previous execution if not provided on decision
	if attributes.GetTaskStartToCloseTimeoutSeconds() <= 0 {
		attributes.TaskStartToCloseTimeoutSeconds = common.Int32Ptr(executionInfo.DecisionTimeoutValue)
	}

	return nil
}

func validateStartChildExecutionAttributes(parentInfo *persistence.WorkflowExecutionInfo,
	attributes *workflow.StartChildWorkflowExecutionDecisionAttributes) error {
	if attributes == nil {
		return &workflow.BadRequestError{Message: "StartChildWorkflowExecutionDecisionAttributes is not set on decision."}
	}

	if attributes.GetWorkflowId() == "" {
		return &workflow.BadRequestError{Message: "Required field WorkflowID is not set on decision."}
	}

	if attributes.WorkflowType == nil || attributes.WorkflowType.GetName() == "" {
		return &workflow.BadRequestError{Message: "Required field WorkflowType is not set on decision."}
	}

	if attributes.ChildPolicy == nil {
		return &workflow.BadRequestError{Message: "Required field ChildPolicy is not set on decision."}
	}

	// Inherit tasklist from parent workflow execution if not provided on decision
	if attributes.TaskList == nil || attributes.TaskList.GetName() == "" {
		attributes.TaskList = &workflow.TaskList{Name: common.StringPtr(parentInfo.TaskList)}
	}

	// Inherit workflow timeout from parent workflow execution if not provided on decision
	if attributes.GetExecutionStartToCloseTimeoutSeconds() <= 0 {
		attributes.ExecutionStartToCloseTimeoutSeconds = common.Int32Ptr(parentInfo.WorkflowTimeout)
	}

	// Inherit decision task timeout from parent workflow execution if not provided on decision
	if attributes.GetTaskStartToCloseTimeoutSeconds() <= 0 {
		attributes.TaskStartToCloseTimeoutSeconds = common.Int32Ptr(parentInfo.DecisionTimeoutValue)
	}

	return nil
}

func validateStartWorkflowExecutionRequest(request *workflow.StartWorkflowExecutionRequest) error {
	if request.ExecutionStartToCloseTimeoutSeconds == nil || request.GetExecutionStartToCloseTimeoutSeconds() <= 0 {
		return &workflow.BadRequestError{Message: "Missing or invalid ExecutionStartToCloseTimeoutSeconds."}
	}
	if request.TaskStartToCloseTimeoutSeconds == nil || request.GetTaskStartToCloseTimeoutSeconds() <= 0 {
		return &workflow.BadRequestError{Message: "Missing or invalid TaskStartToCloseTimeoutSeconds."}
	}
	if request.TaskList == nil || request.TaskList.Name == nil || request.TaskList.GetName() == "" {
		return &workflow.BadRequestError{Message: "Missing Tasklist."}
	}
	return nil
}

func getDomainUUID(domainUUID *string) (string, error) {
	if domainUUID == nil {
		return "", &workflow.BadRequestError{Message: "Missing domain UUID."}
	}
	return *domainUUID, nil
}

func getScheduleID(activityID string, msBuilder *mutableStateBuilder) (int64, error) {
	if activityID == "" {
		return 0, &workflow.BadRequestError{Message: "Neither ActivityID nor ScheduleID is provided"}
	}
	scheduleID, ok := msBuilder.GetScheduleIDByActivityID(activityID)
	if !ok {
		return 0, &workflow.BadRequestError{Message: fmt.Sprintf("No such activityID: %s\n", activityID)}
	}
	return scheduleID, nil
}

func getSignalRequest(request *workflow.SignalWithStartWorkflowExecutionRequest) *workflow.SignalWorkflowExecutionRequest {
	req := &workflow.SignalWorkflowExecutionRequest{
		Domain: request.Domain,
		WorkflowExecution: &workflow.WorkflowExecution{
			WorkflowId: request.WorkflowId,
		},
		SignalName: request.SignalName,
		Input:      request.SignalInput,
		RequestId:  request.RequestId,
		Identity:   request.Identity,
		Control:    request.Control,
	}
	return req
}

func getStartRequest(domainID string,
	request *workflow.SignalWithStartWorkflowExecutionRequest) *h.StartWorkflowExecutionRequest {
	policy := workflow.WorkflowIdReusePolicyAllowDuplicate
	req := &workflow.StartWorkflowExecutionRequest{
		Domain:       request.Domain,
		WorkflowId:   request.WorkflowId,
		WorkflowType: request.WorkflowType,
		TaskList:     request.TaskList,
		Input:        request.Input,
		ExecutionStartToCloseTimeoutSeconds: request.ExecutionStartToCloseTimeoutSeconds,
		TaskStartToCloseTimeoutSeconds:      request.TaskStartToCloseTimeoutSeconds,
		Identity:                            request.Identity,
		RequestId:                           request.RequestId,
		WorkflowIdReusePolicy:               &policy,
	}

	startRequest := &h.StartWorkflowExecutionRequest{
		DomainUUID:   common.StringPtr(domainID),
		StartRequest: req,
	}
	return startRequest
}
