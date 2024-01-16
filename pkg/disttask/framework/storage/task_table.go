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

package storage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/go-units"
	"github.com/ngaut/pools"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/disttask/framework/proto"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/cpu"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/sqlescape"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"github.com/tikv/client-go/v2/util"
	"go.uber.org/zap"
)

const (
	defaultSubtaskKeepDays = 14

	basicTaskColumns = `id, task_key, type, state, step, priority, concurrency, create_time`
	// TODO: dispatcher_id will update to scheduler_id later
	taskColumns = basicTaskColumns + `, start_time, state_update_time, meta, dispatcher_id, error`
	// InsertTaskColumns is the columns used in insert task.
	InsertTaskColumns   = `task_key, type, state, priority, concurrency, step, meta, create_time`
	basicSubtaskColumns = `id, step, task_key, type, exec_id, state, concurrency, create_time, ordinal`
	// SubtaskColumns is the columns for subtask.
	SubtaskColumns = basicSubtaskColumns + `, start_time, state_update_time, meta, summary`
	// InsertSubtaskColumns is the columns used in insert subtask.
	InsertSubtaskColumns = `step, task_key, exec_id, meta, state, type, concurrency, ordinal, create_time, checkpoint, summary`
)

var (
	maxSubtaskBatchSize = 16 * units.MiB

	// ErrUnstableSubtasks is the error when we detected that the subtasks are
	// unstable, i.e. count, order and content of the subtasks are changed on
	// different call.
	ErrUnstableSubtasks = errors.New("unstable subtasks")

	// ErrTaskNotFound is the error when we can't found task.
	// i.e. TransferTasks2History move task from tidb_global_task to tidb_global_task_history.
	ErrTaskNotFound = errors.New("task not found")

	// ErrTaskAlreadyExists is the error when we submit a task with the same task key.
	// i.e. SubmitTask in handle may submit a task twice.
	ErrTaskAlreadyExists = errors.New("task already exists")

	// ErrSubtaskNotFound is the error when can't find subtask by subtask_id and execId,
	// i.e. scheduler change the subtask's execId when subtask need to balance to other nodes.
	ErrSubtaskNotFound = errors.New("subtask not found")
)

// SessionExecutor defines the interface for executing SQLs in a session.
type SessionExecutor interface {
	// WithNewSession executes the function with a new session.
	WithNewSession(fn func(se sessionctx.Context) error) error
	// WithNewTxn executes the fn in a new transaction.
	WithNewTxn(ctx context.Context, fn func(se sessionctx.Context) error) error
}

// TaskHandle provides the interface for operations needed by Scheduler.
// Then we can use scheduler's function in Scheduler interface.
type TaskHandle interface {
	// GetPreviousSubtaskMetas gets previous subtask metas.
	GetPreviousSubtaskMetas(taskID int64, step proto.Step) ([][]byte, error)
	SessionExecutor
}

// TaskManager is the manager of task and subtask.
type TaskManager struct {
	sePool sessionPool
}

type sessionPool interface {
	Get() (pools.Resource, error)
	Put(resource pools.Resource)
}

var _ SessionExecutor = &TaskManager{}

var taskManagerInstance atomic.Pointer[TaskManager]

var (
	// TestLastTaskID is used for test to set the last task ID.
	TestLastTaskID atomic.Int64
)

// NewTaskManager creates a new task manager.
func NewTaskManager(sePool sessionPool) *TaskManager {
	return &TaskManager{
		sePool: sePool,
	}
}

// GetTaskManager gets the task manager.
func GetTaskManager() (*TaskManager, error) {
	v := taskManagerInstance.Load()
	if v == nil {
		return nil, errors.New("task manager is not initialized")
	}
	return v, nil
}

// SetTaskManager sets the task manager.
func SetTaskManager(is *TaskManager) {
	taskManagerInstance.Store(is)
}

func row2TaskBasic(r chunk.Row) *proto.Task {
	task := &proto.Task{
		ID:          r.GetInt64(0),
		Key:         r.GetString(1),
		Type:        proto.TaskType(r.GetString(2)),
		State:       proto.TaskState(r.GetString(3)),
		Step:        proto.Step(r.GetInt64(4)),
		Priority:    int(r.GetInt64(5)),
		Concurrency: int(r.GetInt64(6)),
	}
	task.CreateTime, _ = r.GetTime(7).GoTime(time.Local)
	return task
}

// row2Task converts a row to a task.
func row2Task(r chunk.Row) *proto.Task {
	task := row2TaskBasic(r)
	var startTime, updateTime time.Time
	if !r.IsNull(8) {
		startTime, _ = r.GetTime(8).GoTime(time.Local)
	}
	if !r.IsNull(9) {
		updateTime, _ = r.GetTime(9).GoTime(time.Local)
	}
	task.StartTime = startTime
	task.StateUpdateTime = updateTime
	task.Meta = r.GetBytes(10)
	task.SchedulerID = r.GetString(11)
	if !r.IsNull(12) {
		errBytes := r.GetBytes(12)
		stdErr := errors.Normalize("")
		err := stdErr.UnmarshalJSON(errBytes)
		if err != nil {
			logutil.BgLogger().Error("unmarshal task error", zap.Error(err))
			task.Error = errors.New(string(errBytes))
		} else {
			task.Error = stdErr
		}
	}
	return task
}

// WithNewSession executes the function with a new session.
func (mgr *TaskManager) WithNewSession(fn func(se sessionctx.Context) error) error {
	se, err := mgr.sePool.Get()
	if err != nil {
		return err
	}
	defer mgr.sePool.Put(se)
	return fn(se.(sessionctx.Context))
}

// WithNewTxn executes the fn in a new transaction.
func (mgr *TaskManager) WithNewTxn(ctx context.Context, fn func(se sessionctx.Context) error) error {
	ctx = util.WithInternalSourceType(ctx, kv.InternalDistTask)
	return mgr.WithNewSession(func(se sessionctx.Context) (err error) {
		_, err = sqlexec.ExecSQL(ctx, se, "begin")
		if err != nil {
			return err
		}

		success := false
		defer func() {
			sql := "rollback"
			if success {
				sql = "commit"
			}
			_, commitErr := sqlexec.ExecSQL(ctx, se, sql)
			if err == nil && commitErr != nil {
				err = commitErr
			}
		}()

		if err = fn(se); err != nil {
			return err
		}

		success = true
		return nil
	})
}

