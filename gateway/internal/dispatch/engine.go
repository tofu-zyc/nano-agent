package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Engine is one bounded persistence worker, separate from the App lock. No
// execution grant is published before its durable ownership record is synced.
type Engine struct {
	ctx       context.Context
	scheduler *Scheduler
	file      string
	queue     chan Command
	done      chan struct{}
	publish   func(Change)
	mu        sync.Mutex
	committed Checkpoint
	lastError string
}
type Command struct {
	RequestID       string
	Action          string
	Request         Request
	ID, Owner, Node string
	Inventory       []Lease
}
type Change struct {
	RequestID       string
	Action          string
	ID, Owner, Node string
	Lease           Lease
	Grants          []Lease
	Expired         []Lease
	Err             error
}

const maxCheckpointBytes = 8 << 20

func Open(ctx context.Context, file string, cfg Config, publish func(Change)) (*Engine, error) {
	scheduler, err := New(cfg)
	if err != nil {
		return nil, err
	}
	checkpoint := Checkpoint{Version: 1}
	f, err := os.Open(file)
	if err == nil {
		b, readErr := io.ReadAll(io.LimitReader(f, maxCheckpointBytes+1))
		f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if len(b) > maxCheckpointBytes {
			return nil, errors.New("执行租约恢复文件超过上限")
		}
		if err := json.Unmarshal(b, &checkpoint); err != nil {
			return nil, fmt.Errorf("执行租约恢复文件损坏：%w", err)
		}
		if err := scheduler.Restore(checkpoint, true); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	e := &Engine{ctx: ctx, scheduler: scheduler, file: file, queue: make(chan Command, 128), done: make(chan struct{}), publish: publish, committed: scheduler.Checkpoint()}
	go e.loop()
	return e, nil
}

func (e *Engine) Submit(command Command) bool {
	command.Request.Shared = append([]string(nil), command.Request.Shared...)
	command.Request.Exclusive = append([]string(nil), command.Request.Exclusive...)
	if command.Inventory != nil {
		inventory := make([]Lease, len(command.Inventory))
		for i := range inventory {
			inventory[i] = clone(&command.Inventory[i])
		}
		command.Inventory = inventory
	}
	select {
	case <-e.ctx.Done():
		return false
	default:
	}
	select {
	case e.queue <- command:
		return true
	default:
		return false
	}
}
func (e *Engine) Done() <-chan struct{} { return e.done }
func (e *Engine) Snapshot() (Checkpoint, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.committed
	out.Leases = make([]Lease, len(e.committed.Leases))
	for i := range out.Leases {
		out.Leases[i] = clone(&e.committed.Leases[i])
	}
	out.Closed = append([]Closed(nil), out.Closed...)
	return out, e.lastError
}

func (e *Engine) save(checkpoint Checkpoint) error {
	b, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if len(b) > maxCheckpointBytes {
		return errors.New("执行租约检查点超过字节上限")
	}
	if err := os.MkdirAll(filepath.Dir(e.file), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(e.file), ".leases-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(b); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceCheckpoint(name, e.file)
}

func (e *Engine) apply(command Command, now time.Time) {
	before := e.scheduler.Checkpoint()
	change := Change{RequestID: command.RequestID, Action: command.Action, ID: command.ID, Owner: command.Owner, Node: command.Node}
	switch command.Action {
	case "admit":
		change.ID, change.Owner = command.Request.ID, command.Request.Owner
		change.Lease, change.Grants, change.Err = e.scheduler.Admit(command.Request, now)
	case "started":
		change.Err = e.scheduler.Started(command.ID, command.Owner, now)
		change.Lease, _ = e.scheduler.Lookup(command.ID)
	case "release":
		change.Grants, change.Err = e.scheduler.Release(command.ID, command.Owner, now)
	case "unknown":
		change.Err = e.scheduler.MarkUnknown(command.ID, command.Owner)
	case "cancel":
		change.Grants, change.Err = e.scheduler.Cancel(command.ID, command.Owner, now)
	case "detach_prepare":
		change.Lease, change.Err = e.scheduler.PrepareDetach(command.ID, command.Owner, command.RequestID)
	case "detach_commit":
		change.Lease, change.Grants, change.Err = e.scheduler.CommitDetach(command.ID, command.Owner, command.RequestID, now)
	case "detach_abort":
		change.Err = e.scheduler.AbortDetach(command.ID, command.Owner, command.RequestID)
	case "detach":
		change.Lease, change.Err = e.scheduler.Detach(command.ID, command.Owner)
		if change.Err == nil {
			change.Grants = e.scheduler.Pump(now)
		}
	case "disconnect":
		e.scheduler.Disconnect(command.Owner)
	case "sync":
		change.Grants, change.Err = e.scheduler.SyncNode(command.Node, command.Owner, command.Inventory, now)
	case "tick":
		change.Expired, change.Grants = e.scheduler.Expire(now)
		if len(change.Expired) == 0 && len(change.Grants) == 0 {
			return
		}
	default:
		change.Err = errors.New("未知的执行租约操作")
	}
	storageFailure := false
	if change.Err == nil {
		change.Err = e.save(e.scheduler.Checkpoint())
		storageFailure = change.Err != nil
	}
	e.mu.Lock()
	if change.Err != nil {
		_ = e.scheduler.Restore(before, false)
		change.Grants = nil
		change.Expired = nil
		if storageFailure {
			e.lastError = change.Err.Error()
		}
	} else {
		e.committed = e.scheduler.Checkpoint()
		e.lastError = ""
	}
	e.mu.Unlock()
	if e.publish != nil {
		e.publish(change)
	}
}

func (e *Engine) loop() {
	defer close(e.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if e.ctx.Err() != nil {
			return
		}
		select {
		case <-e.ctx.Done():
			return
		case command := <-e.queue:
			e.apply(command, time.Now())
		case now := <-ticker.C:
			e.apply(Command{Action: "tick"}, now)
		}
	}
}
