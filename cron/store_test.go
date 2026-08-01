package cron

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")
	return NewStore(path), path
}

func TestStore_Create(t *testing.T) {
	store, _ := newTestStore(t)

	job := &Job{
		ID:       "cr_test1",
		Name:     "Test Job",
		Schedule: "0 9 * * *",
		Prompt:   "Hello",
	}
	err := store.Create(job)
	require.NoError(t, err)

	// Verify the job was persisted.
	got, err := store.Get("cr_test1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Test Job", got.Name)
	assert.Equal(t, "0 9 * * *", got.Schedule)
	assert.False(t, got.CreatedAt.IsZero())
	assert.False(t, got.UpdatedAt.IsZero())
}

func TestStore_CreateDuplicate(t *testing.T) {
	store, _ := newTestStore(t)

	job := &Job{ID: "cr_test1", Name: "Job 1", Schedule: "@daily", Prompt: "Test"}
	require.NoError(t, store.Create(job))

	err := store.Create(job)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestStore_List(t *testing.T) {
	store, _ := newTestStore(t)

	jobs := []*Job{
		{ID: "cr_a", Name: "A", Schedule: "@daily", Prompt: "A"},
		{ID: "cr_b", Name: "B", Schedule: "@hourly", Prompt: "B"},
	}
	for _, j := range jobs {
		require.NoError(t, store.Create(j))
	}

	listed, err := store.List()
	require.NoError(t, err)
	assert.Len(t, listed, 2)
}

func TestStore_Update(t *testing.T) {
	store, _ := newTestStore(t)

	job := &Job{ID: "cr_test1", Name: "Original", Schedule: "@daily", Prompt: "Test"}
	require.NoError(t, store.Create(job))

	job.Name = "Updated"
	err := store.Update(job)
	require.NoError(t, err)

	got, err := store.Get("cr_test1")
	require.NoError(t, err)
	assert.Equal(t, "Updated", got.Name)
}

func TestStore_UpdateNonExistent(t *testing.T) {
	store, _ := newTestStore(t)
	err := store.Update(&Job{ID: "cr_nonexistent"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStore_Delete(t *testing.T) {
	store, _ := newTestStore(t)

	job := &Job{ID: "cr_test1", Name: "Job", Schedule: "@daily", Prompt: "Test"}
	require.NoError(t, store.Create(job))

	err := store.Delete("cr_test1")
	require.NoError(t, err)

	got, err := store.Get("cr_test1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_DeleteNonExistent(t *testing.T) {
	store, _ := newTestStore(t)
	err := store.Delete("cr_nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStore_Persistence(t *testing.T) {
	store, path := newTestStore(t)

	job := &Job{
		ID:             "cr_test1",
		Name:           "Persistent",
		Schedule:       "@daily",
		Prompt:         "Test prompt",
		Status:         JobStatusActive,
		TargetType:     "channel",
		TargetThreadID: "wxid:user1",
	}
	require.NoError(t, store.Create(job))

	// Reopen from the same file.
	store2 := NewStore(path)
	got, err := store2.Get("cr_test1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Persistent", got.Name)
	assert.Equal(t, "Test prompt", got.Prompt)
	assert.Equal(t, JobStatusActive, got.Status)
}

func TestStore_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")

	// Don't create a file at all — the store should handle missing files.
	store := NewStore(path)
	jobs, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, jobs)
}

func TestGenerateID(t *testing.T) {
	id := GenerateID()
	assert.True(t, len(id) > 3)
	assert.Equal(t, "cr_", id[:3])
}
