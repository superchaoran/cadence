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
	"sync/atomic"
	"time"

	"github.com/uber-common/bark"

	"github.com/uber/cadence/client/history"
	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/persistence"
)

type (
	transferTaskFilter func(timer *persistence.TransferTaskInfo) (bool, error)

	transferQueueProcessorImpl struct {
		isGlobalDomainEnabled bool
		currentClusterName    string
		shard                 ShardContext
		config                *Config
		historyService        *historyEngineImpl
		visibilityMgr         persistence.VisibilityManager
		matchingClient        matching.Client
		historyClient         history.Client
		logger                bark.Logger
		isStarted             int32
		isStopped             int32
		shutdownChan          chan struct{}
		activeTaskProcessor   *transferQueueActiveProcessorImpl
		standbyTaskProcessors map[string]*transferQueueStandbyProcessorImpl
	}
)

func newTransferQueueProcessor(shard ShardContext, historyService *historyEngineImpl, visibilityMgr persistence.VisibilityManager,
	matchingClient matching.Client, historyClient history.Client, logger bark.Logger) *transferQueueProcessorImpl {
	logger = logger.WithFields(bark.Fields{
		logging.TagWorkflowComponent: logging.TagValueTransferQueueComponent,
	})
	currentClusterName := shard.GetService().GetClusterMetadata().GetCurrentClusterName()
	standbyTaskProcessors := make(map[string]*transferQueueStandbyProcessorImpl)
	for clusterName := range shard.GetService().GetClusterMetadata().GetAllClusterFailoverVersions() {
		if clusterName != currentClusterName {
			standbyTaskProcessors[clusterName] = newTransferQueueStandbyProcessor(clusterName, shard, historyService, visibilityMgr, logger)
		}
	}

	return &transferQueueProcessorImpl{
		isGlobalDomainEnabled: shard.GetService().GetClusterMetadata().IsGlobalDomainEnabled(),
		currentClusterName:    currentClusterName,
		shard:                 shard,
		config:                shard.GetConfig(),
		historyService:        historyService,
		visibilityMgr:         visibilityMgr,
		matchingClient:        matchingClient,
		historyClient:         historyClient,
		logger:                logger,
		shutdownChan:          make(chan struct{}),
		activeTaskProcessor:   newTransferQueueActiveProcessor(shard, historyService, visibilityMgr, matchingClient, historyClient, logger),
		standbyTaskProcessors: standbyTaskProcessors,
	}
}

func (t *transferQueueProcessorImpl) Start() {
	if !atomic.CompareAndSwapInt32(&t.isStarted, 0, 1) {
		return
	}
	t.activeTaskProcessor.Start()
	if t.isGlobalDomainEnabled {
		for _, standbyTaskProcessor := range t.standbyTaskProcessors {
			standbyTaskProcessor.Start()
		}
	}

	go t.completeTransferLoop()
}

func (t *transferQueueProcessorImpl) Stop() {
	if !atomic.CompareAndSwapInt32(&t.isStopped, 0, 1) {
		return
	}
	t.activeTaskProcessor.Stop()
	if t.isGlobalDomainEnabled {
		for _, standbyTaskProcessor := range t.standbyTaskProcessors {
			standbyTaskProcessor.Stop()
		}
	}
	close(t.shutdownChan)
}

// NotifyNewTask - Notify the processor about the new active / standby transfer task arrival.
// This should be called each time new transfer task arrives, otherwise tasks maybe delayed.
func (t *transferQueueProcessorImpl) NotifyNewTask(clusterName string, currentTime time.Time) {
	if clusterName == t.currentClusterName {
		// we will ignore the current time passed in, since the active processor process task immediately
		t.activeTaskProcessor.notifyNewTask()
		return
	}

	standbyTaskProcessor, ok := t.standbyTaskProcessors[clusterName]
	if !ok {
		panic(fmt.Sprintf("Cannot find transfer processor for %s.", clusterName))
	}
	currentClusterTime := t.shard.GetCurrentTime(t.currentClusterName)
	if currentClusterTime.Sub(currentTime) > t.config.TransferProcessorStandbyTaskDelay {
		standbyTaskProcessor.notifyNewTask()
	}
}

func (t *transferQueueProcessorImpl) FailoverDomain(domainID string, standbyClusterName string) {
	// we should consider make the failover idempotent
	failoverTaskProcessor := newTransferQueueFailoverProcessor(t.shard, t.historyService, t.visibilityMgr, t.matchingClient, t.historyClient, domainID, standbyClusterName, t.logger)
	failoverTaskProcessor.Start()
	failoverTaskProcessor.notifyNewTask()
}

func (t *transferQueueProcessorImpl) completeTransferLoop() {
	timer := time.NewTimer(t.config.TransferProcessorCompleteTransferInterval)
	defer timer.Stop()

	for {
		select {
		case <-t.shutdownChan:
			// before shutdown, make sure the ack level is up to date
			t.completeTransfer()
			return
		case <-timer.C:
		CompleteLoop:
			for attempt := 0; attempt < t.config.TransferProcessorCompleteTransferFailureRetryCount; attempt++ {
				err := t.completeTransfer()
				if err != nil {
					t.logger.Infof("Failed to complete transfer task: %v.", err)
					backoff := time.Duration(attempt * 100)
					time.Sleep(backoff * time.Millisecond)
				} else {
					break CompleteLoop
				}
			}
			timer.Reset(t.config.TransferProcessorCompleteTransferInterval)
		}
	}
}

func (t *transferQueueProcessorImpl) completeTransfer() error {
	lowerAckLevel := t.shard.GetTransferAckLevel()
	upperAckLevel := t.activeTaskProcessor.queueAckMgr.getAckLevel()

	if t.isGlobalDomainEnabled {
		for _, standbyTaskProcessor := range t.standbyTaskProcessors {
			ackLevel := standbyTaskProcessor.queueAckMgr.getAckLevel()
			if upperAckLevel > ackLevel {
				upperAckLevel = ackLevel
			}
		}
	}

	if lowerAckLevel >= upperAckLevel {
		return nil
	}

	executionMgr := t.shard.GetExecutionManager()
	maxLevel := upperAckLevel + 1
	batchSize := t.config.TransferTaskBatchSize

LoadCompleteLoop:
	for {
		request := &persistence.GetTransferTasksRequest{
			ReadLevel:    lowerAckLevel,
			MaxReadLevel: maxLevel,
			BatchSize:    batchSize,
		}
		response, err := executionMgr.GetTransferTasks(request)
		if err != nil {
			return err
		}

		more := len(response.Tasks) >= batchSize
		for _, task := range response.Tasks {
			if upperAckLevel < task.GetTaskID() {
				break LoadCompleteLoop
			}
			lowerAckLevel = task.GetTaskID()
			if err := executionMgr.CompleteTransferTask(&persistence.CompleteTransferTaskRequest{TaskID: task.GetTaskID()}); err != nil {
				t.logger.Warnf("Timer queue ack manager unable to complete timer task: %v; %v", task, err)
			}
		}

		if !more {
			break LoadCompleteLoop
		}
	}
	t.shard.UpdateTransferAckLevel(upperAckLevel)
	return nil
}
