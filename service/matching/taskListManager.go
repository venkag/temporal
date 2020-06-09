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

package matching

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	commonpb "go.temporal.io/temporal-proto/common"
	tasklistpb "go.temporal.io/temporal-proto/tasklist"

	commongenpb "github.com/temporalio/temporal/.gen/proto/common"
	"github.com/temporalio/temporal/.gen/proto/matchingservice"

	"github.com/temporalio/temporal/.gen/proto/persistenceblobs"
	"github.com/temporalio/temporal/common"
	"github.com/temporalio/temporal/common/backoff"
	"github.com/temporalio/temporal/common/cache"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/log/tag"
	"github.com/temporalio/temporal/common/metrics"
	"github.com/temporalio/temporal/common/persistence"
)

const (
	// Time budget for empty task to propagate through the function stack and be returned to
	// pollForActivityTask or pollForDecisionTask handler.
	returnEmptyTaskTimeBudget = time.Second

	// Fake Task ID to wrap a task for syncmatch
	syncMatchTaskId = -137
)

type (
	addTaskParams struct {
		execution     *commonpb.WorkflowExecution
		taskInfo      *persistenceblobs.TaskInfo
		source        commongenpb.TaskSource
		forwardedFrom string
	}

	taskListManager interface {
		Start() error
		Stop()
		// AddTask adds a task to the task list. This method will first attempt a synchronous
		// match with a poller. When that fails, task will be written to database and later
		// asynchronously matched with a poller
		AddTask(ctx context.Context, params addTaskParams) (syncMatch bool, err error)
		// GetTask blocks waiting for a task Returns error when context deadline is exceeded
		// maxDispatchPerSecond is the max rate at which tasks are allowed to be dispatched
		// from this task list to pollers
		GetTask(ctx context.Context, maxDispatchPerSecond *float64) (*internalTask, error)
		// DispatchTask dispatches a task to a poller. When there are no pollers to pick
		// up the task, this method will return error. Task will not be persisted to db
		DispatchTask(ctx context.Context, task *internalTask) error
		// DispatchQueryTask will dispatch query to local or remote poller. If forwarded then result or error is returned,
		// if dispatched to local poller then nil and nil is returned.
		DispatchQueryTask(ctx context.Context, taskID string, request *matchingservice.QueryWorkflowRequest) (*matchingservice.QueryWorkflowResponse, error)
		CancelPoller(pollerID string)
		GetAllPollerInfo() []*tasklistpb.PollerInfo
		// DescribeTaskList returns information about the target task list
		DescribeTaskList(includeTaskListStatus bool) *matchingservice.DescribeTaskListResponse
		String() string
	}

	// Single task list in memory state
	taskListManagerImpl struct {
		taskListID       *taskListID
		taskListKind     tasklistpb.TaskListKind // sticky taskList has different process in persistence
		config           *taskListConfig
		db               *taskListDB
		engine           *matchingEngineImpl
		taskWriter       *taskWriter
		taskReader       *taskReader // reads tasks from db and async matches it with poller
		taskGC           *taskGC
		taskAckManager   ackManager   // tracks ackLevel for delivered messages
		matcher          *TaskMatcher // for matching a task producer with a poller
		namespaceCache   cache.NamespaceCache
		logger           log.Logger
		metricsClient    metrics.Client
		namespaceValue   atomic.Value
		metricScopeValue atomic.Value // namespace/tasklist tagged metric scope
		// pollerHistory stores poller which poll from this tasklist in last few minutes
		pollerHistory *pollerHistory
		// outstandingPollsMap is needed to keep track of all outstanding pollers for a
		// particular tasklist.  PollerID generated by frontend is used as the key and
		// CancelFunc is the value.  This is used to cancel the context to unblock any
		// outstanding poller when the frontend detects client connection is closed to
		// prevent tasks being dispatched to zombie pollers.
		outstandingPollsLock sync.Mutex
		outstandingPollsMap  map[string]context.CancelFunc

		shutdownCh chan struct{}  // Delivers stop to the pump that populates taskBuffer
		startWG    sync.WaitGroup // ensures that background processes do not start until setup is ready
		stopped    int32
	}
)

const (
	// maxSyncMatchWaitTime is the max amount of time that we are willing to wait for a sync match to happen
	maxSyncMatchWaitTime = 200 * time.Millisecond
)

var _ taskListManager = (*taskListManagerImpl)(nil)

