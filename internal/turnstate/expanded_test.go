package turnstate

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestExpandedEnvelopePolicy(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	value := synthetic(33, now, 1)
	if len(value) != 780 {
		t.Fatalf("expanded state length = %d, want 780", len(value))
	}
	for _, baseline := range []int{10, 12} {
		policy := Policy{[]int{baseline, 33}, time.Hour, 20 * time.Minute}
		for _, tc := range []struct {
			name   string
			blocks int
			age    time.Duration
			want   bool
		}{
			{"legacy", baseline, 0, true},
			{"expanded", 33, 0, true},
			{"legacy extra block", baseline + 1, 0, false},
			{"shorter unknown", 32, 0, false},
			{"longer unknown", 34, 0, false},
			{"expired expanded", 33, 2 * time.Hour, false},
			{"expiry margin", 33, time.Hour - 30*time.Second, false},
			{"future expanded", 33, -time.Minute, false},
		} {
			token, err := Parse(synthetic(tc.blocks, now.Add(-tc.age), 1))
			if err != nil {
				t.Fatal(err)
			}
			if got := policy.Accept(token, now); got != tc.want {
				t.Errorf("baseline=%d %s: Accept=%v, want %v", baseline, tc.name, got, tc.want)
			}
		}
	}
	for _, mutate := range []func([]byte) []byte{
		func(raw []byte) []byte { raw[0] = 0; return raw },
		func(raw []byte) []byte { clear(raw[1:9]); return raw },
		func(raw []byte) []byte { return raw[:len(raw)-1] },
	} {
		raw, _ := base64.URLEncoding.DecodeString(value)
		if _, err := Parse(base64.URLEncoding.EncodeToString(mutate(raw))); err == nil {
			t.Error("malformed expanded envelope accepted")
		}
	}
}

func TestExpandedStatePromotesWithoutMutatingSnapshot(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, baseline := range []int{10, 12} {
		s := New(Policy{[]int{baseline, 33}, time.Hour, 20 * time.Minute})
		legacy, _ := Parse(synthetic(baseline, now.Add(-45*time.Minute), 1))
		expanded, _ := Parse(synthetic(33, now, 2))
		if !s.Offer(legacy, 0, now) {
			t.Fatal("legacy state rejected")
		}
		before, _ := s.Acquire(now)
		if s.Observe(expanded.Value, before, now) || s.Status(now).Strikes != 0 {
			t.Fatal("expanded response incorrectly marked suspect")
		}
		if !s.Offer(expanded, 1, now) {
			t.Fatal("expanded replacement rejected")
		}
		after, usable := s.Acquire(now)
		if !usable || after.Token.Value != expanded.Value || after.Route != 1 || after.Version <= before.Version {
			t.Fatal("expanded replacement was not promoted")
		}
		if before.Token.Value != legacy.Value {
			t.Fatal("in-flight snapshot mutated")
		}
	}
}
