package orchestration

import "testing"

func TestOperationalStateMachineTransitions(t *testing.T) {
	tests := []struct {
		codexHealth float64
		agyHealth   float64
		freeHealth  float64
		wantStage   OperationalStage
	}{
		{1.0, 1.0, 1.0, Stage1Nominal},
		{0.60, 0.90, 1.0, Stage2MediumPressure},
		{0.0, 0.80, 1.0, Stage3CodexExhausted},
		{0.90, 0.0, 1.0, Stage4AGYExhausted},
		{0.0, 0.0, 1.0, Stage5DualCloudOutage},
		{0.0, 0.0, 0.0, Stage5DualCloudOutage},
		{0.80, 0.80, 0.0, Stage6FreeTier429},
	}
	for _, tc := range tests {
		stage := DetermineOperationalStage(tc.codexHealth, tc.agyHealth, tc.freeHealth)
		if stage != tc.wantStage {
			t.Errorf("for (%v, %v, %v): got %s, want %s", tc.codexHealth, tc.agyHealth, tc.freeHealth, stage, tc.wantStage)
		}
	}
}
