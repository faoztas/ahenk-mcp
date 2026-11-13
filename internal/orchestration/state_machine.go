package orchestration

type OperationalStage string

const (
	Stage1Nominal         OperationalStage = "Stage1Nominal"
	Stage2MediumPressure  OperationalStage = "Stage2MediumPressure"
	Stage3CodexExhausted  OperationalStage = "Stage3CodexExhausted"
	Stage4AGYExhausted    OperationalStage = "Stage4AGYExhausted"
	Stage5DualCloudOutage OperationalStage = "Stage5DualCloudOutage"
	Stage6FreeTier429     OperationalStage = "Stage6FreeTier429"
)

// DetermineOperationalStage evaluates the health values of Codex, Antigravity,
// and OpenCode Free tiers to determine the system's operational routing stage.
func DetermineOperationalStage(codexHealth, agyHealth, freeHealth float64) OperationalStage {
	if codexHealth <= 0.0 && agyHealth <= 0.0 {
		return Stage5DualCloudOutage
	}
	if freeHealth <= 0.0 {
		return Stage6FreeTier429
	}
	if codexHealth <= 0.0 {
		return Stage3CodexExhausted
	}
	if agyHealth <= 0.0 {
		return Stage4AGYExhausted
	}
	if codexHealth < 0.70 || agyHealth < 0.70 {
		return Stage2MediumPressure
	}
	return Stage1Nominal
}