// ExecuteSQLWithNewSession executes one SQL with new session.
func (mgr *TaskManager) ExecuteSQLWithNewSession(ctx context.Context, sql string, args ...interface{}) (rs []chunk.Row, err error) {
	err = mgr.WithNewSession(func(se sessionctx.Context) error {
		rs, err = sqlexec.ExecSQL(ctx, se, sql, args...)
		return err
	})

	if err != nil {
		return nil, err
	}

	return
}

// CreateTask adds a new task to task table.
func (mgr *TaskManager) CreateTask(ctx context.Context, key string, tp proto.TaskType, concurrency int, meta []byte) (taskID int64, err error) {
	err = mgr.WithNewSession(func(se sessionctx.Context) error {
		var err2 error
		taskID, err2 = mgr.CreateTaskWithSession(ctx, se, key, tp, concurrency, meta)
		return err2
	})
	return
}

// CreateTaskWithSession adds a new task to task table with session.
func (mgr *TaskManager) CreateTaskWithSession(ctx context.Context, se sessionctx.Context, key string, tp proto.TaskType, concurrency int, meta []byte) (taskID int64, err error) {
	cpuCount, err := mgr.getCPUCountOfManagedNode(ctx, se)
	if err != nil {
		return 0, err
	}
	if concurrency > cpuCount {
		return 0, errors.Errorf("task concurrency(%d) larger than cpu count(%d) of managed node", concurrency, cpuCount)
	}
	_, err = sqlexec.ExecSQL(ctx, se, `
			insert into mysql.tidb_global_task(`+InsertTaskColumns+`)
			values (%?, %?, %?, %?, %?, %?, %?, CURRENT_TIMESTAMP())`,
		key, tp, proto.TaskStatePending, proto.NormalPriority, concurrency, proto.StepInit, meta)
	if err != nil {
		return 0, err
	}

	rs, err := sqlexec.ExecSQL(ctx, se, "select @@last_insert_id")
	if err != nil {
		return 0, err
	}

	taskID = int64(rs[0].GetUint64(0))
	failpoint.Inject("testSetLastTaskID", func() { TestLastTaskID.Store(taskID) })

	return taskID, nil
}

// GetOneTask get a task from task table, it's used by scheduler only.
func (mgr *TaskManager) GetOneTask(ctx context.Context) (task *proto.Task, err error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, "select "+taskColumns+" from mysql.tidb_global_task where state = %? limit 1", proto.TaskStatePending)
	if err != nil {
		return task, err
	}

	if len(rs) == 0 {
		return nil, nil
	}

	return row2Task(rs[0]), nil
}

// GetTopUnfinishedTasks implements the scheduler.TaskManager interface.
func (mgr *TaskManager) GetTopUnfinishedTasks(ctx context.Context) (task []*proto.Task, err error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx,
		`select `+basicTaskColumns+` from mysql.tidb_global_task
		where state in (%?, %?, %?, %?, %?, %?)
		order by priority asc, create_time asc, id asc
		limit %?`,
		proto.TaskStatePending,
		proto.TaskStateRunning,
		proto.TaskStateReverting,
		proto.TaskStateCancelling,
		proto.TaskStatePausing,
		proto.TaskStateResuming,
		proto.MaxConcurrentTask*2,
	)
	if err != nil {
		return task, err
	}

	for _, r := range rs {
		task = append(task, row2TaskBasic(r))
	}
	return task, nil
}

// GetTasksInStates gets the tasks in the states(order by priority asc, create_time acs, id asc).
func (mgr *TaskManager) GetTasksInStates(ctx context.Context, states ...interface{}) (task []*proto.Task, err error) {
	if len(states) == 0 {
		return task, nil
	}

	rs, err := mgr.ExecuteSQLWithNewSession(ctx,
		"select "+taskColumns+" from mysql.tidb_global_task "+
			"where state in ("+strings.Repeat("%?,", len(states)-1)+"%?)"+
			" order by priority asc, create_time asc, id asc", states...)
	if err != nil {
		return task, err
	}

	for _, r := range rs {
		task = append(task, row2Task(r))
	}
	return task, nil
}

// GetTasksFromHistoryInStates gets the tasks in history table in the states.
func (mgr *TaskManager) GetTasksFromHistoryInStates(ctx context.Context, states ...interface{}) (task []*proto.Task, err error) {
	if len(states) == 0 {
		return task, nil
	}

	rs, err := mgr.ExecuteSQLWithNewSession(ctx, "select "+taskColumns+" from mysql.tidb_global_task_history where state in ("+strings.Repeat("%?,", len(states)-1)+"%?)", states...)
	if err != nil {
		return task, err
	}

	for _, r := range rs {
		task = append(task, row2Task(r))
	}
	return task, nil
}

// GetTaskByID gets the task by the task ID.
func (mgr *TaskManager) GetTaskByID(ctx context.Context, taskID int64) (task *proto.Task, err error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, "select "+taskColumns+" from mysql.tidb_global_task where id = %?", taskID)
	if err != nil {
		return task, err
	}
	if len(rs) == 0 {
		return nil, ErrTaskNotFound
	}

	return row2Task(rs[0]), nil
}

// GetTaskByIDWithHistory gets the task by the task ID from both tidb_global_task and tidb_global_task_history.
func (mgr *TaskManager) GetTaskByIDWithHistory(ctx context.Context, taskID int64) (task *proto.Task, err error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, "select "+taskColumns+" from mysql.tidb_global_task where id = %? "+
		"union select "+taskColumns+" from mysql.tidb_global_task_history where id = %?", taskID, taskID)
	if err != nil {
		return task, err
	}
	if len(rs) == 0 {
		return nil, ErrTaskNotFound
	}

	return row2Task(rs[0]), nil
}

