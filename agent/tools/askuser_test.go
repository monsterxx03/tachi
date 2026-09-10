package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuestionUnmarshalMultiSelect pins the dual spelling: the tool schema
// advertises "multiSelect" and that is what models emit, while the struct
// marshals "multi_select". Reading only one of them silently downgraded every
// question to single-select (the flag was dropped by encoding/json).
func TestQuestionUnmarshalMultiSelect(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"schema spelling true", `{"question":"q","header":"h","multiSelect":true,"options":[{"label":"A","description":"a"}]}`, true},
		{"schema spelling false", `{"question":"q","header":"h","multiSelect":false}`, false},
		{"struct spelling true", `{"question":"q","header":"h","multi_select":true}`, true},
		{"struct spelling false", `{"question":"q","header":"h","multi_select":false}`, false},
		{"absent", `{"question":"q","header":"h"}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var q Question
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &q))
			assert.Equal(t, tc.want, q.MultiSelect)
			assert.Equal(t, "q", q.Question)
			assert.Equal(t, "h", q.Header)
		})
	}
}

// TestQuestionUnmarshalKeepsOptions makes sure the custom unmarshaller does not
// drop the rest of the payload.
func TestQuestionUnmarshalKeepsOptions(t *testing.T) {
	var q Question
	require.NoError(t, json.Unmarshal([]byte(`{"question":"q","header":"h","multiSelect":true,"options":[{"label":"A","description":"da","preview":"pa"}]}`), &q))

	require.Len(t, q.Options, 1)
	assert.Equal(t, "A", q.Options[0].Label)
	assert.Equal(t, "da", q.Options[0].Description)
	assert.Equal(t, "pa", q.Options[0].Preview)
}

// TestAskUserToolParsesModelPayload covers the model payload end to end: the
// flag must survive into the question the frontends receive.
func TestAskUserToolParsesModelPayload(t *testing.T) {
	_, err := AskUserTool{}.ExecuteContext(context.Background(),
		`{"questions":[{"question":"喜欢哪些?","header":"元素","multiSelect":true,"options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`)

	var askErr *AskUserQuestionError
	require.True(t, errors.As(err, &askErr), "expected an AskUserQuestionError, got %v", err)
	require.Len(t, askErr.Questions, 1)
	assert.True(t, askErr.Questions[0].MultiSelect, "multiSelect must survive the tool boundary")
}

// TestBuildElicitationSchemaDegradesMultiSelect documents the ACP limit: form
// schemas admit only flat objects with primitive/enum properties, so a
// multi-select question is offered as a single choice there (never as an
// out-of-spec array).
func TestBuildElicitationSchemaDegradesMultiSelect(t *testing.T) {
	schema := buildElicitationSchema([]Question{{
		Question:    "喜欢哪些?",
		Header:      "元素",
		MultiSelect: true,
		Options:     []QuestionOption{{Label: "A", Description: "a"}, {Label: "B", Description: "b"}},
	}})

	prop, ok := schema.Properties["question_0"].(map[string]any)
	require.True(t, ok, "expected a property for question_0")
	assert.Equal(t, "string", prop["type"])
	assert.NotContains(t, prop, "items", "an array schema would violate the elicitation spec")
	_, hasOneOf := prop["oneOf"]
	assert.True(t, hasOneOf)
}
