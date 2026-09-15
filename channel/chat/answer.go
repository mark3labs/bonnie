package chat

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/bonnie/runtime"
)

// Choice is one answer a parked run accepts, ready to be drawn as a button,
// a keyboard key, or a Block Kit action.
//
// A chat platform gives a person two ways to answer a question: type prose,
// or press something. Prose is what every adapter already supported, and it
// is the worse one for a decision — an approval answered with "sure, go
// ahead" has to be interpreted, and the agent that asked "may I?" deserves a
// verdict rather than a reading of someone's tone.
type Choice struct {
	// Label is what the person reads on the control.
	Label string
	// Token is what the platform sends back when the control is used. It
	// is opaque to the platform and meaningful only to [Answer].
	Token string
}

// Limits every surface has to live inside. The smallest wins, so one token
// works everywhere and an adapter never has to think about it:
//
//   - Telegram caps `callback_data` at 64 bytes and REJECTS the message
//     outright if a key exceeds it.
//   - Discord allows 100 characters of `custom_id` and 80 of button label.
//   - Slack allows far more of both.
const (
	tokenLimit = 64
	labelLimit = 80
)

// The selectors a token carries. They are one letter because the budget is
// 64 bytes and a tool-call ID takes most of it.
const (
	selectApprove = "a"
	selectReject  = "r"
	selectOption  = "o" // followed by the option's index
	tokenSep      = "|"
)

// Choices returns the answers a suspended run accepts, or nil when the run
// is not parked or wants prose.
//
// The suspension's Kind decides, not its Options. [runtime.ApprovalTool]
// sets both — Kind approval AND Options ["approve", "reject"] — and reading
// the options there would turn a click into the TEXT "approve" instead of
// the verdict the runtime has a field for. An approval is a binary with a
// fixed vocabulary (see renderResponses), so it gets exactly two controls
// whatever words the model chose to restate them with.
//
// A question with options gets one control per option, answering with that
// option's text. A question without options gets nil: there is nothing to
// draw, and the person types.
//
// It returns nil rather than a partial set when any token would exceed
// [tokenLimit]. Half the options on screen is worse than none, because the
// person cannot tell that the rest exist.
func Choices(run *runtime.Run) []Choice {
	if run == nil || run.State != runtime.RunWaiting || run.Suspend == nil {
		return nil
	}
	sus := run.Suspend

	var out []Choice
	switch {
	case sus.Kind == runtime.SuspendApproval:
		out = []Choice{
			{Label: "Approve", Token: token(sus, selectApprove)},
			{Label: "Reject", Token: token(sus, selectReject)},
		}
	case len(sus.Options) > 0:
		for i, opt := range sus.Options {
			out = append(out, Choice{
				Label: truncateLabel(opt),
				Token: token(sus, selectOption+strconv.Itoa(i)),
			})
		}
	default:
		return nil
	}

	for _, c := range out {
		if len(c.Token) > tokenLimit {
			return nil
		}
	}
	return out
}

// Answer resolves a token back into the responses that resume the run, and
// reports whether the token belongs to this suspension.
//
// The tool-call ID in the token is checked against the one the run is parked
// on. That is the staleness guard, and it is the reason the ID is in there:
// the controls of an answered question stay on screen in a chat history, and
// an index alone would resolve against whatever the run is parked on NOW.
// Someone scrolling back and pressing "Approve" on last week's question must
// not approve this week's.
//
// It reports false for a token from another suspension, a malformed one, and
// an index that no longer names an option. A caller tells the person the
// question has moved on rather than guessing which answer was meant.
func Answer(sus *runtime.SuspendRequest, tok string) ([]runtime.InputResponse, bool) {
	if sus == nil {
		return nil, false
	}
	id, sel, ok := strings.Cut(tok, tokenSep)
	if !ok || id != sus.ToolCallID {
		return nil, false
	}
	switch {
	case sus.Kind == runtime.SuspendApproval && sel == selectApprove:
		return []runtime.InputResponse{runtime.Approve("")}, true
	case sus.Kind == runtime.SuspendApproval && sel == selectReject:
		return []runtime.InputResponse{runtime.Reject("")}, true
	case strings.HasPrefix(sel, selectOption):
		i, err := strconv.Atoi(strings.TrimPrefix(sel, selectOption))
		if err != nil || i < 0 || i >= len(sus.Options) {
			return nil, false
		}
		return []runtime.InputResponse{{Text: sus.Options[i]}}, true
	}
	return nil, false
}

// token builds one control's token: the halting tool call, then what was
// chosen.
func token(sus *runtime.SuspendRequest, selector string) string {
	return sus.ToolCallID + tokenSep + selector
}

// truncateLabel fits a label inside what a platform will draw. The cut is on
// a rune boundary, because half a rune is not a character.
func truncateLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= labelLimit {
		return s
	}
	cut := s[:labelLimit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
