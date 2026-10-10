package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// The SDK's view of our store. The interface is small but it makes two demands we have
// to honour rather than approximate:
//
//   - Create must refuse an id it already holds (ErrTaskAlreadyExists) rather than
//     overwrite it;
//   - Update carries the version it expects (PrevVersion) and MUST be refused
//     (ErrConcurrentModification) when that is not the current one. That is what stops
//     the stack's own status and artifact updates from silently clobbering each other.
//
// The caller-identity half of List is the SDK's Authenticator idea, implemented here
// from the CallContext: with no authenticated caller there is no owner to scope a list
// to, and answering a list anyway would be the resource-leak the design forbids. It is
// filled by the node's own surface — the TLS handshake establishes the identity and an
// interceptor puts it on the CallContext (node/auth.go) — so on loopback, where nobody
// is identified, List refuses rather than answering.
var _ taskstore.Store = (*taskStore)(nil)

// Create stores a task the request handler just built. The fingerprint written with it
// comes from the admission layer through the context, which is what makes the task
// record and the replay key land in ONE atomic file write.
func (s *taskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil || task.ID == "" {
		return taskstore.TaskVersionMissing, errors.New("node: cannot store a task without an id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.tasks[task.ID]; exists {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: %s", taskstore.ErrTaskAlreadyExists, task.ID)
	}

	caller := callerFrom(ctx)
	if strings.Contains(caller, dedupeKeySep) {
		return taskstore.TaskVersionMissing, fmt.Errorf(
			"node: caller identity %q contains the dedupe key separator %q", caller, dedupeKeySep)
	}

	now := time.Now().UTC()
	rec := &taskRecord{
		Task:             task,
		Version:          1,
		CallerID:         caller,
		SelfReportedName: selfReportedNameFrom(ctx),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if fp, ok := fingerprintFrom(ctx); ok {
		rec.MessageID = fp.messageID
		rec.Digest = fp.digest
	}
	if err := s.putLocked(rec); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(rec.Version), nil
}

// Update replaces a task's stored state after the version check.
func (s *taskStore) Update(ctx context.Context, update *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if update == nil || update.Task == nil || update.Task.ID == "" {
		return taskstore.TaskVersionMissing, errors.New("node: update without a task")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.tasks[update.Task.ID]
	if !ok {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: %s", a2a.ErrTaskNotFound, update.Task.ID)
	}
	// Version 0 means "no expectation" (TaskVersionMissing), which is what a caller that
	// never read a version can honestly say. Anything else has to match.
	if update.PrevVersion != taskstore.TaskVersionMissing && int64(update.PrevVersion) != rec.Version {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: task %s is at version %d, update expected %d",
			taskstore.ErrConcurrentModification, update.Task.ID, rec.Version, update.PrevVersion)
	}

	rec.Task = update.Task
	rec.Version++
	rec.UpdatedAt = time.Now().UTC()
	if err := s.putLocked(rec); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(rec.Version), nil
}

// Get returns a stored task by id — to its owner, and to nobody else.
//
// Ownership is checked here and not only in List, because an id is not a capability: the design's
// rule is that reading a task re-checks it (§6.3), and the refusal is TASK_NOT_FOUND rather than a
// permission error so that a caller cannot learn that somebody else's task exists at that id.
//
// A request with NO caller identity keeps the behaviour it had before this check existed: on
// loopback the transport is the boundary, and there is no identity to compare against. A record
// with no owner — one written on that surface — therefore matches no authenticated caller, which
// is what "nobody's" means.
func (s *taskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	rec, ok := s.record(taskID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", a2a.ErrTaskNotFound, taskID)
	}
	if caller := callerFrom(ctx); caller != "" && caller != rec.CallerID {
		return nil, fmt.Errorf("%w: %s", a2a.ErrTaskNotFound, taskID)
	}
	return &taskstore.StoredTask{
		Task:    rec.Task,
		Version: taskstore.TaskVersion(rec.Version),
		User:    rec.CallerID,
	}, nil
}

// List returns the tasks the caller owns, newest first.
//
// It refuses an unauthenticated caller outright: a list is a view of everything, and
// scoping it needs an owner to scope it to. Paging is by offset, so a caller that reads
// a page and asks for the next one gets the next one instead of a silently truncated
// answer.
func (s *taskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	caller := callerFrom(ctx)
	if caller == "" {
		return nil, a2a.ErrUnauthenticated
	}

	var matching []*taskRecord
	for _, rec := range s.snapshot() {
		if rec.CallerID != caller {
			continue
		}
		if req != nil && req.ContextID != "" && rec.Task.ContextID != req.ContextID {
			continue
		}
		if req != nil && req.Status != a2a.TaskStateUnspecified && rec.Task.Status.State != req.Status {
			continue
		}
		matching = append(matching, rec)
	}

	pageSize := listDefaultPageSize
	if req != nil && req.PageSize > 0 {
		pageSize = min(req.PageSize, listMaxPageSize)
	}
	offset := 0
	if req != nil && req.PageToken != "" {
		if _, err := fmt.Sscanf(req.PageToken, "%d", &offset); err != nil || offset < 0 {
			return nil, fmt.Errorf("%w: bad page token %q", a2a.ErrInvalidParams, req.PageToken)
		}
	}

	resp := &a2a.ListTasksResponse{TotalSize: len(matching), PageSize: pageSize}
	for i := offset; i < len(matching) && len(resp.Tasks) < pageSize; i++ {
		resp.Tasks = append(resp.Tasks, matching[i].Task)
	}
	if next := offset + len(resp.Tasks); next < len(matching) {
		resp.NextPageToken = fmt.Sprintf("%d", next)
	}
	return resp, nil
}

const (
	listDefaultPageSize = 50
	listMaxPageSize     = 100
)

// callerFrom is the caller's identity as the authentication layer established it.
//
// It reads the CallContext and nothing else: a caller cannot name itself in a message or
// in metadata, which is the whole point of the design's "调用者身份取自认证和服务端绑定". The
// node's TLS handshake is what fills it (node/auth.go), out of the certificate the caller
// presented; a request that never went through that handshake has no identity, and the
// emptiness is what makes an unauthenticated list impossible rather than merely discouraged.
func callerFrom(ctx context.Context) string {
	callCtx, ok := a2asrv.CallContextFrom(ctx)
	if !ok || callCtx == nil || callCtx.User == nil {
		return ""
	}
	return callCtx.User.Name
}
