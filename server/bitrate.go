package server

import (
	"fmt"
	"strconv"
	"strings"
)

// Priority decides which knob the rate controller gives up first when the
// stream is over budget.
type Priority int

const (
	// PriorityFPS protects the frame rate and sacrifices image quality first.
	// Interaction stays smooth; text gets softer.
	PriorityFPS Priority = iota
	// PriorityQuality protects image quality and sacrifices frame rate first.
	// The picture stays sharp; motion gets choppier.
	PriorityQuality
)

func (p Priority) String() string {
	if p == PriorityQuality {
		return "quality"
	}
	return "fps"
}

// ParsePriority maps the -priority flag onto a Priority.
func ParsePriority(s string) (Priority, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "fps", "frame", "framerate":
		return PriorityFPS, nil
	case "quality", "image":
		return PriorityQuality, nil
	}
	return 0, fmt.Errorf("未知的优先级 %q（可选 fps 或 quality）", s)
}

// ParseBitrate parses a -max-bitrate value into bits per second.
//
// Accepted forms: "0" (unlimited), "800k", "2M", or a bare number in kbps --
// so "2000" and "2M" both mean 2 Mbps.
func ParseBitrate(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, nil
	}

	mult := int64(1000) // a bare number is kbps
	switch {
	case strings.HasSuffix(t, "k"):
		t = strings.TrimSuffix(t, "k")
	case strings.HasSuffix(t, "m"):
		t, mult = strings.TrimSuffix(t, "m"), 1000*1000
	case strings.HasSuffix(t, "g"):
		t, mult = strings.TrimSuffix(t, "g"), 1000*1000*1000
	}

	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析 %q：应为 0、800k、2M 这类写法，或纯数字（单位 kbps）", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("码率上限不能为负数")
	}
	return int64(v * float64(mult)), nil
}

// knob is one of the two things the controller can give up.
type knob int

const (
	knobQuality knob = iota
	knobFPS
)

// Rate-control tuning. The controller runs once per control window (about a
// second) and is deliberately asymmetric: give up ground fast, take it back
// slowly. That is the classic AIMD shape, and it matters here because lowering
// the frame rate lowers the very bitrate being measured -- a controller that
// reacted symmetrically would chase its own tail.
const (
	qualityStep = 5 // additive step for JPEG quality
	fpsStepMin  = 2 // smallest frame-rate step
	fpsStepDiv  = 8 // frame-rate steps are max(fpsStepMin, fps/8)

	// highWater and lowWater are the fractions of the budget above and below
	// which the controller acts. The gap is the hysteresis band that stops it
	// oscillating, and the whole band sits below the ceiling so that ordinary
	// variation does not breach it.
	highWater = 0.85
	lowWater  = 0.60
)

// videoBudget returns the share of the total egress ceiling left for video,
// which is that ceiling minus the audio stream's fixed cost.
//
// The subtraction uses audio's *nominal* rate, not a measurement: PCM is
// constant-rate, so there is nothing to measure, and a stable target stops the
// controller from chasing audio statistics that vary only in how they are
// reported. maxBitrate <= 0 means no ceiling and passes through unchanged.
//
// When audio alone exceeds the ceiling the result is clamped to one bit per
// second rather than to zero, because observe reads zero as "no cap" and would
// otherwise switch off the very limit that was just exceeded. One bit per
// second drives the controller to its floors and reports saturation, which is
// the honest outcome.
func videoBudget(maxBitrate, audioBps int64) int64 {
	if maxBitrate <= 0 {
		return 0
	}
	if audioBps <= 0 {
		return maxBitrate
	}
	return max(1, maxBitrate-audioBps)
}

// rateController adapts JPEG quality and frame rate to hold a bitrate budget.
//
// It is a plain value with no clock and no I/O, so its behaviour is fully
// exercisable in unit tests. It is not safe for concurrent use: the capture
// loop drives it, and only the accessors are read elsewhere.
type rateController struct {
	priority Priority

	maxQuality, minQuality int
	maxFPS, minFPS         int

	curQuality int
	curFPS     int

	throttled bool    // sitting below the configured maximum
	saturated bool    // at both floors and still over budget
	lastBps   float64 // most recent measurement
}

func newRateController(p Priority, quality, minQuality, fps, minFPS int) *rateController {
	return &rateController{
		priority:   p,
		maxQuality: quality,
		minQuality: minQuality,
		maxFPS:     fps,
		minFPS:     minFPS,
		curQuality: quality,
		curFPS:     fps,
	}
}

