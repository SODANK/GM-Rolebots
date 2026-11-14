package rolebots

import "time"

// Clock is the abstract tier's time source. The test harness swaps in a fake clock so a
// simulated week runs in seconds. See docs/comment-notes-b02.md#clock.
type Clock interface {
	Now() time.Time
	Unix() int64
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Unix() int64    { return time.Now().Unix() }

// clock is the abstract tier's time source. Written exactly once, by the
// harness, before any bot goroutine exists; never written in production.
var clock Clock = realClock{}
