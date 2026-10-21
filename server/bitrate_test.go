package server

import "testing"

func TestParseBitrate(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"", 0, false},
		{"800k", 800_000, false},
		{"2M", 2_000_000, false},
		{"2000", 2_000_000, false}, // a bare number means kbps
		{"1.5M", 1_500_000, false},
		{" 500K ", 500_000, false},
		{"-1", 0, true},
		{"abc", 0, true},
		{"2x", 0, true},
	}
	for _, c := range cases {
		got, err := ParseBitrate(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseBitrate(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseBitrate(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBitrate(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePriority(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    Priority
		wantErr bool
	}{
		{"fps", PriorityFPS, false},
		{"quality", PriorityQuality, false},
		{"QUALITY", PriorityQuality, false},
		{" quality ", PriorityQuality, false},
		{"", PriorityFPS, false},
		{"nonsense", 0, true},
	} {
		got, err := ParsePriority(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsePriority(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePriority(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePriority(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

const (
	testCap    = 1_000_000.0
	testOver   = 10_000_000.0 // hopelessly above the cap
	testSlight = 1_100_000.0  // just over it: one step per window
	testUnder  = 1_000.0      // comfortably below it
	testInBand = 800_000.0    // between lowWater (60%) and highWater (85%)
)

// fps-first must exhaust image quality before it touches the frame rate.
//
// The overshoot here is small enough to buy exactly one step per window, so the
// ordering is measured on its own; the multi-step response has its own test.
func TestRateControllerFPSFirstTightensQualityBeforeFPS(t *testing.T) {
	rc := newRateController(PriorityFPS, 65, 30, 15, 5)

	for i := 0; rc.quality() > rc.minQuality && i < 100; i++ {
		rc.observe(testSlight, testCap)
	}

	if rc.quality() != 30 {
		t.Fatalf("quality = %d, want the floor 30", rc.quality())
	}
	if rc.fps() != 15 {
		t.Fatalf("fps = %d, want it untouched at 15 while quality was still above its floor", rc.fps())
	}

	// Only once quality is exhausted may the frame rate start paying.
	rc.observe(testSlight, testCap)
	if rc.fps() >= 15 {
		t.Errorf("fps = %d, want it to start dropping after quality hit its floor", rc.fps())
	}
}

// quality-first is the mirror image.
func TestRateControllerQualityFirstTightensFPSBeforeQuality(t *testing.T) {
	rc := newRateController(PriorityQuality, 65, 30, 15, 5)

	for i := 0; rc.fps() > rc.minFPS && i < 100; i++ {
		rc.observe(testSlight, testCap)
	}

	if rc.fps() != 5 {
		t.Fatalf("fps = %d, want the floor 5", rc.fps())
	}
	if rc.quality() != 65 {
		t.Fatalf("quality = %d, want it untouched at 65 while fps was still above its floor", rc.quality())
	}

	rc.observe(testSlight, testCap)
	if rc.quality() >= 65 {
		t.Errorf("quality = %d, want it to start dropping after fps hit its floor", rc.quality())
	}
}

// A greedy window that runs out of one knob must keep spending the other, not
// abandon the remaining steps -- but only in priority order.
func TestRateControllerGreedyWindowSpillsIntoTheSecondKnob(t *testing.T) {
	rc := newRateController(PriorityFPS, 65, 30, 15, 5)

	// Walk quality down to just above its floor with single steps...
	for rc.quality() > 35 {
		rc.observe(testSlight, testCap)
	}
	// ...then hand it a window worth three steps. It spends one on quality,
	// hitting the floor, and the other two on the frame rate.
	rc.observe(testCap*4, testCap)

	if rc.quality() != 30 {
		t.Errorf("quality = %d, want it taken to its floor of 30", rc.quality())
	}
	if rc.fps() != 11 {
		t.Errorf("fps = %d, want 11 (two steps of 2 spent on the frame rate)", rc.fps())
	}
}

func TestRateControllerRespectsFloorsAndRecovers(t *testing.T) {
	rc := newRateController(PriorityFPS, 65, 30, 15, 5)

	for i := 0; i < 200; i++ {
		rc.observe(testOver, testCap) // hopelessly over budget
	}
	if rc.quality() != 30 || rc.fps() != 5 {
		t.Fatalf("floors not respected: quality=%d fps=%d, want 30/5", rc.quality(), rc.fps())
	}
	if !rc.limited() {
		t.Error("limited() = false while sitting at both floors")
	}

	for i := 0; i < 200; i++ {
		rc.observe(testUnder, testCap)
	}
	if rc.quality() != 65 || rc.fps() != 15 {
		t.Fatalf("did not recover: quality=%d fps=%d, want 65/15", rc.quality(), rc.fps())
	}
	if rc.limited() {
		t.Error("limited() = true after recovering to the configured maximum")
	}
}

// Inside the hysteresis band the controller must hold still, or the stream
// would visibly oscillate between quality levels.
func TestRateControllerHoldsInsideHysteresisBand(t *testing.T) {
	rc := newRateController(PriorityFPS, 65, 30, 15, 5)

	rc.observe(testOver, testCap) // one tighten
	quality, fps := rc.quality(), rc.fps()

	for i := 0; i < 10; i++ {
		rc.observe(testInBand, testCap)
	}
	if rc.quality() != quality || rc.fps() != fps {
		t.Errorf("settings moved inside the hysteresis band: quality %d->%d, fps %d->%d",
			quality, rc.quality(), fps, rc.fps())
	}
}

// With no cap configured the stream must sit at the configured maximum,
// however much traffic it happens to produce.
func TestRateControllerWithoutCapNeverThrottles(t *testing.T) {
	rc := newRateController(PriorityFPS, 65, 30, 15, 5)

	for i := 0; i < 100; i++ {
		rc.observe(999_000_000, 0)
	}
	if rc.quality() != 65 || rc.fps() != 15 || rc.limited() {
		t.Errorf("no cap should mean no throttling, got quality=%d fps=%d limited=%v",
			rc.quality(), rc.fps(), rc.limited())
	}
}

// One step per window is far too slow when a burst lands far over budget: at
// four times the cap the controller must give up several steps at once, or a
// two-second spike takes a dozen seconds to absorb.
func TestRateControllerTightensHarderWhenFarOverBudget(t *testing.T) {
	const cap = 1_000_000.0

	gentle := newRateController(PriorityFPS, 65, 30, 15, 5)
	gentle.observe(cap*1.1, cap) // barely over: one step
	if got := gentle.quality(); got != 60 {
		t.Errorf("barely over budget: quality = %d, want 60 (one step)", got)
	}

	hard := newRateController(PriorityFPS, 65, 30, 15, 5)
	hard.observe(cap*4, cap) // four times over: three steps
	if got := hard.quality(); got != 50 {
		t.Errorf("four times over budget: quality = %d, want 50 (three steps)", got)
	}
}

// At both floors the controller has nothing left to give. Reporting that lets
// the server warn, instead of silently missing the target forever.
func TestRateControllerReportsSaturation(t *testing.T) {
	rc := newRateController(PriorityFPS, 65, 30, 15, 5)

	if rc.atFloors() {
		t.Fatal("atFloors() = true before any measurement")
	}
	for i := 0; i < 200; i++ {
		rc.observe(testOver, testCap)
	}
	if !rc.atFloors() {
		t.Error("atFloors() = false while pinned at both floors and over budget")
	}

	// Back inside budget, the warning must clear.
	rc.observe(testInBand, testCap)
	if rc.atFloors() {
		t.Error("atFloors() = true after returning to budget")
	}
}

// The caller inverts the static-frame baseline when the setpoint moves, so it
// has to be told exactly when that happens -- a static screen would otherwise
// keep showing the old quality until something on it changed.
func TestRateControllerReportsSetpointChanges(t *testing.T) {
	rc := newRateController(PriorityFPS, 65, 30, 15, 5)

	if !rc.observe(testOver, testCap) {
		t.Error("observe() = false on the first tightening step, want true")
	}
	if rc.observe(testInBand, testCap) {
		t.Error("observe() = true inside the hysteresis band, want false")
	}
	if !rc.observe(testUnder, testCap) {
		t.Error("observe() = false while relaxing, want true")
	}

	// At the ceiling there is nothing left to give back.
	for i := 0; i < 100; i++ {
		rc.observe(testUnder, testCap)
	}
	if rc.observe(testUnder, testCap) {
		t.Error("observe() = true while already at the configured maximum, want false")
	}
}