func (rc *rateController) quality() int     { return rc.curQuality }
func (rc *rateController) fps() int         { return rc.curFPS }
func (rc *rateController) limited() bool    { return rc.throttled }
func (rc *rateController) bitrate() float64 { return rc.lastBps }

// atFloors reports that the controller has given up everything it can and the
// stream is *still* over budget. Only -scale can help at that point, so the
// caller surfaces this rather than silently missing the target forever.
func (rc *rateController) atFloors() bool { return rc.saturated }

// order lists the two knobs in the order they are given up. Relaxing walks the
// same list backwards, so "give back in the reverse order we spent it" falls
// out of the data instead of being duplicated in two mirrored branches -- the
// only difference between the two priority modes is this one line.
func (rc *rateController) order() [2]knob {
	if rc.priority == PriorityFPS {
		// The frame rate is what we are protecting, so quality pays first.
		return [2]knob{knobQuality, knobFPS}
	}
	return [2]knob{knobFPS, knobQuality}
}

// observe feeds one measurement window into the controller and adjusts the
// stream. It reports whether the setpoint actually changed, which the caller
// needs in order to invalidate the static-frame baseline.
//
// bitsPerSec should be what really reached the sockets in the window, not what
// was generated: a client with a full send queue has its frames dropped, and
// counting those would make the controller throttle to fix congestion that the
// transport already handled.
//
// targetBitsPerSec <= 0 means no cap is configured.
func (rc *rateController) observe(bitsPerSec, targetBitsPerSec float64) (changed bool) {
	before := [2]int{rc.curQuality, rc.curFPS}

	rc.lastBps = bitsPerSec
	rc.saturated = false

	if targetBitsPerSec <= 0 {
		rc.curQuality, rc.curFPS = rc.maxQuality, rc.maxFPS
		rc.throttled = rc.curQuality < rc.maxQuality || rc.curFPS < rc.maxFPS
		return before != [2]int{rc.curQuality, rc.curFPS}
	}

	switch ratio := bitsPerSec / targetBitsPerSec; {
	case ratio > highWater:
		// Give up more the further over we are. One step per window is far too
		// slow when a burst lands at three times the budget.
		rc.tighten(stepsFor(ratio))
	case ratio < lowWater:
		rc.relax(1)
	}

	rc.throttled = rc.curQuality < rc.maxQuality || rc.curFPS < rc.maxFPS
	return before != [2]int{rc.curQuality, rc.curFPS}
}

// stepsFor scales the response to how far over budget the window was.
func stepsFor(ratio float64) int {
	switch {
	case ratio > 3:
		return 3
	case ratio > 1.5:
		return 2
	default:
		return 1
	}
}

// tighten gives up up to steps, spending the knobs in priority order.
func (rc *rateController) tighten(steps int) {
	order := rc.order()
	for i := 0; i < steps; i++ {
		if rc.giveUp(order[0]) {
			continue
		}
		if !rc.giveUp(order[1]) {
			// Both knobs are at their floors and the stream is still over
			// budget. Nothing here can fix that; the caller reports it.
			rc.saturated = true
			return
		}
	}
}

// relax takes back up to steps, in the reverse of the order tighten spent them.
func (rc *rateController) relax(steps int) {
	order := rc.order()
	for i := 0; i < steps; i++ {
		if rc.giveBack(order[1]) {
			continue
		}
		if !rc.giveBack(order[0]) {
			return
		}
	}
}

// giveUp lowers one knob by a step, reporting false if it is already at its
// floor.
func (rc *rateController) giveUp(k knob) bool {
	switch k {
	case knobQuality:
		if rc.curQuality > rc.minQuality {
			rc.curQuality = max(rc.minQuality, rc.curQuality-qualityStep)
			return true
		}
	case knobFPS:
		if rc.curFPS > rc.minFPS {
			// Scale the step with the frame rate: a flat -2 would take
			// twenty-eight windows to walk -fps 60 down to the floor.
			rc.curFPS = max(rc.minFPS, rc.curFPS-max(fpsStepMin, rc.curFPS/fpsStepDiv))
			return true
		}
	}
	return false
}

// giveBack raises one knob by a step, reporting false if it is already at its
// configured maximum.
func (rc *rateController) giveBack(k knob) bool {
	switch k {
	case knobQuality:
		if rc.curQuality < rc.maxQuality {
			rc.curQuality = min(rc.maxQuality, rc.curQuality+qualityStep)
			return true
		}
	case knobFPS:
		if rc.curFPS < rc.maxFPS {
			rc.curFPS = min(rc.maxFPS, rc.curFPS+fpsStepMin)
			return true
		}
	}
	return false
}
