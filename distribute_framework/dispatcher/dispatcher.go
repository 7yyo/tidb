// Copyright 2023 PingCAP, Inc.
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

package dispatcher

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/distribute_framework/proto"
	"github.com/pingcap/tidb/distribute_framework/storage"
	"github.com/pingcap/tidb/domain/infosync"
	tidbutil "github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

const (
	DefaultConcurrency        = 8
	DefaultSubtaskConcurrency = 16
	MaxSubtaskConcurrency     = 256
	checkTaskFinishedInterval = 300 * time.Millisecond
	checkTaskRunningInterval  = 300 * time.Millisecond
)

type Dispatch interface {
	// Start enables dispatching and monitoring mechanisms.
	Start()
	// Stop stops the dispatcher.
	Stop()
	// GetEligibleInstance gets an eligible instance.
	GetEligibleInstance(ctx context.Context) (string, error)
}

type dispatcher struct {
	ctx        context.Context
	cancel     context.CancelFunc
	gTaskMgr   *storage.GlobalTaskManager
	subTaskMgr *storage.SubTaskManager
	wg         tidbutil.WaitGroupWrapper

	runningGlobalTasks struct {
		sync.RWMutex
		tasks map[int64]*proto.Task
	}
}

func (d *dispatcher) getRunningGlobalTasks() map[int64]*proto.Task {
	d.runningGlobalTasks.RLock()
	defer d.runningGlobalTasks.RUnlock()
	return d.runningGlobalTasks.tasks
}

func (d *dispatcher) isRunningGlobalTask(gTask *proto.Task) bool {
	d.runningGlobalTasks.RLock()
	defer d.runningGlobalTasks.RUnlock()
	_, ok := d.runningGlobalTasks.tasks[gTask.ID]
	return ok
}

func (d *dispatcher) setRunningGlobalTasks(gTask *proto.Task) {
	d.runningGlobalTasks.Lock()
	defer d.runningGlobalTasks.Unlock()
	d.runningGlobalTasks.tasks[gTask.ID] = gTask
}

func (d *dispatcher) delRunningGlobalTasks(globalTaskID int64) {
	d.runningGlobalTasks.Lock()
	defer d.runningGlobalTasks.Unlock()
	delete(d.runningGlobalTasks.tasks, globalTaskID)
}

func (d *dispatcher) detectionTask(gTask *proto.Task) (isFinished bool, subTaskErr string) {
	// TODO: Consider putting the following operations into a transaction.
	// TODO: Consider collect some information about the tasks.
	cnt, err := d.subTaskMgr.CheckTaskState(gTask.ID, proto.TaskStateFailed, true)
	if err != nil {
		logutil.BgLogger().Warn("check task failed", zap.Error(err))
		return false, ""
	}
	if cnt > 0 {
		return false, proto.TaskStateFailed
	}

	// Suppose that the tasks are succeed or reverted  means that all subtask finish.
	cnt, err = d.subTaskMgr.CheckTaskNonStates(gTask.ID, proto.TaskStateSucceed, proto.TaskStateReverted)
	if err != nil {
		logutil.BgLogger().Warn("check task failed", zap.Error(err))
		return false, ""
	}
	if cnt > 0 {
		return false, ""
	}
	if gTask.State == proto.TaskStateReverting {
		return true, proto.TaskStateReverted
	}

	return true, ""
}

// DetectionTaskLoop monitors the status of the subtasks.
func (d *dispatcher) DetectionTaskLoop() {
	ticker := time.NewTicker(checkTaskFinishedInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			logutil.BgLogger().Info("detection task loop exits", zap.Error(d.ctx.Err()))
			return
		case <-ticker.C:
			gTasks := d.getRunningGlobalTasks()
			// TODO: Do we need to handle it asynchronously?
			for _, gTask := range gTasks {
				stepIsFinished, errStr := d.detectionTask(gTask)
				// The global task isn't finished and failed.
				if !stepIsFinished && errStr == "" {
					logutil.BgLogger().Debug("detection, this task keeps current state",
						zap.Int64("taskID", gTask.ID), zap.String("state", gTask.State))
					continue
				}

				var err error
				if stepIsFinished {
					logutil.BgLogger().Info("detection, load task and progress", zap.Int64("taskID", gTask.ID))
					err = d.loadTaskAndProgress(gTask, false)
				} else {
					logutil.BgLogger().Info("detection, handle an error", zap.Int64("taskID", gTask.ID))
					err = d.handleError(gTask, errStr)
				}
				if err == nil && (gTask.State == proto.TaskStateSucceed || gTask.State == proto.TaskStateReverted) {
					logutil.BgLogger().Info("detection, task is finished", zap.Int64("taskID", gTask.ID))
					d.delRunningGlobalTasks(gTask.ID)
				}
			}
		}
	}
}

