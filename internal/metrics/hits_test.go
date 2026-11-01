package metrics

import (
	"math/rand/v2"
	"testing"
)

func TestHitBuckets(t *testing.T) {
	for us, want := range map[int64]int{50: 0, 100: 0, 1000: 10, 10_000: 20, 999_999: 39, 1_000_000: 40, 1e9: 59} {
		if got := HitBucket(us); got != want {
			t.Errorf("HitBucket(%d) = %d, want %d", us, got, want)
		}
	}
	if f := HitBucketFloor(20); f < 9999 || f > 10001 {
		t.Errorf("floor(20) = %v", f)
	}
}

func TestHitsReservoirAndMerge(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	h := &Hits{}
	for i := 0; i < 1000; i++ {
		e := ""
		if i%10 == 0 {
			e = "status:500"
		}
		h.Add(HitSample{Key: "s|a", Latency: int64(1000 + i), Err: e}, rnd)
	}
	if len(h.Samples) != MaxHitSamples {
		t.Fatalf("samples %d", len(h.Samples))
	}
	var ok, bad int32
	for i := range h.OK {
		ok += h.OK[i]
	}
	for i := range h.Err {
		bad += h.Err[i]
	}
	if ok != 900 || bad != 100 {
		t.Fatalf("counts ok %d err %d", ok, bad)
	}
	h.Trim()
	m := MergeHits(nil, h)
	m = MergeHits(m, &Hits{OK: []int32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}})
	if m.OK[10] != h.OK[10]+5 || len(m.Samples) != MaxHitSamples {
		t.Fatalf("merge %v", m.OK)
	}
	if (&Hits{OK: make([]int32, HitBuckets)}).Trim() != nil {
		t.Fatal("empty hits should trim to nil")
	}
}
