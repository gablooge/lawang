package outbox

import "time"

// Ladder is the retry schedule: element i is the wait after attempt i+1 fails.
type Ladder []time.Duration

// DefaultLadder gives a failing sink about a day to come back before a row is parked.
var DefaultLadder = Ladder{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	time.Hour,
	6 * time.Hour,
	12 * time.Hour,
}

// Next returns how long to wait after the given number of attempts have failed. ok is false when
// the ladder is used up and the row should become a dead letter. An attempt count below 1 is a
// caller bug, and is answered with "give up" so it cannot turn into a free retry loop.
func (l Ladder) Next(attempts int) (delay time.Duration, ok bool) {
	if attempts < 1 || attempts > len(l) {
		return 0, false
	}
	return l[attempts-1], true //nolint:gosec // G602: bounds checked on the line above
}

// Exhausted reports an attempt count that this ladder can no longer account for, which is one
// past the last attempt it schedules.
//
// The last attempt the ladder allows is len(l)+1: Next schedules a wait after each of the first
// len(l) failures, and the attempt after the last of those is the one Fail dead-letters. So a
// claim numbered higher than that is a row that was claimed again although the ladder was over,
// which can only mean that no attempt ever reached Fail. attempts is incremented by the claim
// and read by nothing else, so a worker that dies or hangs leaves exactly this trace and no
// other (Outbox.MarkAbandoned).
//
// A halt is not such an attempt and never trips this: Halt gives the attempt back.
func (l Ladder) Exhausted(attempts int) bool { return attempts > len(l)+1 }
