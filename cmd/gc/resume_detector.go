package main

import "time"

// resumeClockReading is one simultaneous reading of the wall clock and the
// monotonic clock.
type resumeClockReading struct {
	wall time.Time
	mono time.Duration
}

// resumeClock supplies readings to a resumeDetector.
type resumeClock func() resumeClockReading

// newSystemResumeClock returns the production resumeClock.
func newSystemResumeClock() resumeClock {
	return func() resumeClockReading { return resumeClockReading{} }
}

// resumeDetector detects a host suspend/resume between controller passes.
type resumeDetector struct{}

// newResumeDetector builds a detector over clock.
func newResumeDetector(resumeClock) *resumeDetector { return &resumeDetector{} }

// observe evaluates one pass and reports whether it detected a resume.
func (d *resumeDetector) observe(time.Duration) bool { return false }

// resumedAt returns the wall time of the latest detected resume.
func (d *resumeDetector) resumedAt() time.Time { return time.Time{} }

// detections returns how many resumes have been detected.
func (d *resumeDetector) detections() uint64 { return 0 }

// floor returns the current activity floor.
func (d *resumeDetector) floor() activityFloor { return activityFloor{} }

// activityFloor is the earliest instant a wall-clock activity stamp counts from.
type activityFloor struct{ at time.Time }

// raise returns t, lifted to the floor when it is before it.
func (f activityFloor) raise(t time.Time) time.Time { return t }