// GetTaskByKey gets the task by the task key.
func (mgr *TaskManager) GetTaskByKey(ctx context.Context, key string) (task *proto.Task, err error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, "select "+taskColumns+" from mysql.tidb_global_task where task_key = %?", key)
	if err != nil {
		return task, err
	}
	if len(rs) == 0 {
		return nil, ErrTaskNotFound
	}

	return row2Task(rs[0]), nil
}

// GetTaskByKeyWithHistory gets the task from history table by the task key.
func (mgr *TaskManager) GetTaskByKeyWithHistory(ctx context.Context, key string) (task *proto.Task, err error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, "select "+taskColumns+" from mysql.tidb_global_task where task_key = %?"+
		"union select "+taskColumns+" from mysql.tidb_global_task_history where task_key = %?", key, key)
	if err != nil {
		return task, err
	}
	if len(rs) == 0 {
		return nil, ErrTaskNotFound
	}

	return row2Task(rs[0]), nil
}

// GetUsedSlotsOnNodes implements the scheduler.TaskManager interface.
func (mgr *TaskManager) GetUsedSlotsOnNodes(ctx context.Context) (map[string]int, error) {
	// concurrency of subtasks of some step is the same, we use max(concurrency)
	// to make group by works.
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `
		select
			exec_id, sum(concurrency)
		from (
			select exec_id, task_key, max(concurrency) concurrency
			from mysql.tidb_background_subtask
			where state in (%?, %?)
			group by exec_id, task_key
		) a
		group by exec_id`,
		proto.TaskStatePending, proto.TaskStateRunning,
	)
	if err != nil {
		return nil, err
	}

	slots := make(map[string]int, len(rs))
	for _, r := range rs {
		val, _ := r.GetMyDecimal(1).ToInt()
		slots[r.GetString(0)] = int(val)
	}
	return slots, nil
}

// row2BasicSubTask converts a row to a subtask with basic info
func row2BasicSubTask(r chunk.Row) *proto.Subtask {
	taskIDStr := r.GetString(2)
	tid, err := strconv.Atoi(taskIDStr)
	if err != nil {
		logutil.BgLogger().Warn("unexpected subtask id", zap.String("subtask-id", taskIDStr))
	}
	createTime, _ := r.GetTime(7).GoTime(time.Local)
	var ordinal int
	if !r.IsNull(8) {
		ordinal = int(r.GetInt64(8))
	}
	subtask := &proto.Subtask{
		ID:          r.GetInt64(0),
		Step:        proto.Step(r.GetInt64(1)),
		TaskID:      int64(tid),
		Type:        proto.Int2Type(int(r.GetInt64(3))),
		ExecID:      r.GetString(4),
		State:       proto.SubtaskState(r.GetString(5)),
		Concurrency: int(r.GetInt64(6)),
		CreateTime:  createTime,
		Ordinal:     ordinal,
	}
	return subtask
}

// Row2SubTask converts a row to a subtask.
func Row2SubTask(r chunk.Row) *proto.Subtask {
	subtask := row2BasicSubTask(r)
	// subtask defines start/update time as bigint, to ensure backward compatible,
	// we keep it that way, and we convert it here.
	var startTime, updateTime time.Time
	if !r.IsNull(9) {
		ts := r.GetInt64(9)
		startTime = time.Unix(ts, 0)
	}
	if !r.IsNull(10) {
		ts := r.GetInt64(10)
		updateTime = time.Unix(ts, 0)
	}
	subtask.StartTime = startTime
	subtask.UpdateTime = updateTime
	subtask.Meta = r.GetBytes(11)
	subtask.Summary = r.GetJSON(12).String()
	return subtask
}

// GetSubtasksByStepAndStates gets all subtasks by given states.
func (mgr *TaskManager) GetSubtasksByStepAndStates(ctx context.Context, tidbID string, taskID int64, step proto.Step, states ...proto.SubtaskState) ([]*proto.Subtask, error) {
	args := []interface{}{tidbID, taskID, step}
	for _, state := range states {
		args = append(args, state)
	}
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select `+SubtaskColumns+` from mysql.tidb_background_subtask
		where exec_id = %? and task_key = %? and step = %?
		and state in (`+strings.Repeat("%?,", len(states)-1)+"%?)", args...)
	if err != nil {
		return nil, err
	}

	subtasks := make([]*proto.Subtask, len(rs))
	for i, row := range rs {
		subtasks[i] = Row2SubTask(row)
	}
	return subtasks, nil
}

// GetSubtasksByExecIdsAndStepAndState gets all subtasks by given taskID, exec_id, step and state.
func (mgr *TaskManager) GetSubtasksByExecIdsAndStepAndState(ctx context.Context, execIDs []string, taskID int64, step proto.Step, state proto.SubtaskState) ([]*proto.Subtask, error) {
	args := []interface{}{taskID, step, state}
	for _, execID := range execIDs {
		args = append(args, execID)
	}
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select `+SubtaskColumns+` from mysql.tidb_background_subtask
		where task_key = %? and step = %? and state = %?
		and exec_id in (`+strings.Repeat("%?,", len(execIDs)-1)+"%?)", args...)
	if err != nil {
		return nil, err
	}

	subtasks := make([]*proto.Subtask, len(rs))
	for i, row := range rs {
		subtasks[i] = Row2SubTask(row)
	}
	return subtasks, nil
}

