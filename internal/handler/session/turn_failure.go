package session

import (
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/api"
)

const (
	// failedTurnBusyText is shown when the model service is rate limited,
	// out of quota or overloaded: the user can succeed by trying again later.
	failedTurnBusyText = "Hệ thống đang bận, vui lòng thử lại sau ít phút."
	// failedTurnGenericText is shown for every other failure.
	failedTurnGenericText = "Trợ lý không trả lời được câu hỏi này do lỗi hệ thống. Vui lòng thử lại."
)

// busyStatusPattern finds a 429 or 503 status in the flattened text of an
// upstream error (for example "API request failed with status 503: ...").
var busyStatusPattern = regexp.MustCompile(`status (?:429|503)\b`)

// failedTurnText maps the error that ended a turn to the short notice stored
// on the assistant message. It never echoes the error: upstream bodies can
// carry URLs, keys and provider details that must not reach the user or the
// database.
func failedTurnText(errMsg string) string {
	lower := strings.ToLower(errMsg)
	if api.IsRateLimitErrorText(errMsg) ||
		busyStatusPattern.MatchString(lower) ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "overloaded") {
		return failedTurnBusyText
	}
	return failedTurnGenericText
}
