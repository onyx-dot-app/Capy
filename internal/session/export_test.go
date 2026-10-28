package session

import "time"

// SetHeartbeatEvery sets how often sessions heartbeat, until the returned
// func puts it back.
func SetHeartbeatEvery(d time.Duration) (restore func()) {
	was := heartbeatEvery
	heartbeatEvery = d
	return func() { heartbeatEvery = was }
}

// SetSummarizeRetryWait sets the wait before Summarizer's first retry,
// until the returned func puts it back.
func SetSummarizeRetryWait(d time.Duration) (restore func()) {
	was := summarizeRetryWait
	summarizeRetryWait = d
	return func() { summarizeRetryWait = was }
}

// SetHighlightTiming sets how long a name line's check waits, how soon
// after the last check it may start, and the last check's budget, until
// the returned func puts them back.
func SetHighlightTiming(nameDelay, gap, budget time.Duration) (restore func()) {
	was := [3]time.Duration{nameCheckDelay, minCheckGap, finalCheckBudget}
	nameCheckDelay, minCheckGap, finalCheckBudget = nameDelay, gap, budget
	return func() { nameCheckDelay, minCheckGap, finalCheckBudget = was[0], was[1], was[2] }
}
