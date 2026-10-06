package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Failure streaks that make a refusal tell the model to stop. A small model
// that is refused tends to send the identical call again, or to try call after
// call until a client-side iteration cap ends the turn with no reply at all.
const (
	repeatFailLimit = 2 // identical failing calls in a row
	anyFailLimit    = 3 // failing calls in a row, whatever they were
)

// failStreak is one caller's run of failed tool calls.
type failStreak struct {
	lastKey  string
	identity int // consecutive failures of lastKey
	total    int // consecutive failures of any call
}

// loopBreaker counts consecutive failing tools/call replies per caller and
// appends an order to stop to the reply once a streak reaches a limit. A
// successful call ends the streak.
type loopBreaker struct {
	mu      sync.Mutex
	streaks map[string]*failStreak
}

func newLoopBreaker() *loopBreaker {
	return &loopBreaker{streaks: map[string]*failStreak{}}
}

// record notes the outcome of a call and returns the warning to append to a
// failing reply, or "".
func (b *loopBreaker) record(owner, key string, failed bool) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.streaks[owner]
	if s == nil {
		s = &failStreak{}
		b.streaks[owner] = s
	}
	if !failed {
		*s = failStreak{}
		return ""
	}
	if key == s.lastKey {
		s.identity++
	} else {
		s.lastKey, s.identity = key, 1
	}
	s.total++
	switch {
	case s.identity >= repeatFailLimit:
		return fmt.Sprintf("STOP: this exact call has now failed %d times in a row and will fail the same way every time. Do not send it again. Change the arguments as the error says, use a different tool, or write your reply to the user saying what you retrieved and what failed.", s.identity)
	case s.total >= anyFailLimit:
		return fmt.Sprintf("STOP: %d calls in a row have failed. Do not call another tool for this step: write your reply to the user saying what you retrieved, what failed and what they could ask instead.", s.total)
	}
	return ""
}

// breakFailureLoops is receiving middleware that adds the loopBreaker's
// warning to the text of a failing tools/call reply.
func breakFailureLoops(cfg *config) mcp.Middleware {
	b := newLoopBreaker()
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != methodCallTool {
				return res, err
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return res, err
			}
			owner, _ := callerOf(cfg, &mcp.CallToolRequest{Extra: req.GetExtra()})
			out, _ := res.(*mcp.CallToolResult)
			failed := err != nil || (out != nil && out.IsError)
			warn := b.record(owner, params.Name+" "+string(params.Arguments), failed)
			if warn != "" && out != nil && len(out.Content) > 0 {
				if tc, ok := out.Content[0].(*mcp.TextContent); ok {
					tc.Text += "\n" + warn
				}
			}
			return res, err
		}
	}
}