var errRemoteSyncMatchFailed = errors.New("remote sync match failed")

func newTaskListManager(
	e *matchingEngineImpl,
	taskList *taskListID,
	taskListKind tasklistpb.TaskListKind,
	config *Config,
) (taskListManager, error) {

	taskListConfig, err := newTaskListConfig(taskList, config, e.namespaceCache)
	if err != nil {
		return nil, err
	}

	db := newTaskListDB(e.taskManager, taskList.namespaceID, taskList.name, taskList.taskType, taskListKind, e.logger)

	tlMgr := &taskListManagerImpl{
		namespaceCache: e.namespaceCache,
		metricsClient:  e.metricsClient,
		engine:         e,
		shutdownCh:     make(chan struct{}),
		taskListID:     taskList,
		taskListKind:   taskListKind,
		logger: e.logger.WithTags(tag.WorkflowTaskListName(taskList.name),
			tag.WorkflowTaskListType(taskList.taskType)),
		db:                  db,
		taskAckManager:      newAckManager(e.logger),
		taskGC:              newTaskGC(db, taskListConfig),
		config:              taskListConfig,
		pollerHistory:       newPollerHistory(),
		outstandingPollsMap: make(map[string]context.CancelFunc),
	}

	tlMgr.namespaceValue.Store("")
	if tlMgr.metricScope() == nil { // namespace name lookup failed
		// metric scope to use when namespace lookup fails
		tlMgr.metricScopeValue.Store(newPerTaskListScope(
			"",
			tlMgr.taskListID.name,
			tlMgr.taskListKind,
			e.metricsClient,
			metrics.MatchingTaskListMgrScope,
		))
	}

	tlMgr.taskWriter = newTaskWriter(tlMgr)
	tlMgr.taskReader = newTaskReader(tlMgr)
	var fwdr *Forwarder
	if tlMgr.isFowardingAllowed(taskList, taskListKind) {
		fwdr = newForwarder(&taskListConfig.forwarderConfig, taskList, taskListKind, e.matchingClient)
	}
	tlMgr.matcher = newTaskMatcher(taskListConfig, fwdr, tlMgr.metricScope)
	tlMgr.startWG.Add(1)
	return tlMgr, nil
}

// Starts reading pump for the given task list.
// The pump fills up taskBuffer from persistence.
func (c *taskListManagerImpl) Start() error {
	defer c.startWG.Done()

	// Make sure to grab the range first before starting task writer, as it needs the range to initialize maxReadLevel
	state, err := c.renewLeaseWithRetry()
	if err != nil {
		c.Stop()
		return err
	}

	c.taskAckManager.setAckLevel(state.ackLevel)
	c.taskWriter.Start(c.rangeIDToTaskIDBlock(state.rangeID))
	c.taskReader.Start()

	return nil
}

// Stops pump that fills up taskBuffer from persistence.
func (c *taskListManagerImpl) Stop() {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return
	}
	close(c.shutdownCh)
	c.taskWriter.Stop()
	c.taskReader.Stop()
	c.engine.removeTaskListManager(c.taskListID)
	c.engine.removeTaskListManager(c.taskListID)
	c.logger.Info("", tag.LifeCycleStopped)
}

// AddTask adds a task to the task list. This method will first attempt a synchronous
// match with a poller. When there are no pollers or if ratelimit is exceeded, task will
// be written to database and later asynchronously matched with a poller
func (c *taskListManagerImpl) AddTask(ctx context.Context, params addTaskParams) (bool, error) {
	c.startWG.Wait()
	var syncMatch bool
	_, err := c.executeWithRetry(func() (interface{}, error) {
		td := params.taskInfo

		namespaceEntry, err := c.namespaceCache.GetNamespaceByID(td.GetNamespaceId())
		if err != nil {
			return nil, err
		}

		if namespaceEntry.GetNamespaceNotActiveErr() != nil {
			r, err := c.taskWriter.appendTask(params.execution, td)
			syncMatch = false
			return r, err
		}

		syncMatch, err = c.trySyncMatch(ctx, params)
		if syncMatch {
			return &persistence.CreateTasksResponse{}, err
		}

		if params.forwardedFrom != "" {
			// forwarded from child partition - only do sync match
			// child partition will persist the task when sync match fails
			return &persistence.CreateTasksResponse{}, errRemoteSyncMatchFailed
		}

		return c.taskWriter.appendTask(params.execution, params.taskInfo)
	})
	if err == nil {
		c.taskReader.Signal()
	}
	return syncMatch, err
}

