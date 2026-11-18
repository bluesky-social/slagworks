package inventory

import "time"

const (
	StepStatusOK      = "ok"
	StepStatusPartial = "partial"
	StepStatusFailed  = "failed"
	// StepStatusSkipped marks a step deliberately not run for this host
	// (e.g. bmc_ipmitool on an Intel AMT host).
	StepStatusSkipped = "skipped"
)

// stepRecorder accumulates CommissionStep entries as collect funcs run.
// Each call to record() captures wall-clock duration around the
// supplied func and stores the verdict the caller hands back.
type stepRecorder struct {
	steps []CommissionStep
	now   func() time.Time // overridable for tests
}

func newStepRecorder() *stepRecorder {
	return &stepRecorder{now: time.Now}
}

// stepVerdict is what each wrapped collector reports back: the status
// level for this step plus an optional error/warning message. Status
// always wins out over the message — passing StepStatusOK with an
// error message is ignored on the error field but warnings still apply.
type stepVerdict struct {
	Status  string
	Error   string
	Warning string
}

// record runs fn, times it, and appends the resulting CommissionStep.
// fn is responsible for deciding the verdict from whatever it observed
// during collection (got non-empty result? sub-tool failed? partial
// data?).
func (r *stepRecorder) record(name string, fn func() stepVerdict) {
	start := r.now()
	v := fn()
	elapsed := r.now().Sub(start)
	step := CommissionStep{
		Name:       name,
		Status:     v.Status,
		DurationMs: elapsed.Milliseconds(),
	}
	if v.Status == StepStatusFailed {
		step.Error = v.Error
	}
	if v.Warning != "" {
		step.Warning = v.Warning
	}
	r.steps = append(r.steps, step)
}

// result returns the accumulated steps. Always returns a non-nil
// slice if at least one step was recorded; nil if record was never
// called (so an empty array is omitted from JSON via omitempty).
func (r *stepRecorder) result() []CommissionStep {
	return r.steps
}
