package outbox

import (
	"testing"
	"time"
)

func TestLadderNext(t *testing.T) {
	l := Ladder{time.Second, time.Minute}
	tests := []struct {
		attempts int
		want     time.Duration
		ok       bool
	}{
		{1, time.Second, true},
		{2, time.Minute, true},
		{3, 0, false}, // used up: dead letter
		{0, 0, false}, // a caller bug must not become a free retry
		{-1, 0, false},
	}
	for _, tt := range tests {
		got, ok := l.Next(tt.attempts)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Next(%d) = %v, %v, want %v, %v", tt.attempts, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := (Ladder{}).Next(1); ok {
		t.Error("an empty ladder offered a retry")
	}
}

func TestDefaultLadderOnlyClimbs(t *testing.T) {
	for i := 1; i < len(DefaultLadder); i++ {
		if DefaultLadder[i] <= DefaultLadder[i-1] {
			t.Errorf("step %d (%v) does not exceed step %d (%v)", i, DefaultLadder[i], i-1, DefaultLadder[i-1])
		}
	}
}