// GetFirstSubtaskInStates gets the first subtask by given states.
func (mgr *TaskManager) GetFirstSubtaskInStates(ctx context.Context, tidbID string, taskID int64, step proto.Step, states ...proto.SubtaskState) (*proto.Subtask, error) {
	args := []interface{}{tidbID, taskID, step}
	for _, state := range states {
		args = append(args, state)
	}
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select `+SubtaskColumns+` from mysql.tidb_background_subtask
		where exec_id = %? and task_key = %? and step = %?
		and state in (`+strings.Repeat("%?,", len(states)-1)+"%?) limit 1", args...)
	if err != nil {
		return nil, err
	}

	if len(rs) == 0 {
		return nil, nil
	}
	return Row2SubTask(rs[0]), nil
}

// FailSubtask update the task's subtask state to failed and set the err.
func (mgr *TaskManager) FailSubtask(ctx context.Context, execID string, taskID int64, err error) error {
	if err == nil {
		return nil
	}
	_, err1 := mgr.ExecuteSQLWithNewSession(ctx,
		`update mysql.tidb_background_subtask
		set state = %?, 
		error = %?, 
		start_time = unix_timestamp(), 
		state_update_time = unix_timestamp(),
		end_time = CURRENT_TIMESTAMP()
		where exec_id = %? and 
		task_key = %? and 
		state in (%?, %?) 
		limit 1;`,
		proto.SubtaskStateFailed,
		serializeErr(err),
		execID,
		taskID,
		proto.SubtaskStatePending,
		proto.SubtaskStateRunning)
	return err1
}

// CancelSubtask update the task's subtasks' state to canceled.
func (mgr *TaskManager) CancelSubtask(ctx context.Context, execID string, taskID int64) error {
	_, err1 := mgr.ExecuteSQLWithNewSession(ctx,
		`update mysql.tidb_background_subtask
		set state = %?, 
		start_time = unix_timestamp(), 
		state_update_time = unix_timestamp(),
		end_time = CURRENT_TIMESTAMP()
		where exec_id = %? and 
		task_key = %? and 
		state in (%?, %?) 
		limit 1;`,
		proto.SubtaskStateCanceled,
		execID,
		taskID,
		proto.SubtaskStatePending,
		proto.SubtaskStateRunning)
	return err1
}

// GetActiveSubtasks implements TaskManager.GetActiveSubtasks.
func (mgr *TaskManager) GetActiveSubtasks(ctx context.Context, taskID int64) ([]*proto.Subtask, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `
		select `+basicSubtaskColumns+` from mysql.tidb_background_subtask
		where task_key = %? and state in (%?, %?)`,
		taskID, proto.SubtaskStatePending, proto.SubtaskStateRunning)
	if err != nil {
		return nil, err
	}
	subtasks := make([]*proto.Subtask, 0, len(rs))
	for _, r := range rs {
		subtasks = append(subtasks, row2BasicSubTask(r))
	}
	return subtasks, nil
}

// GetSubtasksByStepAndState gets the subtask by step and state.
func (mgr *TaskManager) GetSubtasksByStepAndState(ctx context.Context, taskID int64, step proto.Step, state proto.TaskState) ([]*proto.Subtask, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select `+SubtaskColumns+` from mysql.tidb_background_subtask
		where task_key = %? and state = %? and step = %?`,
		taskID, state, step)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, nil
	}
	subtasks := make([]*proto.Subtask, 0, len(rs))
	for _, r := range rs {
		subtasks = append(subtasks, Row2SubTask(r))
	}
	return subtasks, nil
}

// GetSubtaskRowCount gets the subtask row count.
func (mgr *TaskManager) GetSubtaskRowCount(ctx context.Context, taskID int64, step proto.Step) (int64, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select
    	cast(sum(json_extract(summary, '$.row_count')) as signed) as row_count
		from mysql.tidb_background_subtask where task_key = %? and step = %?`,
		taskID, step)
	if err != nil {
		return 0, err
	}
	if len(rs) == 0 {
		return 0, nil
	}
	return rs[0].GetInt64(0), nil
}

// UpdateSubtaskRowCount updates the subtask row count.
func (mgr *TaskManager) UpdateSubtaskRowCount(ctx context.Context, subtaskID int64, rowCount int64) error {
	_, err := mgr.ExecuteSQLWithNewSession(ctx,
		`update mysql.tidb_background_subtask
		set summary = json_set(summary, '$.row_count', %?) where id = %?`,
		rowCount, subtaskID)
	return err
}

// GetSubtaskCntGroupByStates gets the subtask count by states.
func (mgr *TaskManager) GetSubtaskCntGroupByStates(ctx context.Context, taskID int64, step proto.Step) (map[proto.SubtaskState]int64, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `
		select state, count(*)
		from mysql.tidb_background_subtask
		where task_key = %? and step = %?
		group by state`,
		taskID, step)
	if err != nil {
		return nil, err
	}

	res := make(map[proto.SubtaskState]int64, len(rs))
	for _, r := range rs {
		state := proto.SubtaskState(r.GetString(0))
		res[state] = r.GetInt64(1)
	}

	return res, nil
}

// CollectSubTaskError collects the subtask error.
func (mgr *TaskManager) CollectSubTaskError(ctx context.Context, taskID int64) ([]error, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx,
		`select error from mysql.tidb_background_subtask
             where task_key = %? AND state in (%?, %?)`, taskID, proto.SubtaskStateFailed, proto.SubtaskStateCanceled)
	if err != nil {
		return nil, err
	}
	subTaskErrors := make([]error, 0, len(rs))
	for _, row := range rs {
		if row.IsNull(0) {
			subTaskErrors = append(subTaskErrors, nil)
			continue
		}
		errBytes := row.GetBytes(0)
		if len(errBytes) == 0 {
			subTaskErrors = append(subTaskErrors, nil)
			continue
		}
		stdErr := errors.Normalize("")
		err := stdErr.UnmarshalJSON(errBytes)
		if err != nil {
			return nil, err
		}
		subTaskErrors = append(subTaskErrors, stdErr)
	}

	return subTaskErrors, nil
}

// HasSubtasksInStates checks if there are subtasks in the states.
func (mgr *TaskManager) HasSubtasksInStates(ctx context.Context, tidbID string, taskID int64, step proto.Step, states ...proto.SubtaskState) (bool, error) {
	args := []interface{}{tidbID, taskID, step}
	for _, state := range states {
		args = append(args, state)
	}
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select 1 from mysql.tidb_background_subtask
		where exec_id = %? and task_key = %? and step = %?
			and state in (`+strings.Repeat("%?,", len(states)-1)+"%?) limit 1", args...)
	if err != nil {
		return false, err
	}

	return len(rs) > 0, nil
}