// DispatchTask dispatches a task to a poller. When there are no pollers to pick
// up the task or if rate limit is exceeded, this method will return error. Task
// *will not* be persisted to db
func (c *taskListManagerImpl) DispatchTask(ctx context.Context, task *internalTask) error {
	return c.matcher.MustOffer(ctx, task)
}

// DispatchQueryTask will dispatch query to local or remote poller. If forwarded then result or error is returned,
// if dispatched to local poller then nil and nil is returned.
func (c *taskListManagerImpl) DispatchQueryTask(
	ctx context.Context,
	taskID string,
	request *matchingservice.QueryWorkflowRequest,
) (*matchingservice.QueryWorkflowResponse, error) {
	c.startWG.Wait()
	task := newInternalQueryTask(taskID, request)
	return c.matcher.OfferQuery(ctx, task)
}

// GetTask blocks waiting for a task.
// Returns error when context deadline is exceeded
// maxDispatchPerSecond is the max rate at which tasks are allowed
// to be dispatched from this task list to pollers
func (c *taskListManagerImpl) GetTask(
	ctx context.Context,
	maxDispatchPerSecond *float64,
) (*internalTask, error) {
	task, err := c.getTask(ctx, maxDispatchPerSecond)
	if err != nil {
		return nil, err
	}
	task.namespace = c.namespace()
	task.backlogCountHint = c.taskAckManager.getBacklogCountHint()
	return task, nil
}

func (c *taskListManagerImpl) getTask(ctx context.Context, maxDispatchPerSecond *float64) (*internalTask, error) {
	// We need to set a shorter timeout than the original ctx; otherwise, by the time ctx deadline is
	// reached, instead of emptyTask, context timeout error is returned to the frontend by the rpc stack,
	// which counts against our SLO. By shortening the timeout by a very small amount, the emptyTask can be
	// returned to the handler before a context timeout error is generated.
	childCtx, cancel := c.newChildContext(ctx, c.config.LongPollExpirationInterval(), returnEmptyTaskTimeBudget)
	defer cancel()

	pollerID, ok := ctx.Value(pollerIDKey).(string)
	if ok && pollerID != "" {
		// Found pollerID on context, add it to the map to allow it to be canceled in
		// response to CancelPoller call
		c.outstandingPollsLock.Lock()
		c.outstandingPollsMap[pollerID] = cancel
		c.outstandingPollsLock.Unlock()
		defer func() {
			c.outstandingPollsLock.Lock()
			delete(c.outstandingPollsMap, pollerID)
			c.outstandingPollsLock.Unlock()
		}()
	}

	identity, ok := ctx.Value(identityKey).(string)
	if ok && identity != "" {
		c.pollerHistory.updatePollerInfo(pollerIdentity(identity), maxDispatchPerSecond)
	}

	namespaceEntry, err := c.namespaceCache.GetNamespaceByID(c.taskListID.namespaceID)
	if err != nil {
		return nil, err
	}

	// the desired global rate limit for the task list comes from the
	// poller, which lives inside the client side worker. There is
	// one rateLimiter for this entire task list and as we get polls,
	// we update the ratelimiter rps if it has changed from the last
	// value. Last poller wins if different pollers provide different values
	c.matcher.UpdateRatelimit(maxDispatchPerSecond)

	if namespaceEntry.GetNamespaceNotActiveErr() != nil {
		return c.matcher.PollForQuery(childCtx)
	}

	return c.matcher.Poll(childCtx)
}

// GetAllPollerInfo returns all pollers that polled from this tasklist in last few minutes
func (c *taskListManagerImpl) GetAllPollerInfo() []*tasklistpb.PollerInfo {
	return c.pollerHistory.getAllPollerInfo()
}

func (c *taskListManagerImpl) CancelPoller(pollerID string) {
	c.outstandingPollsLock.Lock()
	cancel, ok := c.outstandingPollsMap[pollerID]
	c.outstandingPollsLock.Unlock()

	if ok && cancel != nil {
		cancel()
	}
}

