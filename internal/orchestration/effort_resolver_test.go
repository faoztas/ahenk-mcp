package orchestration

import (
	"testing"
)

func TestResolveEffortTruthTable(t *testing.T) {
	tests := []struct {
		name        string
		category    string
		complexity  float64
		quotaHealth float64
		modelID     string
		wantEffort  EffortLevel
		wantReason  string
	}{
		{
			name:        "high complexity nominal quota",
			category:    "coding",
			complexity:  0.9,
			quotaHealth: 0.85,
			modelID:     "codex/gpt-6.1-sol",
			wantEffort:  EffortHigh,
			wantReason:  "NOMINAL_HIGH_EFFORT",
		},
		{
			name:        "high complexity medium pressure throttled",
			category:    "coding",
			complexity:  0.9,
			quotaHealth: 0.50,
			modelID:     "codex/gpt-6.1-sol",
			wantEffort:  EffortMedium,
			wantReason:  "QUOTA_PRESSURE_THROTTLED",
		},
		{
			name:        "routine research low effort always",
			category:    "research",
			complexity:  0.2,
			quotaHealth: 1.0,
			modelID:     "codex/gpt-6-luna",
			wantEffort:  EffortLow,
			wantReason:  "ROUTINE_TASK_LOW_EFFORT",
		},
		{
			name:        "quota critical low effort",
			category:    "coding",
			complexity:  0.8,
			quotaHealth: 0.20,
			modelID:     "codex/gpt-6.1-sol",
			wantEffort:  EffortLow,
			wantReason:  "QUOTA_CRITICAL_LOW_EFFORT",
		},
		{
			name:        "standard effort",
			category:    "coding",
			complexity:  0.5,
			quotaHealth: 0.80,
			modelID:     "codex/gpt-6.1-sol",
			wantEffort:  EffortMedium,
			wantReason:  "STANDARD_EFFORT",
		},
		{
			name:        "routine low complexity task",
			category:    "coding",
			complexity:  0.25,
			quotaHealth: 0.90,
			modelID:     "codex/gpt-6.1-sol",
			wantEffort:  EffortLow,
			wantReason:  "ROUTINE_TASK_LOW_EFFORT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotEffort, gotReason := ResolveEffort(tc.category, tc.complexity, tc.quotaHealth, tc.modelID)
			if gotEffort != tc.wantEffort {
				t.Errorf("effort mismatch: got %s, want %s", gotEffort, tc.wantEffort)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason mismatch: got %s, want %s", gotReason, tc.wantReason)
			}
		})
	}
}

func BenchmarkResolveEffort(b *testing.B) {
	for i := 0; i < b.N; i++ {
		ResolveEffort("coding", 0.8, 0.75, "codex/gpt-6.1-sol")
	}
}