func (d *dispatcher) updateTaskRevertInfo(gTask *proto.Task) error {
	gTask.State = proto.TaskStateReverted
	// Write the global task meta into the storage.
	err := d.gTaskMgr.UpdateTask(gTask)
	if err != nil {
		logutil.BgLogger().Warn("update global task failed", zap.Error(err))
		return err
	}
	return nil
}

func (d *dispatcher) handleError(gTask *proto.Task, receiveErr string) error {
	// All subtasks are reverted, update the global task.
	if receiveErr == proto.TaskStateReverted {
		return d.updateTaskRevertInfo(gTask)
	}

	meta, err := GetGTaskFlowHandle(gTask.Type).HandleError(d, gTask, receiveErr)
	if err != nil {
		logutil.BgLogger().Warn("handle error failed", zap.Error(err))
		// TODO: Consider retry
		return err
	}

	// TODO: Consider using a new context.
	instanceIDs, err := d.getTaskAllInstances(d.ctx, gTask.ID)
	if err != nil {
		logutil.BgLogger().Warn("get global task's all instances failed", zap.Error(err))
		return err
	}

	if len(instanceIDs) == 0 {
		gTask.State = proto.TaskStateReverted
	} else {
		gTask.State = proto.TaskStateReverting
	}
	// Write the global task meta into the storage.
	err = d.gTaskMgr.UpdateTask(gTask)
	if err != nil {
		logutil.BgLogger().Warn("update global task failed", zap.Error(err))
		return err
	}

	// New rollback subtasks and write into the storage.
	for _, id := range instanceIDs {
		// TODO: Generate the subtask ID.
		subtask := &proto.Subtask{
			Type:        gTask.Type,
			TaskID:      gTask.ID,
			State:       gTask.State,
			SchedulerID: id,
			Meta:        *meta,
		}
		err = d.subTaskMgr.AddNewTask(gTask.ID, subtask.SchedulerID, nil, gTask.Type)
		if err != nil {
			logutil.BgLogger().Warn("add subtask failed", zap.Stringer("subtask", subtask), zap.Error(err))
			return err
		}
	}
	return nil
}

func (d *dispatcher) loadTaskAndProgress(gTask *proto.Task, fromPending bool) (err error) {
	// Generate the needed global task meta and subTask meta.
	finished, subtasks, err := GetGTaskFlowHandle(gTask.Type).Progress(d, gTask, fromPending)
	if err != nil {
		logutil.BgLogger().Warn("gen dist-plan failed", zap.Error(err))
		return err
	}

	// Adjust global task meta.
	if gTask.Concurrency == 0 {
		gTask.Concurrency = DefaultSubtaskConcurrency
	}
	if gTask.Concurrency > MaxSubtaskConcurrency {
		gTask.Concurrency = MaxSubtaskConcurrency
	}
	if finished {
		gTask.State = proto.TaskStateSucceed
	}

	// Special handling for the new tasks.
	// About TaskStateSucceed gTask, we consider keeping records first.
	if gTask.State == proto.TaskStatePending {
		// TODO: Consider time zone.
		gTask.StartTime = time.Now()
		gTask.State = proto.TaskStateRunning
	}

	// TODO: UpdateTask and addSubtasks in a txn.
	// Write the global task meta into the storage.
	err = d.gTaskMgr.UpdateTask(gTask)
	if err != nil {
		logutil.BgLogger().Warn("update global task failed", zap.Error(err))
		return err
	}

	if finished {
		return nil
	}

	// Write subtasks into the storage.
	for _, subtask := range subtasks {
		// TODO: Using the following code.
		// instanceID, err := d.GetEligibleInstance(d.ctx)
		// if err != nil {
		// 	logutil.BgLogger().Warn("get a eligible instance failed", zap.Stringer("subtask", subtask), zap.Error(err))
		// 	return err
		// }
		// subtask.SchedulerID = instanceID

		// TODO: Consider batch splitting
		// TODO: Synchronization interruption problem, e.g. AddNewTask failed
		// TODO: batch insert
		err = d.subTaskMgr.AddNewTask(gTask.ID, subtask.SchedulerID, subtask.Meta.Serialize(), gTask.Type)
		if err != nil {
			logutil.BgLogger().Warn("add subtask failed", zap.Stringer("subtask", subtask), zap.Error(err))
			return err
		}
	}
	return nil
}

