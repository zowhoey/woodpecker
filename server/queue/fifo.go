// Copyright 2022 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package queue

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v2/server/model"
)

type entry struct {
	item     *model.Task
	done     chan bool
	error    error
	deadline time.Time
}

type worker struct {
	agentID int64
	filter  FilterFn
	channel chan *model.Task
	stop    context.CancelCauseFunc
}

type fifo struct {
	sync.Mutex

	workers       map[*worker]struct{}
	running       map[string]*entry
	pending       *list.List
	waitingOnDeps *list.List
	extension     time.Duration
	paused        bool
}

// New returns a new fifo queue.
func New(_ context.Context) Queue {
	return &fifo{
		workers:       map[*worker]struct{}{},
		running:       map[string]*entry{},
		pending:       list.New(),
		waitingOnDeps: list.New(),
		extension:     time.Minute * 10,
		paused:        false,
	}
}

// Push pushes a task to the tail of this queue.
func (q *fifo) Push(_ context.Context, task *model.Task) error {
	q.Lock()
	q.pending.PushBack(task)
	q.Unlock()
	go q.process()
	return nil
}

// PushAtOnce pushes multiple tasks to the tail of this queue.
func (q *fifo) PushAtOnce(_ context.Context, tasks []*model.Task) error {
	q.Lock()
	for _, task := range tasks {
		q.pending.PushBack(task)
	}
	q.Unlock()
	go q.process()
	return nil
}

// Poll retrieves and removes a task head of this queue.
func (q *fifo) Poll(c context.Context, agentID int64, f FilterFn) (*model.Task, error) {
	q.Lock()
	ctx, stop := context.WithCancelCause(c)

	w := &worker{
		agentID: agentID,
		channel: make(chan *model.Task, 1),
		filter:  f,
		stop:    stop,
	}
	q.workers[w] = struct{}{}
	q.Unlock()
	go q.process()

	for {
		select {
		case <-ctx.Done():
			q.Lock()
			delete(q.workers, w)
			q.Unlock()
			return nil, ctx.Err()
		case t := <-w.channel:
			return t, nil
		}
	}
}

// Done signals the task is complete.
func (q *fifo) Done(_ context.Context, id string, exitStatus model.StatusValue) error {
	return q.finished([]string{id}, exitStatus, nil)
}

// Error signals the task is done with an error.
func (q *fifo) Error(_ context.Context, id string, err error) error {
	return q.finished([]string{id}, model.StatusFailure, err)
}

// ErrorAtOnce signals multiple done are complete with an error.
func (q *fifo) ErrorAtOnce(_ context.Context, ids []string, err error) error {
	return q.finished(ids, model.StatusFailure, err)
}

func (q *fifo) finished(ids []string, exitStatus model.StatusValue, err error) error {
	q.Lock()

	for _, id := range ids {
		taskEntry, ok := q.running[id]
		if ok {
			taskEntry.error = err
			close(taskEntry.done)
			delete(q.running, id)
		} else {
			q.removeFromPending(id)
		}
		q.updateDepStatusInQueue(id, exitStatus)
	}

	q.Unlock()
	return nil
}

// Evict removes a pending task from the queue.
func (q *fifo) Evict(c context.Context, id string) error {
	return q.EvictAtOnce(c, []string{id})
}

// EvictAtOnce removes multiple pending tasks from the queue.
func (q *fifo) EvictAtOnce(_ context.Context, ids []string) error {
	q.Lock()
	defer q.Unlock()

	for _, id := range ids {
		var next *list.Element
		for e := q.pending.Front(); e != nil; e = next {
			next = e.Next()
			task, ok := e.Value.(*model.Task)
			if ok && task.ID == id {
				q.pending.Remove(e)
				return nil
			}
		}
	}
	return ErrNotFound
}

// Wait waits until the item is done executing.
func (q *fifo) Wait(c context.Context, id string) error {
	q.Lock()
	state := q.running[id]
	q.Unlock()
	if state != nil {
		select {
		case <-c.Done():
		case <-state.done:
			return state.error
		}
	}
	return nil
}

// Extend extends the task execution deadline.
func (q *fifo) Extend(_ context.Context, id string) error {
	q.Lock()
	defer q.Unlock()

	state, ok := q.running[id]
	if ok {
		state.deadline = time.Now().Add(q.extension)
		return nil
	}
	return ErrNotFound
}

// Info returns internal queue information.
func (q *fifo) Info(_ context.Context) InfoT {
	q.Lock()
	stats := InfoT{}
	stats.Stats.Workers = len(q.workers)
	stats.Stats.Pending = q.pending.Len()
	stats.Stats.WaitingOnDeps = q.waitingOnDeps.Len()
	stats.Stats.Running = len(q.running)

	for e := q.pending.Front(); e != nil; e = e.Next() {
		task, _ := e.Value.(*model.Task)
		stats.Pending = append(stats.Pending, task)
	}
	for e := q.waitingOnDeps.Front(); e != nil; e = e.Next() {
		task, _ := e.Value.(*model.Task)
		stats.WaitingOnDeps = append(stats.WaitingOnDeps, task)
	}
	for _, entry := range q.running {
		stats.Running = append(stats.Running, entry.item)
	}
	stats.Paused = q.paused

	q.Unlock()
	return stats
}

