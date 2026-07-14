package cron

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEffectiveNotify(t *testing.T) {
	tests := []struct {
		name   string
		notify NotifyPolicy
		want   NotifyPolicy
	}{
		{"empty defaults to always", "", NotifyAlways},
		{"explicit always", NotifyAlways, NotifyAlways},
		{"when_relevant", NotifyWhenRelevant, NotifyWhenRelevant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Notify: tt.notify}
			assert.Equal(t, tt.want, job.EffectiveNotify())
		})
	}
}

func TestShouldSuppressResult(t *testing.T) {
	tests := []struct {
		name     string
		notify   NotifyPolicy
		result   string
		suppress bool
	}{
		// notify=always: never suppress
		{"always + content", NotifyAlways, "blog updated!", false},
		{"always + empty", NotifyAlways, "", false},
		{"always + silent marker", NotifyAlways, "[SILENT]", false},

		// notify=when_relevant: suppress on SILENT or empty
		{"when_relevant + SILENT", NotifyWhenRelevant, "[SILENT]", true},
		{"when_relevant + SILENT with whitespace", NotifyWhenRelevant, "  [SILENT]  \n", true},
		{"when_relevant + empty", NotifyWhenRelevant, "", true},
		{"when_relevant + whitespace only", NotifyWhenRelevant, "   \n  ", true},
		{"when_relevant + content", NotifyWhenRelevant, "Blog has a new post: ...", false},
		{"when_relevant + content with SILENT inside", NotifyWhenRelevant, "Result: [SILENT] not really", true},

		// empty notify (defaults to always): never suppress
		{"default + silent marker", "", "[SILENT]", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Notify: tt.notify}
			assert.Equal(t, tt.suppress, job.ShouldSuppressResult(tt.result))
		})
	}
}