// DispatchTaskLoop dispatches the global tasks.
func (d *dispatcher) DispatchTaskLoop() {
	ticker := time.NewTicker(checkTaskRunningInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			logutil.BgLogger().Info("dispatch task loop exits", zap.Error(d.ctx.Err()))
			return
		case <-ticker.C:
			cnt := len(d.getRunningGlobalTasks())

			for cnt < DefaultConcurrency {
				// TODO: Consider retry
				gTask, err := d.gTaskMgr.GetNewTask()
				if err != nil {
					logutil.BgLogger().Warn("get new task failed", zap.Error(err))
					break
				}
				// There are currently no global tasks to work on.
				if gTask == nil {
					break
				}
				if d.isRunningGlobalTask(gTask) {
					break
				}
				if gTask.State == proto.TaskStateReverting {
					d.setRunningGlobalTasks(gTask)
					cnt++
					continue
				}

				err = d.loadTaskAndProgress(gTask, true)
				if err != nil {
					d.delRunningGlobalTasks(gTask.ID)
				}
				d.setRunningGlobalTasks(gTask)
				cnt++
			}
		}
	}
}

// NewDispatcher creates a dispatcher struct.
func NewDispatcher(ctx context.Context, globalTaskTable *storage.GlobalTaskManager, subtaskTable *storage.SubTaskManager) (*dispatcher, error) {
	// TODO: Consider session using.
	dispatcher := &dispatcher{
		gTaskMgr:   globalTaskTable,
		subTaskMgr: subtaskTable,
	}
	dispatcher.ctx, dispatcher.cancel = context.WithCancel(ctx)
	dispatcher.runningGlobalTasks.tasks = make(map[int64]*proto.Task)

	return dispatcher, nil
}

// Start implements Dispatch.Start interface.
func (d *dispatcher) Start() {
	d.wg.Run(func() {
		d.DispatchTaskLoop()
	})
	d.wg.Run(func() {
		d.DetectionTaskLoop()
	})
}

// Stop implements Dispatch.Stop interface.
func (d *dispatcher) Stop() {
	d.cancel()
	d.wg.Wait()
}

// GetEligibleInstance implements Dispatch.GetEligibleInstance interface.
func (d *dispatcher) GetEligibleInstance(ctx context.Context) (string, error) {
	serverInfos, err := infosync.GetAllServerInfo(ctx)
	if err != nil {
		return "", err
	}
	if len(serverInfos) == 0 {
		return "", errors.New("not found instance")
	}

	// TODO: Consider valid instances, and then consider scheduling strategies.
	num := rand.Intn(len(serverInfos))
	for _, info := range serverInfos {
		if num == 0 {
			return info.ID, nil
		}
		num--
	}
	return "", errors.New("not found instance")
}

func (d *dispatcher) getTaskAllInstances(ctx context.Context, gTaskID int64) ([]string, error) {
	serverInfos, err := infosync.GetAllServerInfo(ctx)
	if err != nil {
		return nil, err
	}
	if len(serverInfos) == 0 {
		return nil, nil
	}

	schedulerIDs, err := d.subTaskMgr.GetSchedulerIDs(gTaskID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(schedulerIDs))
	for _, id := range schedulerIDs {
		if _, ok := serverInfos[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
