package orchestration

// EffortLevel characterizes the reasoning effort allocated to a model execution.
type EffortLevel string

const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
)

// ResolveEffort dynamically calculates the target reasoning effort and justification reason
// based on task category, task cognitive complexity (0.0 - 1.0), provider quota health (0.0 - 1.0),
// and the assigned candidate modelID.
func ResolveEffort(category string, complexity float64, quotaHealth float64, modelID string) (EffortLevel, string) {
	if category == "research" || complexity < 0.3 {
		return EffortLow, "ROUTINE_TASK_LOW_EFFORT"
	}
	if quotaHealth < 0.60 && quotaHealth >= 0.30 {
		return EffortMedium, "QUOTA_PRESSURE_THROTTLED"
	}
	if quotaHealth < 0.30 {
		return EffortLow, "QUOTA_CRITICAL_LOW_EFFORT"
	}
	if complexity >= 0.7 && quotaHealth >= 0.60 {
		return EffortHigh, "NOMINAL_HIGH_EFFORT"
	}
	return EffortMedium, "STANDARD_EFFORT"
}
