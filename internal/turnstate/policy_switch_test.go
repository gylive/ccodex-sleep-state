package turnstate

import (
	"testing"
	"time"
)

func TestHoldActiveUntilRejectedOrExpired(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := New(Policy{[]int{10}, time.Hour, 20 * time.Minute})
	a, _ := Parse(synthetic(10, now.Add(-45*time.Minute), 1))
	b, _ := Parse(synthetic(10, now, 2))
	s.Offer(a, 0, now)
	used, _ := s.Acquire(now)
	s.HoldActive(true)
	s.Offer(b, 1, now)
	held, _ := s.Acquire(now)
	if held.Token.Value != a.Value {
		t.Fatal("hold strategy rotated early")
	}
	if !s.RejectAndPromote(used, now) {
		t.Fatal("ready not promoted")
	}
	next, _ := s.Acquire(now)
	if next.Token.Value != b.Value {
		t.Fatal("wrong standby")
	}
	if s.RejectAndPromote(used, now) {
		t.Fatal("stale failure invalidated newer active")
	}
	current, _ := s.Acquire(now)
	if current.Token.Value != b.Value {
		t.Fatal("new state lost")
	}
}
func TestHoldDoesNotExtendExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := New(Policy{[]int{10}, time.Hour, 20 * time.Minute})
	a, _ := Parse(synthetic(10, now, 1))
	s.Offer(a, 0, now)
	s.HoldActive(true)
	if _, ok := s.Acquire(now.Add(time.Hour)); ok {
		t.Fatal("expired state accepted")
	}
}