// StartSubtask updates the subtask state to running.
func (mgr *TaskManager) StartSubtask(ctx context.Context, subtaskID int64, execID string) error {
	err := mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		vars := se.GetSessionVars()
		_, err := sqlexec.ExecSQL(ctx,
			se,
			`update mysql.tidb_background_subtask
			 set state = %?, start_time = unix_timestamp(), state_update_time = unix_timestamp()
			 where id = %? and exec_id = %?`,
			proto.TaskStateRunning,
			subtaskID,
			execID)
		if err != nil {
			return err
		}
		if vars.StmtCtx.AffectedRows() == 0 {
			return ErrSubtaskNotFound
		}
		return nil
	})
	return err
}

// InitMeta insert the manager information into dist_framework_meta.
func (mgr *TaskManager) InitMeta(ctx context.Context, tidbID string, role string) error {
	return mgr.WithNewSession(func(se sessionctx.Context) error {
		return mgr.InitMetaSession(ctx, se, tidbID, role)
	})
}

// InitMetaSession insert the manager information into dist_framework_meta.
// if the record exists, update the cpu_count and role.
func (*TaskManager) InitMetaSession(ctx context.Context, se sessionctx.Context, execID string, role string) error {
	cpuCount := cpu.GetCPUCount()
	_, err := sqlexec.ExecSQL(ctx, se, `
		insert into mysql.dist_framework_meta(host, role, cpu_count, keyspace_id)
		values (%?, %?, %?, -1)
		on duplicate key
		update cpu_count = %?, role = %?`,
		execID, role, cpuCount, cpuCount, role)
	return err
}

// RecoverMeta insert the manager information into dist_framework_meta.
// if the record exists, update the cpu_count.
// Don't update role for we only update it in `set global tidb_service_scope`.
// if not there might has a data race.
func (mgr *TaskManager) RecoverMeta(ctx context.Context, execID string, role string) error {
	cpuCount := cpu.GetCPUCount()
	_, err := mgr.ExecuteSQLWithNewSession(ctx, `
		insert into mysql.dist_framework_meta(host, role, cpu_count, keyspace_id)
		values (%?, %?, %?, -1)
		on duplicate key
		update cpu_count = %?`,
		execID, role, cpuCount, cpuCount)
	return err
}

// UpdateSubtaskStateAndError updates the subtask state.
func (mgr *TaskManager) UpdateSubtaskStateAndError(
	ctx context.Context,
	execID string,
	id int64, state proto.SubtaskState, subTaskErr error) error {
	_, err := mgr.ExecuteSQLWithNewSession(ctx, `update mysql.tidb_background_subtask
		set state = %?, error = %?, state_update_time = unix_timestamp() where id = %? and exec_id = %?`,
		state, serializeErr(subTaskErr), id, execID)
	return err
}

// FinishSubtask updates the subtask meta and mark state to succeed.
func (mgr *TaskManager) FinishSubtask(ctx context.Context, execID string, id int64, meta []byte) error {
	_, err := mgr.ExecuteSQLWithNewSession(ctx, `update mysql.tidb_background_subtask
		set meta = %?, state = %?, state_update_time = unix_timestamp(), end_time = CURRENT_TIMESTAMP()
		where id = %? and exec_id = %?`,
		meta, proto.TaskStateSucceed, id, execID)
	return err
}

// DeleteSubtasksByTaskID deletes the subtask of the given task ID.
func (mgr *TaskManager) DeleteSubtasksByTaskID(ctx context.Context, taskID int64) error {
	_, err := mgr.ExecuteSQLWithNewSession(ctx, `delete from mysql.tidb_background_subtask
		where task_key = %?`, taskID)
	if err != nil {
		return err
	}

	return nil
}

