package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

const rawUpstreamError = `API request failed with status 429: {"error":"key sk-secret at https://llm.internal/v1"}`

func TestFailedTurnText(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want string
	}{
		{"rate limit", rawUpstreamError, failedTurnBusyText},
		{"gateway wrapped rate limit", "API request failed with status 503: upstream rate limit [429]", failedTurnBusyText},
		{"plain unavailable", "API request failed with status 503: no healthy upstream", failedTurnBusyText},
		{"quota", "API request failed with status 403: insufficient_quota", failedTurnBusyText},
		{"overloaded", "Overloaded, try later", failedTurnBusyText},
		{"server error", "API request failed with status 500: boom", failedTurnGenericText},
		{"stream ended early", types.StreamEndedEarlyError, failedTurnGenericText},
		{"empty", "", failedTurnGenericText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failedTurnText(tt.err)
			require.Equal(t, tt.want, got)
			require.NotContains(t, got, "sk-secret")
			require.NotContains(t, got, "https://")
		})
	}
}

func emitAgentTurnEnd(
	t *testing.T, ctx context.Context, bus *event.EventBus, errFirst bool, errMsg string,
) {
	t.Helper()
	complete := func() {
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentComplete, Data: event.AgentCompleteData{MessageID: "m"},
		}))
	}
	fail := func() {
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventError,
			Data: event.ErrorData{Error: errMsg, Stage: "agent_execution"},
		}))
	}
	if errFirst {
		fail()
		complete()
		return
	}
	// The engine completes the turn before it reports the error.
	complete()
	fail()
}

func TestAgentTurnErrorIsPersistedAsFailed(t *testing.T) {
	for _, errFirst := range []bool{false, true} {
		for _, tt := range []struct {
			name, partial, want string
		}{
			{"empty answer gets notice", "", failedTurnBusyText},
			{"partial answer is kept", "partial answer", "partial answer"},
		} {
			name := tt.name + map[bool]string{false: " complete first", true: " error first"}[errFirst]
			t.Run(name, func(t *testing.T) {
				messages := &imageCompletionMessages{}
				stream := &imageCompletionStream{}
				h := &Handler{messageService: messages, streamManager: stream}
				bus := event.NewEventBus()
				message := &types.Message{ID: "m", SessionID: "s", Role: "assistant", Content: tt.partial}
				ctx := types.WithExecutionTenant(context.Background(), 1)
				sh := h.setupStreamHandler(ctx, "s", "m", "req", 1, time.Now(), message, bus)

				emitAgentTurnEnd(t, ctx, bus, errFirst, rawUpstreamError)
				h.completeStreamAssistantMessage(ctx, &sseStreamContext{
					eventBus: bus, streamHandler: sh, assistantMessage: message,
				}, "the question", "u1")

				require.NotNil(t, messages.saved)
				require.Equal(t, types.MessageRunStateFailed, messages.saved.RunState)
				require.True(t, messages.saved.IsCompleted)
				require.Equal(t, tt.want, messages.saved.Content)
				require.NotContains(t, messages.saved.Content, "sk-secret")

				done := stream.events[len(stream.events)-1]
				require.Equal(t, types.ResponseTypeComplete, done.Type)
				require.Equal(t, tt.want, done.Data["final_content"])
			})
		}
	}
}

func TestAgentTurnWithoutErrorKeepsRunStateEmpty(t *testing.T) {
	messages := &imageCompletionMessages{}
	h := &Handler{messageService: messages, streamManager: &imageCompletionStream{}}
	bus := event.NewEventBus()
	message := &types.Message{ID: "m", SessionID: "s", Role: "assistant"}
	ctx := types.WithExecutionTenant(context.Background(), 1)
	sh := h.setupStreamHandler(ctx, "s", "m", "req", 1, time.Now(), message, bus)
	require.NoError(t, bus.Emit(ctx, event.Event{
		Type: event.EventAgentComplete, Data: event.AgentCompleteData{MessageID: "m", FinalAnswer: "ok"},
	}))
	h.completeStreamAssistantMessage(ctx, &sseStreamContext{
		eventBus: bus, streamHandler: sh, assistantMessage: message,
	}, "", "")
	require.Equal(t, "", messages.saved.RunState)
	require.Equal(t, "ok", messages.saved.Content)
}