// DescribeTaskList returns information about the target tasklist, right now this API returns the
// pollers which polled this tasklist in last few minutes and status of tasklist's ackManager
// (readLevel, ackLevel, backlogCountHint and taskIDBlock).
func (c *taskListManagerImpl) DescribeTaskList(includeTaskListStatus bool) *matchingservice.DescribeTaskListResponse {
	response := &matchingservice.DescribeTaskListResponse{Pollers: c.GetAllPollerInfo()}
	if !includeTaskListStatus {
		return response
	}

	taskIDBlock := c.rangeIDToTaskIDBlock(c.db.RangeID())
	response.TaskListStatus = &tasklistpb.TaskListStatus{
		ReadLevel:        c.taskAckManager.getReadLevel(),
		AckLevel:         c.taskAckManager.getAckLevel(),
		BacklogCountHint: c.taskAckManager.getBacklogCountHint(),
		RatePerSecond:    c.matcher.Rate(),
		TaskIdBlock: &tasklistpb.TaskIdBlock{
			StartId: taskIDBlock.start,
			EndId:   taskIDBlock.end,
		},
	}

	return response
}

func (c *taskListManagerImpl) String() string {
	buf := new(bytes.Buffer)
	if c.taskListID.taskType == tasklistpb.TASK_LIST_TYPE_ACTIVITY {
		buf.WriteString("Activity")
	} else {
		buf.WriteString("Decision")
	}
	rangeID := c.db.RangeID()
	_, _ = fmt.Fprintf(buf, " task list %v\n", c.taskListID.name)
	_, _ = fmt.Fprintf(buf, "RangeID=%v\n", rangeID)
	_, _ = fmt.Fprintf(buf, "TaskIDBlock=%+v\n", c.rangeIDToTaskIDBlock(rangeID))
	_, _ = fmt.Fprintf(buf, "AckLevel=%v\n", c.taskAckManager.ackLevel)
	_, _ = fmt.Fprintf(buf, "MaxReadLevel=%v\n", c.taskAckManager.getReadLevel())

	return buf.String()
}

// completeTask marks a task as processed. Only tasks created by taskReader (i.e. backlog from db) reach
// here. As part of completion:
//   - task is deleted from the database when err is nil
//   - new task is created and current task is deleted when err is not nil
func (c *taskListManagerImpl) completeTask(task *persistenceblobs.AllocatedTaskInfo, err error) {
	if err != nil {
		// failed to start the task.
		// We cannot just remove it from persistence because then it will be lost.
		// We handle this by writing the task back to persistence with a higher taskID.
		// This will allow subsequent tasks to make progress, and hopefully by the time this task is picked-up
		// again the underlying reason for failing to start will be resolved.
		// Note that RecordTaskStarted only fails after retrying for a long time, so a single task will not be
		// re-written to persistence frequently.
		_, err = c.executeWithRetry(func() (interface{}, error) {
			wf := &commonpb.WorkflowExecution{WorkflowId: task.Data.GetWorkflowId(), RunId: task.Data.GetRunId()}
			return c.taskWriter.appendTask(wf, task.Data)
		})

		if err != nil {
			// OK, we also failed to write to persistence.
			// This should only happen in very extreme cases where persistence is completely down.
			// We still can't lose the old task so we just unload the entire task list
			c.logger.Error("Persistent store operation failure",
				tag.StoreOperationStopTaskList,
				tag.Error(err),
				tag.WorkflowTaskListName(c.taskListID.name),
				tag.WorkflowTaskListType(c.taskListID.taskType))
			c.Stop()
			return
		}
		c.taskReader.Signal()
	}

	ackLevel := c.taskAckManager.completeTask(task.GetTaskId())
	c.taskGC.Run(ackLevel)
}

func (c *taskListManagerImpl) renewLeaseWithRetry() (taskListState, error) {
	var newState taskListState
	op := func() (err error) {
		newState, err = c.db.RenewLease()
		return
	}
	c.metricScope().IncCounter(metrics.LeaseRequestPerTaskListCounter)
	err := backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
	if err != nil {
		c.metricScope().IncCounter(metrics.LeaseFailurePerTaskListCounter)
		c.engine.unloadTaskList(c.taskListID)
		return newState, err
	}
	return newState, nil
}

func (c *taskListManagerImpl) rangeIDToTaskIDBlock(rangeID int64) taskIDBlock {
	return taskIDBlock{
		start: (rangeID-1)*c.config.RangeSize + 1,
		end:   rangeID * c.config.RangeSize,
	}
}

