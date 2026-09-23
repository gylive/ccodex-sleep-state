package turnstate

import (
	"encoding/base64"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func synthetic(blocks int, at time.Time, marker byte) string {
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(at.Unix()))
	raw[9] = marker
	return base64.URLEncoding.EncodeToString(raw)
}
func TestEnvelopePolicy(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	policy := Policy{[]int{10}, time.Hour, 20 * time.Minute}
	for _, tc := range []struct {
		name   string
		blocks int
		age    time.Duration
		want   bool
	}{
		{"baseline", 10, 0, true}, {"extra block is not baseline", 11, 0, false}, {"team shape rejected", 12, 0, false}, {"expired", 10, 2 * time.Hour, false}, {"nearly expired", 10, time.Hour - 20*time.Second, false}, {"future", 10, -time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := Parse(synthetic(tc.blocks, now.Add(-tc.age), 0))
			if err != nil {
				t.Fatal(err)
			}
			if got := policy.Accept(token, now); got != tc.want {
				t.Fatalf("Accept=%v, want %v", got, tc.want)
			}
		})
	}
	for _, value := range []string{"", "plain", "AAAA", "a===", "gAAAA\r\nHeader: value", synthetic(10, now, 0) + "="} {
		if _, err := Parse(value); err == nil {
			t.Errorf("invalid token accepted")
		}
	}
}
func TestReadyPromotionAndRequestSnapshot(t *testing.T) {
	now := time.Now()
	s := New(Policy{[]int{10}, time.Hour, 20 * time.Minute})
	a, _ := Parse(synthetic(10, now, 1))
	b, _ := Parse(synthetic(10, now.Add(time.Second), 2))
	if !s.Offer(a, 0, now) {
		t.Fatal("offer active")
	}
	snapshot, _ := s.Acquire(now)
	if !s.Offer(b, 1, now) {
		t.Fatal("offer ready")
	}
	current, _ := s.Acquire(now)
	if current.Token.Value != a.Value {
		t.Fatal("healthy active was overwritten")
	}
	suspect := synthetic(11, now, 3)
	if !s.Observe(suspect, snapshot, now) || !s.Observe(suspect, snapshot, now) {
		t.Fatal("expected shape signal")
	}
	promoted, _ := s.Acquire(now)
	if promoted.Token.Value != b.Value || promoted.Route != 1 || promoted.Version <= snapshot.Version {
		t.Fatal("ready not promoted")
	}
	if snapshot.Token.Value != a.Value {
		t.Fatal("request snapshot mutated")
	}
	s.Observe(suspect, snapshot, now)
	if s.Status(now).Strikes != 0 {
		t.Fatal("stale response affected new version")
	}
}
func TestObservationCannotPublish(t *testing.T) {
	now := time.Now()
	s := New(Policy{[]int{10}, time.Hour, 20 * time.Minute})
	s.Observe(synthetic(10, now, 1), Snapshot{}, now)
	if _, ok := s.Acquire(now); ok {
		t.Fatal("response observation became active")
	}
	expired, _ := Parse(synthetic(10, now.Add(-2*time.Hour), 0))
	if s.Offer(expired, 0, now) {
		t.Fatal("expired candidate accepted")
	}
}
func TestStoreConcurrent(t *testing.T) {
	now := time.Now()
	s := New(Policy{[]int{10}, time.Hour, 20 * time.Minute})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token, _ := Parse(synthetic(10, now, byte(i)))
			s.Offer(token, i, now)
			snap, _ := s.Acquire(now)
			s.Observe(token.Value, snap, now)
			s.NeedsRefresh(now)
			s.Status(now)
		}(i)
	}
	wg.Wait()
}
func FuzzParse(f *testing.F) {
	f.Add("not-a-token")
	f.Add(synthetic(10, time.Unix(1800000000, 0), 0))
	f.Add(synthetic(33, time.Unix(1800000000, 0), 0))
	f.Fuzz(func(t *testing.T, value string) {
		token, err := Parse(value)
		if err == nil && (token.Blocks < 1 || token.Value == "") {
			t.Fatal("invalid parsed token")
		}
	})
}