// A user stop cancels the run context and the engine then reports the
// cancellation as an error; that must not turn the stopped turn into a failure.
func TestStoppedAgentTurnIsNotFailed(t *testing.T) {
	messages := &imageCompletionMessages{}
	h := &Handler{messageService: messages, streamManager: &imageCompletionStream{}}
	bus := event.NewEventBus()
	message := &types.Message{ID: "m", SessionID: "s", Role: "assistant", Content: "so far"}
	runCtx, cancel := context.WithCancel(types.WithExecutionTenant(context.Background(), 1))
	sh := h.setupStreamHandler(runCtx, "s", "m", "req", 1, time.Now(), message, bus)
	cancel()

	emitAgentTurnEnd(t, context.Background(), bus, false, "context canceled")
	saveCtx := types.WithExecutionTenant(context.Background(), 1)
	h.completeStreamAssistantMessage(saveCtx, &sseStreamContext{
		eventBus: bus, streamHandler: sh, assistantMessage: message,
	}, "", "")

	require.NotNil(t, messages.saved)
	require.Equal(t, "", messages.saved.RunState)
	require.Equal(t, "so far", messages.saved.Content)
}

func newQuickAnswerFailureFixture(t *testing.T, asyncCtx context.Context) (
	*Handler, *imageCompletionMessages, *event.EventBus, *types.Message, *atomic.Bool,
) {
	t.Helper()
	messages := &imageCompletionMessages{}
	h := &Handler{messageService: messages, streamManager: &imageCompletionStream{}}
	bus := event.NewEventBus()
	message := &types.Message{ID: "m", SessionID: "s", Role: "assistant"}
	sh := h.setupStreamHandler(asyncCtx, "s", "m", "req", 1, time.Now(), message, bus)
	streamCtx := &sseStreamContext{
		eventBus: bus, asyncCtx: asyncCtx, streamHandler: sh, assistantMessage: message,
	}
	handled := &atomic.Bool{}
	h.registerQuickAnswerErrorCompletion(streamCtx, 1, "the question", "u1", handled)
	return h, messages, bus, message, handled
}

func TestQuickAnswerErrorIsPersistedAsFailed(t *testing.T) {
	ctx := types.WithExecutionTenant(context.Background(), 1)
	_, messages, bus, message, handled := newQuickAnswerFailureFixture(t, ctx)

	require.NoError(t, bus.Emit(ctx, event.Event{
		Type: event.EventError,
		Data: event.ErrorData{Error: rawUpstreamError, Stage: "chat_completion_stream"},
	}))

	require.NotNil(t, messages.saved)
	require.Equal(t, types.MessageRunStateFailed, messages.saved.RunState)
	require.True(t, messages.saved.IsCompleted)
	require.Equal(t, failedTurnBusyText, messages.saved.Content)
	require.True(t, handled.Load())
	require.Equal(t, types.MessageRunStateFailed, message.RunState)
}

func TestQuickAnswerErrorKeepsPartialAnswer(t *testing.T) {
	ctx := types.WithExecutionTenant(context.Background(), 1)
	_, messages, bus, message, _ := newQuickAnswerFailureFixture(t, ctx)
	message.Content = "partial answer"

	require.NoError(t, bus.Emit(ctx, event.Event{
		Type: event.EventError, Data: event.ErrorData{Error: "boom", Stage: "chat_completion_stream"},
	}))

	require.Equal(t, types.MessageRunStateFailed, messages.saved.RunState)
	require.Equal(t, "partial answer", messages.saved.Content)
}

func TestQuickAnswerErrorAfterCompletionDoesNotPersistAgain(t *testing.T) {
	ctx := types.WithExecutionTenant(context.Background(), 1)
	_, messages, bus, _, handled := newQuickAnswerFailureFixture(t, ctx)
	handled.Store(true)

	require.NoError(t, bus.Emit(ctx, event.Event{
		Type: event.EventError, Data: event.ErrorData{Error: "boom"},
	}))
	require.Nil(t, messages.saved)
}

func TestStoppedQuickAnswerTurnIsNotFailed(t *testing.T) {
	base := types.WithExecutionTenant(context.Background(), 1)
	asyncCtx, cancel := context.WithCancel(base)
	_, messages, bus, message, handled := newQuickAnswerFailureFixture(t, asyncCtx)
	cancel()

	require.NoError(t, bus.Emit(base, event.Event{
		Type: event.EventError, Data: event.ErrorData{Error: "context canceled"},
	}))

	require.Nil(t, messages.saved, "the stop path persists the row, not the error path")
	require.False(t, handled.Load())
	require.Equal(t, "", message.RunState)
}