func (c *taskListManagerImpl) allocTaskIDBlock(prevBlockEnd int64) (taskIDBlock, error) {
	currBlock := c.rangeIDToTaskIDBlock(c.db.RangeID())
	if currBlock.end != prevBlockEnd {
		return taskIDBlock{},
			fmt.Errorf("allocTaskIDBlock: invalid state: prevBlockEnd:%v != currTaskIDBlock:%+v", prevBlockEnd, currBlock)
	}
	state, err := c.renewLeaseWithRetry()
	if err != nil {
		return taskIDBlock{}, err
	}
	return c.rangeIDToTaskIDBlock(state.rangeID), nil
}

// Retry operation on transient error. On rangeID update by another process calls c.Stop().
func (c *taskListManagerImpl) executeWithRetry(
	operation func() (interface{}, error)) (result interface{}, err error) {

	op := func() error {
		result, err = operation()
		return err
	}

	var retryCount int64
	err = backoff.Retry(op, persistenceOperationRetryPolicy, func(err error) bool {
		c.logger.Debug("Retry executeWithRetry as task list range has changed", tag.AttemptCount(retryCount), tag.Error(err))
		if _, ok := err.(*persistence.ConditionFailedError); ok {
			return false
		}
		return common.IsPersistenceTransientError(err)
	})

	if _, ok := err.(*persistence.ConditionFailedError); ok {
		c.metricScope().IncCounter(metrics.ConditionFailedErrorPerTaskListCounter)
		c.logger.Debug("Stopping task list due to persistence condition failure", tag.Error(err))
		c.Stop()
	}
	return
}

func (c *taskListManagerImpl) trySyncMatch(ctx context.Context, params addTaskParams) (bool, error) {
	childCtx, cancel := c.newChildContext(ctx, maxSyncMatchWaitTime, time.Second)

	// Mocking out TaskId for syncmatch as it hasn't been allocated yet
	fakeTaskIdWrapper := &persistenceblobs.AllocatedTaskInfo{
		Data:   params.taskInfo,
		TaskId: syncMatchTaskId,
	}

	task := newInternalTask(fakeTaskIdWrapper, c.completeTask, params.source, params.forwardedFrom, true)
	matched, err := c.matcher.Offer(childCtx, task)
	cancel()
	return matched, err
}

// newChildContext creates a child context with desired timeout.
// if tailroom is non-zero, then child context timeout will be
// the minOf(parentCtx.Deadline()-tailroom, timeout). Use this
// method to create child context when childContext cannot use
// all of parent's deadline but instead there is a need to leave
// some time for parent to do some post-work
func (c *taskListManagerImpl) newChildContext(
	parent context.Context,
	timeout time.Duration,
	tailroom time.Duration,
) (context.Context, context.CancelFunc) {
	select {
	case <-parent.Done():
		return parent, func() {}
	default:
	}
	deadline, ok := parent.Deadline()
	if !ok {
		return context.WithTimeout(parent, timeout)
	}
	remaining := deadline.Sub(time.Now()) - tailroom
	if remaining < timeout {
		timeout = time.Duration(common.MaxInt64(0, int64(remaining)))
	}
	return context.WithTimeout(parent, timeout)
}

func (c *taskListManagerImpl) isFowardingAllowed(taskList *taskListID, kind tasklistpb.TaskListKind) bool {
	return !taskList.IsRoot() && kind != tasklistpb.TASK_LIST_KIND_STICKY
}

func (c *taskListManagerImpl) metricScope() metrics.Scope {
	c.tryInitNamespaceAndScope()
	return c.metricScopeValue.Load().(metrics.Scope)
}

func (c *taskListManagerImpl) namespace() string {
	name := c.namespaceValue.Load().(string)
	if len(name) > 0 {
		return name
	}
	c.tryInitNamespaceAndScope()
	return c.namespaceValue.Load().(string)
}

// reload from namespaceCache in case it got empty result during construction
func (c *taskListManagerImpl) tryInitNamespaceAndScope() {
	namespace := c.namespaceValue.Load().(string)
	if namespace != "" {
		return
	}

	entry, err := c.namespaceCache.GetNamespaceByID(c.taskListID.namespaceID)
	if err != nil {
		return
	}

	namespace = entry.GetInfo().Name

	scope := newPerTaskListScope(
		namespace,
		c.taskListID.name,
		c.taskListKind,
		c.metricsClient,
		metrics.MatchingTaskListMgrScope,
	)

	c.metricScopeValue.Store(scope)
	c.namespaceValue.Store(namespace)
}
