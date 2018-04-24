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
	"time"

	h "github.com/uber/cadence/.gen/go/history"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/persistence"
)

type (
	workflowIdentifier struct {
		domainID   string
		workflowID string
		runID      string
	}

	historyEventNotification struct {
		workflowIdentifier
		lastFirstEventID  int64
		nextEventID       int64
		isWorkflowRunning bool
		timestamp         time.Time
	}

	// Engine represents an interface for managing workflow execution history.
	Engine interface {
		common.Daemon
		// TODO: Convert workflow.WorkflowExecution to pointer all over the place
		StartWorkflowExecution(request *h.StartWorkflowExecutionRequest) (*workflow.StartWorkflowExecutionResponse,
			error)
		GetMutableState(ctx context.Context, request *h.GetMutableStateRequest) (*h.GetMutableStateResponse, error)
		ResetStickyTaskList(resetRequest *h.ResetStickyTaskListRequest) (*h.ResetStickyTaskListResponse, error)
		DescribeWorkflowExecution(
			request *h.DescribeWorkflowExecutionRequest) (*workflow.DescribeWorkflowExecutionResponse, error)
		RecordDecisionTaskStarted(request *h.RecordDecisionTaskStartedRequest) (*h.RecordDecisionTaskStartedResponse, error)
		RecordActivityTaskStarted(request *h.RecordActivityTaskStartedRequest) (*h.RecordActivityTaskStartedResponse, error)
		RespondDecisionTaskCompleted(ctx context.Context, request *h.RespondDecisionTaskCompletedRequest) error
		RespondDecisionTaskFailed(request *h.RespondDecisionTaskFailedRequest) error
		RespondActivityTaskCompleted(request *h.RespondActivityTaskCompletedRequest) error
		RespondActivityTaskFailed(request *h.RespondActivityTaskFailedRequest) error
		RespondActivityTaskCanceled(request *h.RespondActivityTaskCanceledRequest) error
		RecordActivityTaskHeartbeat(request *h.RecordActivityTaskHeartbeatRequest) (*workflow.RecordActivityTaskHeartbeatResponse, error)
		RequestCancelWorkflowExecution(request *h.RequestCancelWorkflowExecutionRequest) error
		SignalWorkflowExecution(request *h.SignalWorkflowExecutionRequest) error
		SignalWithStartWorkflowExecution(request *h.SignalWithStartWorkflowExecutionRequest) (
			*workflow.StartWorkflowExecutionResponse, error)
		RemoveSignalMutableState(request *h.RemoveSignalMutableStateRequest) error
		TerminateWorkflowExecution(request *h.TerminateWorkflowExecutionRequest) error
		ScheduleDecisionTask(request *h.ScheduleDecisionTaskRequest) error
		RecordChildExecutionCompleted(request *h.RecordChildExecutionCompletedRequest) error
		ReplicateEvents(request *h.ReplicateEventsRequest) error
	}

	// EngineFactory is used to create an instance of sharded history engine
	EngineFactory interface {
		CreateEngine(context ShardContext) Engine
	}

	historyEventSerializer interface {
		Serialize(event *workflow.HistoryEvent) ([]byte, error)
		Deserialize(data []byte) (*workflow.HistoryEvent, error)
	}

	queueProcessor interface {
		common.Daemon
		notifyNewTask()
	}

	queueAckMgr interface {
		getFinishedChan() <-chan struct{}
		readQueueTasks() ([]queueTaskInfo, bool, error)
		completeTask(taskID int64)
		getAckLevel() int64
		updateAckLevel()
	}

	queueTaskInfo interface {
		GetTaskID() int64
		GetTaskType() int
	}

	processor interface {
		process(task queueTaskInfo) error
		readTasks(readLevel int64) ([]queueTaskInfo, bool, error)
		completeTask(taskID int64) error
		updateAckLevel(taskID int64) error
	}

	transferQueueProcessor interface {
		common.Daemon
		NotifyNewTask(clusterName string, currentTime time.Time)
	}

	// TODO the timer quque processor and the one below, timer processor
	// in combination are confusing, we should consider a better naming
	// convention, or at least come with a better name for this case.
	timerQueueProcessor interface {
		common.Daemon
		NotifyNewTimers(clusterName string, timerTask []persistence.Task)
		SetCurrentTime(clusterName string, currentTime time.Time)
	}

	timerProcessor interface {
		notifyNewTimers(timerTask []persistence.Task)
		process(task *persistence.TimerTaskInfo) error
		getTimerGate() TimerGate
	}

	timerQueueAckMgr interface {
		getFinishedChan() <-chan struct{}
		readTimerTasks() ([]*persistence.TimerTaskInfo, *persistence.TimerTaskInfo, bool, error)
		completeTimerTask(timerTask *persistence.TimerTaskInfo)
		getAckLevel() TimerSequenceID
		updateAckLevel()
	}

	historyEventNotifier interface {
		common.Daemon
		NotifyNewHistoryEvent(event *historyEventNotification)
		WatchHistoryEvent(identifier *workflowIdentifier) (string, chan *historyEventNotification, error)
		UnwatchHistoryEvent(identifier *workflowIdentifier, subscriberID string) error
	}
)
