package capacityrrd

import (
	"reflect"
	"testing"
)

func tsSeq(samples []CapSample) []int64 {
	out := make([]int64, len(samples))
	for i, s := range samples {
		out[i] = s.TS
	}
	return out
}

// Growth phase: fewer pushes than Slots → chronological, no empty slots held.
func TestLazyGrowth(t *testing.T) {
	s := newCapSeries(5)
	for i := int64(1); i <= 3; i++ {
		s.push(CapSample{TS: i})
	}
	if len(s.Buf) != 3 {
		t.Fatalf("expected compact Buf len 3, got %d", len(s.Buf))
	}
	if got := tsSeq(s.all()); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("growth order wrong: %v", got)
	}
}

// Filling exactly to Slots, then wrapping, must keep newest-N in order.
func TestRingWrap(t *testing.T) {
	s := newCapSeries(3)
	for i := int64(1); i <= 7; i++ { // 7 pushes into a 3-slot ring
		s.push(CapSample{TS: i})
	}
	if len(s.Buf) != 3 {
		t.Fatalf("ring should cap Buf at Slots=3, got %d", len(s.Buf))
	}
	if got := tsSeq(s.all()); !reflect.DeepEqual(got, []int64{5, 6, 7}) {
		t.Fatalf("ring order wrong: %v", got)
	}
}

// Boundary: exactly Slots pushes → full, correct order, next push overwrites oldest.
func TestExactFillThenOne(t *testing.T) {
	s := newCapSeries(3)
	for i := int64(1); i <= 3; i++ {
		s.push(CapSample{TS: i})
	}
	if got := tsSeq(s.all()); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("exact-fill order wrong: %v", got)
	}
	s.push(CapSample{TS: 4})
	if got := tsSeq(s.all()); !reflect.DeepEqual(got, []int64{2, 3, 4}) {
		t.Fatalf("post-fill overwrite wrong: %v", got)
	}
}

// Migration: a legacy full-length buffer (Buf == Slots, partially populated via
// the OLD head/count semantics) must compact to the same logical sequence.
func TestCompactFromLegacyPartial(t *testing.T) {
	// Old format: Slots=5, Count=3, data in Buf[0..2], Head=3, trailing empties.
	s := &capSeries{
		Buf:   []CapSample{{TS: 1}, {TS: 2}, {TS: 3}, {}, {}},
		Head:  3,
		Count: 3,
		Slots: 5,
	}
	s.compactForSlots(5)
	if len(s.Buf) != 3 {
		t.Fatalf("expected compacted len 3, got %d", len(s.Buf))
	}
	if got := tsSeq(s.all()); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("legacy partial migration wrong: %v", got)
	}
	// Continues correctly after migration.
	s.push(CapSample{TS: 4})
	s.push(CapSample{TS: 5})
	s.push(CapSample{TS: 6}) // now full at 5, oldest (1) dropped
	if got := tsSeq(s.all()); !reflect.DeepEqual(got, []int64{2, 3, 4, 5, 6}) {
		t.Fatalf("post-migration ring wrong: %v", got)
	}
}

// Migration: a legacy FULL ring (Count==Slots, Head mid-buffer) must unwrap
// to chronological order.
func TestCompactFromLegacyFullRing(t *testing.T) {
	// Logical order is 3,4,5,6,7 stored as a ring with Head=3 (oldest at idx 3).
	s := &capSeries{
		Buf:   []CapSample{{TS: 6}, {TS: 7}, {TS: 0}, {TS: 3}, {TS: 4}, {TS: 5}},
		Head:  3,
		Count: 6,
		Slots: 6,
	}
	// Note: index 2 (TS:0) is the slot about to be overwritten next; with Head=3
	// the unwrap is Buf[3:]+Buf[:3] = 3,4,5,6,7,0.
	s.compactForSlots(6)
	if got := tsSeq(s.all()); !reflect.DeepEqual(got, []int64{3, 4, 5, 6, 7, 0}) {
		t.Fatalf("legacy full-ring migration wrong: %v", got)
	}
}
