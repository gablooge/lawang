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