// GetTaskExecutorIDsByTaskID gets the task executor IDs of the given task ID.
func (mgr *TaskManager) GetTaskExecutorIDsByTaskID(ctx context.Context, taskID int64) ([]string, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select distinct(exec_id) from mysql.tidb_background_subtask
		where task_key = %?`, taskID)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, nil
	}

	instanceIDs := make([]string, 0, len(rs))
	for _, r := range rs {
		id := r.GetString(0)
		instanceIDs = append(instanceIDs, id)
	}

	return instanceIDs, nil
}

// GetTaskExecutorIDsByTaskIDAndStep gets the task executor IDs of the given global task ID and step.
func (mgr *TaskManager) GetTaskExecutorIDsByTaskIDAndStep(ctx context.Context, taskID int64, step proto.Step) ([]string, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, `select distinct(exec_id) from mysql.tidb_background_subtask
		where task_key = %? and step = %?`, taskID, step)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, nil
	}

	instanceIDs := make([]string, 0, len(rs))
	for _, r := range rs {
		id := r.GetString(0)
		instanceIDs = append(instanceIDs, id)
	}

	return instanceIDs, nil
}

// UpdateSubtasksExecIDs update subtasks' execID.
func (mgr *TaskManager) UpdateSubtasksExecIDs(ctx context.Context, subtasks []*proto.Subtask) error {
	// skip the update process.
	if len(subtasks) == 0 {
		return nil
	}
	err := mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		for _, subtask := range subtasks {
			_, err := sqlexec.ExecSQL(ctx, se, `
				update mysql.tidb_background_subtask
				set exec_id = %?
				where id = %? and state = %?`,
				subtask.ExecID, subtask.ID, subtask.State)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// RunningSubtasksBack2Pending implements the taskexecutor.TaskTable interface.
func (mgr *TaskManager) RunningSubtasksBack2Pending(ctx context.Context, subtasks []*proto.Subtask) error {
	// skip the update process.
	if len(subtasks) == 0 {
		return nil
	}
	err := mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		for _, subtask := range subtasks {
			_, err := sqlexec.ExecSQL(ctx, se, `
				update mysql.tidb_background_subtask
				set state = %?, state_update_time = CURRENT_TIMESTAMP()
				where id = %? and exec_id = %? and state = %?`,
				proto.SubtaskStatePending, subtask.ID, subtask.ExecID, proto.SubtaskStateRunning)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// DeleteDeadNodes deletes the dead nodes from mysql.dist_framework_meta.
func (mgr *TaskManager) DeleteDeadNodes(ctx context.Context, nodes []string) error {
	if len(nodes) == 0 {
		return nil
	}
	return mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		deleteSQL := new(strings.Builder)
		if err := sqlescape.FormatSQL(deleteSQL, "delete from mysql.dist_framework_meta where host in("); err != nil {
			return err
		}
		deleteElems := make([]string, 0, len(nodes))
		for _, node := range nodes {
			deleteElems = append(deleteElems, fmt.Sprintf(`"%s"`, node))
		}

		deleteSQL.WriteString(strings.Join(deleteElems, ", "))
		deleteSQL.WriteString(")")
		_, err := sqlexec.ExecSQL(ctx, se, deleteSQL.String())
		return err
	})
}

// PauseSubtasks update all running/pending subtasks to pasued state.
func (mgr *TaskManager) PauseSubtasks(ctx context.Context, execID string, taskID int64) error {
	_, err := mgr.ExecuteSQLWithNewSession(ctx,
		`update mysql.tidb_background_subtask set state = "paused" where task_key = %? and state in ("running", "pending") and exec_id = %?`, taskID, execID)
	return err
}

// ResumeSubtasks update all paused subtasks to pending state.
func (mgr *TaskManager) ResumeSubtasks(ctx context.Context, taskID int64) error {
	_, err := mgr.ExecuteSQLWithNewSession(ctx,
		`update mysql.tidb_background_subtask set state = "pending", error = null where task_key = %? and state = "paused"`, taskID)
	return err
}

// SwitchTaskStep implements the dispatcher.TaskManager interface.
func (mgr *TaskManager) SwitchTaskStep(
	ctx context.Context,
	task *proto.Task,
	nextState proto.TaskState,
	nextStep proto.Step,
	subtasks []*proto.Subtask,
) error {
	return mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		vars := se.GetSessionVars()
		if vars.MemQuotaQuery < variable.DefTiDBMemQuotaQuery {
			bak := vars.MemQuotaQuery
			if err := vars.SetSystemVar(variable.TiDBMemQuotaQuery,
				strconv.Itoa(variable.DefTiDBMemQuotaQuery)); err != nil {
				return err
			}
			defer func() {
				_ = vars.SetSystemVar(variable.TiDBMemQuotaQuery, strconv.Itoa(int(bak)))
			}()
		}
		err := mgr.updateTaskStateStep(ctx, se, task, nextState, nextStep)
		if err != nil {
			return err
		}
		if vars.StmtCtx.AffectedRows() == 0 {
			// on network partition or owner change, there might be multiple
			// schedulers for the same task, if other scheduler has switched
			// the task to next step, skip the update process.
			// Or when there is no such task.
			return nil
		}
		return mgr.insertSubtasks(ctx, se, subtasks)
	})
}

func (*TaskManager) updateTaskStateStep(ctx context.Context, se sessionctx.Context,
	task *proto.Task, nextState proto.TaskState, nextStep proto.Step) error {
	var extraUpdateStr string
	if task.State == proto.TaskStatePending {
		extraUpdateStr = `start_time = CURRENT_TIMESTAMP(),`
	}
	// TODO: during generating subtask, task meta might change, maybe move meta
	// update to another place.
	_, err := sqlexec.ExecSQL(ctx, se, `
		update mysql.tidb_global_task
		set state = %?,
			step = %?, `+extraUpdateStr+`
			state_update_time = CURRENT_TIMESTAMP(),
			meta = %?
		where id = %? and state = %? and step = %?`,
		nextState, nextStep, task.Meta, task.ID, task.State, task.Step)
	return err
}

// TestChannel is used for test.
var TestChannel = make(chan struct{})

func (*TaskManager) insertSubtasks(ctx context.Context, se sessionctx.Context, subtasks []*proto.Subtask) error {
	if len(subtasks) == 0 {
		return nil
	}
	failpoint.Inject("waitBeforeInsertSubtasks", func() {
		<-TestChannel
		<-TestChannel
	})
	var (
		sb         strings.Builder
		markerList = make([]string, 0, len(subtasks))
		args       = make([]interface{}, 0, len(subtasks)*7)
	)
	sb.WriteString(`insert into mysql.tidb_background_subtask(` + InsertSubtaskColumns + `) values `)
	for _, subtask := range subtasks {
		markerList = append(markerList, "(%?, %?, %?, %?, %?, %?, %?, %?, CURRENT_TIMESTAMP(), '{}', '{}')")
		args = append(args, subtask.Step, subtask.TaskID, subtask.ExecID, subtask.Meta,
			proto.TaskStatePending, proto.Type2Int(subtask.Type), subtask.Concurrency, subtask.Ordinal)
	}
	sb.WriteString(strings.Join(markerList, ","))
	_, err := sqlexec.ExecSQL(ctx, se, sb.String(), args...)
	return err
}

// SwitchTaskStepInBatch implements the dispatcher.TaskManager interface.
func (mgr *TaskManager) SwitchTaskStepInBatch(
	ctx context.Context,
	task *proto.Task,
	nextState proto.TaskState,
	nextStep proto.Step,
	subtasks []*proto.Subtask,
) error {
	return mgr.WithNewSession(func(se sessionctx.Context) error {
		// some subtasks may be inserted by other dispatchers, we can skip them.
		rs, err := sqlexec.ExecSQL(ctx, se, `
			select count(1) from mysql.tidb_background_subtask
			where task_key = %? and step = %?`, task.ID, nextStep)
		if err != nil {
			return err
		}
		existingTaskCnt := int(rs[0].GetInt64(0))
		if existingTaskCnt > len(subtasks) {
			return errors.Annotatef(ErrUnstableSubtasks, "expected %d, got %d",
				len(subtasks), existingTaskCnt)
		}
		subtaskBatches := mgr.splitSubtasks(subtasks[existingTaskCnt:])
		for _, batch := range subtaskBatches {
			if err = mgr.insertSubtasks(ctx, se, batch); err != nil {
				return err
			}
		}
		return mgr.updateTaskStateStep(ctx, se, task, nextState, nextStep)
	})
}

func (*TaskManager) splitSubtasks(subtasks []*proto.Subtask) [][]*proto.Subtask {
	var (
		res       = make([][]*proto.Subtask, 0, 10)
		currBatch = make([]*proto.Subtask, 0, 10)
		size      int
	)
	maxSize := int(min(kv.TxnTotalSizeLimit.Load(), uint64(maxSubtaskBatchSize)))
	for _, s := range subtasks {
		if size+len(s.Meta) > maxSize {
			res = append(res, currBatch)
			currBatch = nil
			size = 0
		}
		currBatch = append(currBatch, s)
		size += len(s.Meta)
	}
	if len(currBatch) > 0 {
		res = append(res, currBatch)
	}
	return res
}

// UpdateTaskAndAddSubTasks update the task and add new subtasks
// TODO: remove this when we remove reverting subtasks.
func (mgr *TaskManager) UpdateTaskAndAddSubTasks(ctx context.Context, task *proto.Task, subtasks []*proto.Subtask, prevState proto.TaskState) (bool, error) {
	retryable := true
	err := mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		_, err := sqlexec.ExecSQL(ctx, se, "update mysql.tidb_global_task "+
			"set state = %?, dispatcher_id = %?, step = %?, concurrency = %?, meta = %?, error = %?, state_update_time = CURRENT_TIMESTAMP()"+
			"where id = %? and state = %?",
			task.State, task.SchedulerID, task.Step, task.Concurrency, task.Meta, serializeErr(task.Error), task.ID, prevState)
		if err != nil {
			return err
		}
		// When AffectedRows == 0, means other admin command have changed the task state, it's illegal to schedule subtasks.
		if se.GetSessionVars().StmtCtx.AffectedRows() == 0 {
			if !intest.InTest {
				// task state have changed by other admin command
				retryable = false
				return errors.New("invalid task state transform, state already changed")
			}
			// TODO: remove it, when OnNextSubtasksBatch returns subtasks, just insert subtasks without updating tidb_global_task.
			// Currently the business running on distributed task framework will update proto.Task in OnNextSubtasksBatch.
			// So when scheduling subtasks, framework needs to update task and insert subtasks in one Txn.
			//
			// In future, it's needed to restrict changes of task in OnNextSubtasksBatch.
			// If OnNextSubtasksBatch won't update any fields in proto.Task, we can insert subtasks only.
			//
			// For now, we update nothing in proto.Task in UT's OnNextSubtasksBatch, so the AffectedRows will be 0. So UT can't fully compatible
			// with current UpdateTaskAndAddSubTasks implementation.
			rs, err := sqlexec.ExecSQL(ctx, se, "select id from mysql.tidb_global_task where id = %? and state = %?", task.ID, prevState)
			if err != nil {
				return err
			}
			// state have changed.
			if len(rs) == 0 {
				retryable = false
				return errors.New("invalid task state transform, state already changed")
			}
		}

		failpoint.Inject("MockUpdateTaskErr", func(val failpoint.Value) {
			if val.(bool) {
				failpoint.Return(errors.New("updateTaskErr"))
			}
		})
		if len(subtasks) > 0 {
			subtaskState := proto.SubtaskStatePending
			if task.State == proto.TaskStateReverting {
				subtaskState = proto.SubtaskStateRevertPending
			}

			sql := new(strings.Builder)
			if err := sqlescape.FormatSQL(sql, `insert into mysql.tidb_background_subtask(`+InsertSubtaskColumns+`) values`); err != nil {
				return err
			}
			for i, subtask := range subtasks {
				if i != 0 {
					if err := sqlescape.FormatSQL(sql, ","); err != nil {
						return err
					}
				}
				if err := sqlescape.FormatSQL(sql, "(%?, %?, %?, %?, %?, %?, %?, NULL, CURRENT_TIMESTAMP(), '{}', '{}')",
					subtask.Step, task.ID, subtask.ExecID, subtask.Meta, subtaskState, proto.Type2Int(subtask.Type), subtask.Concurrency); err != nil {
					return err
				}
			}
			_, err := sqlexec.ExecSQL(ctx, se, sql.String())
			if err != nil {
				return nil
			}
		}
		return nil
	})

	return retryable, err
}

func serializeErr(err error) []byte {
	if err == nil {
		return nil
	}
	originErr := errors.Cause(err)
	tErr, ok := originErr.(*errors.Error)
	if !ok {
		tErr = errors.Normalize(originErr.Error())
	}
	errBytes, err := tErr.MarshalJSON()
	if err != nil {
		return nil
	}
	return errBytes
}

// IsTaskCancelling checks whether the task state is cancelling.
func (mgr *TaskManager) IsTaskCancelling(ctx context.Context, taskID int64) (bool, error) {
	rs, err := mgr.ExecuteSQLWithNewSession(ctx, "select 1 from mysql.tidb_global_task where id=%? and state = %?",
		taskID, proto.TaskStateCancelling,
	)

	if err != nil {
		return false, err
	}

	return len(rs) > 0, nil
}

// GetSubtasksWithHistory gets the subtasks from tidb_global_task and tidb_global_task_history.
func (mgr *TaskManager) GetSubtasksWithHistory(ctx context.Context, taskID int64, step proto.Step) ([]*proto.Subtask, error) {
	var (
		rs  []chunk.Row
		err error
	)
	err = mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		rs, err = sqlexec.ExecSQL(ctx, se,
			`select `+SubtaskColumns+` from mysql.tidb_background_subtask where task_key = %? and step = %?`,
			taskID, step,
		)
		if err != nil {
			return err
		}

		// To avoid the situation that the subtasks has been `TransferTasks2History`
		// when the user show import jobs, we need to check the history table.
		rsFromHistory, err := sqlexec.ExecSQL(ctx, se,
			`select `+SubtaskColumns+` from mysql.tidb_background_subtask_history where task_key = %? and step = %?`,
			taskID, step,
		)
		if err != nil {
			return err
		}

		rs = append(rs, rsFromHistory...)
		return nil
	})

	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, nil
	}
	subtasks := make([]*proto.Subtask, 0, len(rs))
	for _, r := range rs {
		subtasks = append(subtasks, Row2SubTask(r))
	}
	return subtasks, nil
}

// TransferSubtasks2HistoryWithSession transfer the selected subtasks into tidb_background_subtask_history table by taskID.
func (*TaskManager) TransferSubtasks2HistoryWithSession(ctx context.Context, se sessionctx.Context, taskID int64) error {
	_, err := sqlexec.ExecSQL(ctx, se, `insert into mysql.tidb_background_subtask_history select * from mysql.tidb_background_subtask where task_key = %?`, taskID)
	if err != nil {
		return err
	}
	// delete taskID subtask
	_, err = sqlexec.ExecSQL(ctx, se, "delete from mysql.tidb_background_subtask where task_key = %?", taskID)
	return err
}

// TransferTasks2History transfer the selected tasks into tidb_global_task_history table by taskIDs.
func (mgr *TaskManager) TransferTasks2History(ctx context.Context, tasks []*proto.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	taskIDStrs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		taskIDStrs = append(taskIDStrs, fmt.Sprintf("%d", task.ID))
	}
	return mgr.WithNewTxn(ctx, func(se sessionctx.Context) error {
		// sensitive data in meta might be redacted, need update first.
		for _, t := range tasks {
			_, err := sqlexec.ExecSQL(ctx, se, `
				update mysql.tidb_global_task
				set meta= %?, state_update_time = CURRENT_TIMESTAMP()
				where id = %?`, t.Meta, t.ID)
			if err != nil {
				return err
			}
		}
		_, err := sqlexec.ExecSQL(ctx, se, `
			insert into mysql.tidb_global_task_history
			select * from mysql.tidb_global_task
			where id in(`+strings.Join(taskIDStrs, `, `)+`)`)
		if err != nil {
			return err
		}

		_, err = sqlexec.ExecSQL(ctx, se, `
			delete from mysql.tidb_global_task
			where id in(`+strings.Join(taskIDStrs, `, `)+`)`)

		for _, t := range tasks {
			err = mgr.TransferSubtasks2HistoryWithSession(ctx, se, t.ID)
			if err != nil {
				return err
			}
		}
		return err
	})
}

// GCSubtasks deletes the history subtask which is older than the given days.
func (mgr *TaskManager) GCSubtasks(ctx context.Context) error {
	subtaskHistoryKeepSeconds := defaultSubtaskKeepDays * 24 * 60 * 60
	failpoint.Inject("subtaskHistoryKeepSeconds", func(val failpoint.Value) {
		if val, ok := val.(int); ok {
			subtaskHistoryKeepSeconds = val
		}
	})
	_, err := mgr.ExecuteSQLWithNewSession(
		ctx,
		fmt.Sprintf("DELETE FROM mysql.tidb_background_subtask_history WHERE state_update_time < UNIX_TIMESTAMP() - %d ;", subtaskHistoryKeepSeconds),
	)
	return err
}

// GetManagedNodes implements scheduler.TaskManager interface.
func (mgr *TaskManager) GetManagedNodes(ctx context.Context) ([]proto.ManagedNode, error) {
	var nodes []proto.ManagedNode
	err := mgr.WithNewSession(func(se sessionctx.Context) error {
		var err2 error
		nodes, err2 = mgr.getManagedNodesWithSession(ctx, se)
		return err2
	})
	return nodes, err
}

func (mgr *TaskManager) getManagedNodesWithSession(ctx context.Context, se sessionctx.Context) ([]proto.ManagedNode, error) {
	nodes, err := mgr.getAllNodesWithSession(ctx, se)
	if err != nil {
		return nil, err
	}
	nodeMap := make(map[string][]proto.ManagedNode, 2)
	for _, node := range nodes {
		nodeMap[node.Role] = append(nodeMap[node.Role], node)
	}
	if len(nodeMap["background"]) == 0 {
		return nodeMap[""], nil
	}
	return nodeMap["background"], nil
}

// GetAllNodes gets nodes in dist_framework_meta.
func (mgr *TaskManager) GetAllNodes(ctx context.Context) ([]proto.ManagedNode, error) {
	var nodes []proto.ManagedNode
	err := mgr.WithNewSession(func(se sessionctx.Context) error {
		var err2 error
		nodes, err2 = mgr.getAllNodesWithSession(ctx, se)
		return err2
	})
	return nodes, err
}

func (*TaskManager) getAllNodesWithSession(ctx context.Context, se sessionctx.Context) ([]proto.ManagedNode, error) {
	rs, err := sqlexec.ExecSQL(ctx, se, `
		select host, role, cpu_count
		from mysql.dist_framework_meta
		order by host`)
	if err != nil {
		return nil, err
	}
	nodes := make([]proto.ManagedNode, 0, len(rs))
	for _, r := range rs {
		nodes = append(nodes, proto.ManagedNode{
			ID:       r.GetString(0),
			Role:     r.GetString(1),
			CPUCount: int(r.GetInt64(2)),
		})
	}
	return nodes, nil
}

// GetCPUCountOfManagedNode gets the cpu count of managed node.
func (mgr *TaskManager) GetCPUCountOfManagedNode(ctx context.Context) (int, error) {
	var cnt int
	err := mgr.WithNewSession(func(se sessionctx.Context) error {
		var err2 error
		cnt, err2 = mgr.getCPUCountOfManagedNode(ctx, se)
		return err2
	})
	return cnt, err
}

// getCPUCountOfManagedNode gets the cpu count of managed node.
// returns error when there's no managed node or no node has valid cpu count.
func (mgr *TaskManager) getCPUCountOfManagedNode(ctx context.Context, se sessionctx.Context) (int, error) {
	nodes, err := mgr.getManagedNodesWithSession(ctx, se)
	if err != nil {
		return 0, err
	}
	if len(nodes) == 0 {
		return 0, errors.New("no managed nodes")
	}
	var cpuCount int
	for _, n := range nodes {
		if n.CPUCount > 0 {
			cpuCount = n.CPUCount
			break
		}
	}
	if cpuCount == 0 {
		return 0, errors.New("no managed node have enough resource for dist task")
	}
	return cpuCount, nil
}