// Pause stops the queue from handing out new work items in Poll
func (q *fifo) Pause() {
	q.Lock()
	q.paused = true
	q.Unlock()
}

// Resume starts the queue again.
func (q *fifo) Resume() {
	q.Lock()
	q.paused = false
	q.Unlock()
	go q.process()
}

// KickAgentWorkers kicks all workers for a given agent.
func (q *fifo) KickAgentWorkers(agentID int64) {
	q.Lock()
	defer q.Unlock()

	for w := range q.workers {
		if w.agentID == agentID {
			w.stop(fmt.Errorf("worker was kicked"))
			delete(q.workers, w)
		}
	}
}

// helper function that loops through the queue and attempts to
// match the item to a single subscriber.
func (q *fifo) process() {
	q.Lock()
	defer q.Unlock()

	if q.paused {
		return
	}

	q.resubmitExpiredPipelines()
	q.filterWaiting()
	for pending, worker := q.assignToWorker(); pending != nil && worker != nil; pending, worker = q.assignToWorker() {
		task, _ := pending.Value.(*model.Task)
		task.AgentID = worker.agentID
		delete(q.workers, worker)
		q.pending.Remove(pending)
		q.running[task.ID] = &entry{
			item:     task,
			done:     make(chan bool),
			deadline: time.Now().Add(q.extension),
		}
		worker.channel <- task
	}
}

func (q *fifo) filterWaiting() {
	// resubmits all waiting tasks to pending, deps may have cleared
	var nextWaiting *list.Element
	for e := q.waitingOnDeps.Front(); e != nil; e = nextWaiting {
		nextWaiting = e.Next()
		task, _ := e.Value.(*model.Task)
		q.pending.PushBack(task)
	}

	// rebuild waitingDeps
	q.waitingOnDeps = list.New()
	var filtered []*list.Element
	var nextPending *list.Element
	for e := q.pending.Front(); e != nil; e = nextPending {
		nextPending = e.Next()
		task, _ := e.Value.(*model.Task)
		if q.depsInQueue(task) {
			log.Debug().Msgf("queue: waiting due to unmet dependencies %v", task.ID)
			q.waitingOnDeps.PushBack(task)
			filtered = append(filtered, e)
		}
	}

	// filter waiting tasks
	for _, f := range filtered {
		q.pending.Remove(f)
	}
}

func (q *fifo) assignToWorker() (*list.Element, *worker) {
	var next *list.Element
	for e := q.pending.Front(); e != nil; e = next {
		next = e.Next()
		task, _ := e.Value.(*model.Task)
		log.Debug().Msgf("queue: trying to assign task: %v with deps %v", task.ID, task.Dependencies)

		for w := range q.workers {
			if w.filter(task) {
				log.Debug().Msgf("queue: assigned task: %v with deps %v", task.ID, task.Dependencies)
				return e, w
			}
		}
	}

	return nil, nil
}

func (q *fifo) resubmitExpiredPipelines() {
	for id, state := range q.running {
		if time.Now().After(state.deadline) {
			q.pending.PushFront(state.item)
			delete(q.running, id)
			close(state.done)
		}
	}
}

func (q *fifo) depsInQueue(task *model.Task) bool {
	var next *list.Element
	for e := q.pending.Front(); e != nil; e = next {
		next = e.Next()
		possibleDep, ok := e.Value.(*model.Task)
		log.Debug().Msgf("queue: pending right now: %v", possibleDep.ID)
		for _, dep := range task.Dependencies {
			if ok && possibleDep.ID == dep {
				return true
			}
		}
	}
	for possibleDepID := range q.running {
		log.Debug().Msgf("queue: running right now: %v", possibleDepID)
		for _, dep := range task.Dependencies {
			if possibleDepID == dep {
				return true
			}
		}
	}
	return false
}

func (q *fifo) updateDepStatusInQueue(taskID string, status model.StatusValue) {
	var next *list.Element
	for e := q.pending.Front(); e != nil; e = next {
		next = e.Next()
		pending, ok := e.Value.(*model.Task)
		for _, dep := range pending.Dependencies {
			if ok && taskID == dep {
				pending.DepStatus[dep] = status
			}
		}
	}

	for _, running := range q.running {
		for _, dep := range running.item.Dependencies {
			if taskID == dep {
				running.item.DepStatus[dep] = status
			}
		}
	}

	for e := q.waitingOnDeps.Front(); e != nil; e = next {
		next = e.Next()
		waiting, ok := e.Value.(*model.Task)
		for _, dep := range waiting.Dependencies {
			if ok && taskID == dep {
				waiting.DepStatus[dep] = status
			}
		}
	}
}

func (q *fifo) removeFromPending(taskID string) {
	log.Debug().Msgf("queue: trying to remove %s", taskID)
	var next *list.Element
	for e := q.pending.Front(); e != nil; e = next {
		next = e.Next()
		task, _ := e.Value.(*model.Task)
		if task.ID == taskID {
			log.Debug().Msgf("queue: %s is removed from pending", taskID)
			q.pending.Remove(e)
			return
		}
	}
}
