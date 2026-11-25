// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRoutingBenchmarkFixtureUsesMatchedTaskVariants(t *testing.T) {
	fixturePath := filepath.Join("testdata", "routing", "matched_profiles_v1.json")
	suite, err := LoadRoutingBenchmarkFixture(fixturePath)
	if err != nil {
		t.Fatalf("failed to load matched benchmark fixture: %v", err)
	}
	report, err := EvaluateRoutingBenchmark(suite)
	if err != nil {
		t.Fatalf("failed to evaluate matched benchmark fixture: %v", err)
	}
	if report.MatchedCaseCount != 5 {
		t.Fatalf("expected 5 matched cases across 4 profiles, got %d", report.MatchedCaseCount)
	}
	for _, wantRole := range []string{"coding", "debugging", "code_review", "planning"} {
		if !slices.Contains(report.RolesCovered, wantRole) {
			t.Fatalf("missing required role %q in roles_covered %v", wantRole, report.RolesCovered)
		}
	}
	if !report.HasDefectiveReviewCase || !report.HasCleanReviewCase {
		t.Fatalf("expected both known-defective and clean code_review artifacts, got defective=%v clean=%v",
			report.HasDefectiveReviewCase, report.HasCleanReviewCase)
	}
	if !report.HasValidPlanningDAGCase {
		t.Fatal("expected valid planning DAG case")
	}
	for _, profile := range []string{
		BenchmarkProfileReference,
		BenchmarkProfilePricePerformance,
		BenchmarkProfileQuality,
		BenchmarkProfilePrice,
	} {
		summary, ok := report.Profiles[profile]
		if !ok || summary.RawSampleCount != 5 || summary.VerifiedSuccessCount != 5 {
			t.Fatalf("unexpected profile summary for %q: %+v", profile, summary)
		}
	}

	// Mutations across the 4 profiles must be rejected by the validator.
	for _, tc := range []struct {
		name, reason string
		mutate       func(*BenchmarkSuite)
	}{
		{
			name:   "mismatched task_variant across profiles",
			reason: "mismatched task_variant",
			mutate: func(s *BenchmarkSuite) {
				s.Runs[1].TaskVariant = "different-task-variant-v2"
			},
		},
		{
			name:   "mismatched repo_policy_version across profiles",
			reason: "mismatched repo_policy_version",
			mutate: func(s *BenchmarkSuite) {
				s.Runs[2].RepoPolicyVersion = "untrusted-policy-v9"
			},
		},
		{
			name:   "mismatched acceptance_rubric across profiles",
			reason: "mismatched acceptance_rubric",
			mutate: func(s *BenchmarkSuite) {
				s.Runs[3].AcceptanceRubric.RubricVersion = "other-rubric"
			},
		},
		{
			name:   "mismatched reference_baseline across profiles",
			reason: "mismatched reference_baseline",
			mutate: func(s *BenchmarkSuite) {
				s.Runs[1].ReferenceBaseline = "agy:gemini-3.8-flash-medium:medium"
			},
		},
		{
			name:   "missing one of the four profiles",
			reason: "matched profile runs",
			mutate: func(s *BenchmarkSuite) {
				s.Runs = s.Runs[:len(s.Runs)-1]
			},
		},
		{
			name:   "unbalanced fixed run_order across cases",
			reason: "unbalanced run_order",
			mutate: func(s *BenchmarkSuite) {
				orderByProfile := map[string]int{
					BenchmarkProfileReference:        1,
					BenchmarkProfilePricePerformance: 2,
					BenchmarkProfileQuality:          3,
					BenchmarkProfilePrice:            4,
				}
				for i := range s.Runs {
					s.Runs[i].RunOrder = orderByProfile[s.Runs[i].Profile]
				}
			},
		},
		{
			name:   "defective review artifact falsely approved as verified_success",
			reason: "claims independent_verified",
			mutate: func(s *BenchmarkSuite) {
				for i := range s.Runs {
					if s.Runs[i].CaseID == "case-review-defective-01" && s.Runs[i].Profile == BenchmarkProfilePrice {
						s.Runs[i].Attempts[0].Verification.ReviewResult.Verdict = "APPROVED"
						s.Runs[i].Attempts[0].Verification.ReviewResult.Findings = nil
						s.Runs[i].VerifiedSuccess = true
					}
				}
			},
		},
		{
			name:   "planning DAG cycle claimed as verified_success",
			reason: "claims independent_verified",
			mutate: func(s *BenchmarkSuite) {
				for i := range s.Runs {
					if s.Runs[i].CaseID == "case-planning-01" && s.Runs[i].Profile == BenchmarkProfilePrice {
						s.Runs[i].Attempts[0].Verification.PlanNodes[0].DependsOn = []string{"verify"}
						s.Runs[i].VerifiedSuccess = true
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated, err := LoadRoutingBenchmarkFixture(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&mutated)
			if _, err := EvaluateRoutingBenchmark(mutated); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("expected %q, got %v", tc.reason, err)
			}
		})
	}
}

func TestRoutingBenchmarkKeepsUnknownCostAndFailedAttempts(t *testing.T) {
	fixturePath := filepath.Join("testdata", "routing", "cost_and_missing_data_v1.json")
	suite, err := LoadRoutingBenchmarkFixture(fixturePath)
	if err != nil {
		t.Fatalf("failed to load cost/missing-data fixture: %v", err)
	}
	report, err := EvaluateRoutingBenchmark(suite)
	if err != nil {
		t.Fatalf("failed to evaluate cost/missing-data fixture: %v", err)
	}

	// 1. Reference profile has two different accounts/windows (codex-acct-a/five_hour and codex-acct-b/weekly).
	// In codex-acct-a, attempt 1 failed with 0.05 USD and attempt 2 succeeded with 0.10 USD -> total measurable cost = 0.15 USD.
	refSummary := report.Profiles[BenchmarkProfileReference]
	if refSummary.RawSampleCount != 2 || refSummary.VerifiedSuccessCount != 2 || refSummary.FailedAttemptCount != 1 {
		t.Fatalf("unexpected reference profile counts: %+v", refSummary)
	}
	if refSummary.P50MS != 14000 || refSummary.P95MS != 18000 {
		t.Fatalf("expected reference p50=14000 p95=18000, got p50=%d p95=%d", refSummary.P50MS, refSummary.P95MS)
	}
	if len(refSummary.CostGroups) != 2 {
		t.Fatalf("expected 2 separate cost groups for different accounts/windows in reference profile, got %d: %+v",
			len(refSummary.CostGroups), refSummary.CostGroups)
	}
	var acctAGroup *CostGroupSummary
	for i := range refSummary.CostGroups {
		if refSummary.CostGroups[i].Account == "codex-acct-a" {
			acctAGroup = &refSummary.CostGroups[i]
		}
	}
	if acctAGroup == nil || acctAGroup.TotalMeasurableCost == nil || math.Abs(*acctAGroup.TotalMeasurableCost-0.15) > 1e-9 ||
		acctAGroup.FailedAttemptMeasurableCost == nil || math.Abs(*acctAGroup.FailedAttemptMeasurableCost-0.05) > 1e-9 {
		t.Fatalf("failed attempt measurable cost was omitted from codex-acct-a group: %+v", acctAGroup)
	}

	// 2. Quality profile has a failed run (0.08 USD) and a succeeded run (0.12 USD) in codex-acct-b/weekly.
	// Total measurable cost must include the failed run's 0.08 USD -> 0.20 USD total, 0.20 USD per verified success.
	qualSummary := report.Profiles[BenchmarkProfileQuality]
	if qualSummary.RawSampleCount != 2 || qualSummary.VerifiedSuccessCount != 1 || qualSummary.FailedAttemptCount != 1 {
		t.Fatalf("unexpected quality profile counts: %+v", qualSummary)
	}
	if len(qualSummary.CostGroups) != 1 || qualSummary.CostGroups[0].TotalMeasurableCost == nil ||
		math.Abs(*qualSummary.CostGroups[0].TotalMeasurableCost-0.20) > 1e-9 ||
		qualSummary.CostGroups[0].CostPerVerifiedSuccess == nil ||
		math.Abs(*qualSummary.CostGroups[0].CostPerVerifiedSuccess-0.20) > 1e-9 {
		t.Fatalf("quality profile did not include failed attempt cost in total measurable cost: %+v", qualSummary.CostGroups)
	}

	// 3. Price-Performance profile has UNKNOWN reported cost for both runs (claude-unbound and agy subscription).
	// UNKNOWN cost must NEVER be coerced to 0: TotalMeasurableCost must be nil and CostStatus == "UNKNOWN".
	fpSummary := report.Profiles[BenchmarkProfilePricePerformance]
	if fpSummary.UnknownCostCount != 2 || fpSummary.UnknownQuotaCount != 1 || fpSummary.MissingDataRatio <= 0 {
		t.Fatalf("expected unknown cost/quota and positive missing_data_ratio in price_performance, got %+v", fpSummary)
	}
	for _, g := range fpSummary.CostGroups {
		if g.CostStatus != BenchmarkCostUnknown || g.TotalMeasurableCost != nil || g.CostPerVerifiedSuccess != nil {
			t.Fatalf("UNKNOWN cost was coerced to numeric value in group %+v", g)
		}
		if g.MissingDataRatio != 1.0 {
			t.Fatalf("expected missing_data_ratio=1.0 for unknown cost group, got %v", g.MissingDataRatio)
		}
	}

	// 4. Price profile has explicit 0.0 reported cost (ZERO_PRICE_CATALOG) but UNKNOWN quota provenance.
	// Cost is KNOWN 0.0 (non-nil pointer to 0.0), while quota unknown is tracked in MissingDataRatio / UnknownQuotaCount.
	priceSummary := report.Profiles[BenchmarkProfilePrice]
	if priceSummary.UnknownCostCount != 0 || priceSummary.UnknownQuotaCount != 2 {
		t.Fatalf("expected 0 unknown cost and 2 unknown quota in price profile, got %+v", priceSummary)
	}
	if len(priceSummary.CostGroups) != 1 || priceSummary.CostGroups[0].CostStatus != "KNOWN" ||
		priceSummary.CostGroups[0].TotalMeasurableCost == nil || *priceSummary.CostGroups[0].TotalMeasurableCost != 0.0 {
		t.Fatalf("zero reported cost should be preserved as KNOWN 0.0, got %+v", priceSummary.CostGroups)
	}
}

func TestRoutingBenchmarkProtocolRejectsUnsupportedClaims(t *testing.T) {
	fixturePath := filepath.Join("testdata", "routing", "quota_pressure_offline_v1.json")
	suite, err := LoadRoutingBenchmarkFixture(fixturePath)
	if err != nil {
		t.Fatalf("failed to load quota-pressure fixture: %v", err)
	}
	report, err := EvaluateRoutingBenchmark(suite)
	if err != nil {
		t.Fatalf("failed to evaluate quota-pressure fixture: %v", err)
	}

	// Small sample (2 matched cases < min_matched_cases_per_profile=16) must be labeled "pilot"
	// and must not allow superiority claims.
	if report.SampleClass != BenchmarkSamplePilot {
		t.Fatalf("expected small sample to be labeled %q, got %q", BenchmarkSamplePilot, report.SampleClass)
	}
	if report.SuperiorityClaimAllowed {
		t.Fatal("expected superiority claim to be disallowed for pilot sample size")
	}

	// Verify all 5 required offline quota-pressure scenarios are recorded and verified.
	for _, scenario := range []string{
		"EXHAUSTED_0PCT",
		"FALLBACK_429",
		"HALF_OPEN_RECOVERY",
		"NO_ELIGIBLE_ROUTE",
		"RETRY_IDEMPOTENCY",
	} {
		summary, ok := report.QuotaPressureScenarios[scenario]
		if !ok || summary.RunCount == 0 {
			t.Fatalf("missing offline quota-pressure scenario %q in report: %+v", scenario, report.QuotaPressureScenarios)
		}
	}
	if report.QuotaPressureScenarios["EXHAUSTED_0PCT"].BlockedSpawnCount == 0 {
		t.Fatal("expected EXHAUSTED_0PCT scenario to record blocked spawn with 0 process exit")
	}
	if report.QuotaPressureScenarios["FALLBACK_429"].FallbackSuccessCount == 0 {
		t.Fatal("expected FALLBACK_429 scenario to record scoped fallback success")
	}
	if report.QuotaPressureScenarios["HALF_OPEN_RECOVERY"].HalfOpenProbeSuccessCount == 0 {
		t.Fatal("expected HALF_OPEN_RECOVERY scenario to record half-open probe recovery")
	}
	if report.QuotaPressureScenarios["RETRY_IDEMPOTENCY"].IdempotentReplayCount == 0 {
		t.Fatal("expected RETRY_IDEMPOTENCY scenario to record idempotent replay without second spawn")
	}

	suite.Protocol.ClaimSuperiority = true
	_, err = EvaluateRoutingBenchmark(suite)
	if err == nil || !strings.Contains(err.Error(), "PILOT_SAMPLE_SIZE_BELOW_PLAN") {
		t.Fatalf("pilot claim: %v", err)
	}
}

func TestRoutingBenchmarkProtocolMetadata(t *testing.T) {
	suite, err := LoadRoutingBenchmarkFixture("testdata/routing/matched_profiles_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise a full-sized control through the public evaluator. The "live"
	// label here tests the metadata gate using fabricated observations.
	rows := suite.Runs
	for batch := 1; batch < 4; batch++ {
		for _, r := range rows {
			r.CaseID = fmt.Sprintf("%s-%d", r.CaseID, batch)
			r.RunOrder = (r.RunOrder-1+batch)%4 + 1
			suite.Runs = append(suite.Runs, r)
		}
	}
	suite.Protocol.SamplePlan.MinMatchedCasesPerProfile = 20
	suite.Protocol.Environment.ProviderMode = "live"
	suite.Protocol.BudgetLimit.MaxTotalCalls = 100
	report, err := EvaluateRoutingBenchmark(suite)
	if err != nil || !report.ProtocolEligible || report.SampleClass != BenchmarkSampleFull || report.SuperiorityClaimAllowed || !slices.Equal(report.SuperiorityRejectionReasons, []string{"MEASURED_COMPARISON_REQUIRED"}) {
		t.Fatalf("invalid control: report=%+v err=%v", report, err)
	}
	offline := suite
	offline.Protocol.Environment.ProviderMode = "offline_simulated"
	report, err = EvaluateRoutingBenchmark(offline)
	if err != nil || report.ProtocolEligible || report.SampleClass != BenchmarkSamplePilot || report.SuperiorityClaimAllowed || !slices.Contains(report.SuperiorityRejectionReasons, "OFFLINE_SIMULATION") {
		t.Fatalf("full-sized simulation: %+v %v", report, err)
	}
	for _, tc := range []struct {
		reason string
		mutate func(*BenchmarkProtocol)
	}{
		{"PRE_REGISTRATION_MISSING", func(p *BenchmarkProtocol) { p.PreRegistered = false }},
		{"HYPOTHESIS_MISSING", func(p *BenchmarkProtocol) { p.Hypothesis = "" }},
		{"PRIMARY_METRIC_MISSING", func(p *BenchmarkProtocol) { p.PrimaryMetric = "" }},
		{"SAMPLE_PLAN_MISSING", func(p *BenchmarkProtocol) { p.SamplePlan.MinMatchedCasesPerProfile = 0 }},
		{"RECORDED_AT_MISSING", func(p *BenchmarkProtocol) { p.RecordedAt = time.Time{} }},
		{"ENVIRONMENT_METADATA_MISSING", func(p *BenchmarkProtocol) { p.Environment.CPU = "" }},
		{"DRIFT_THRESHOLDS_MISSING", func(p *BenchmarkProtocol) { p.DriftThresholds = BenchmarkDriftThresholds{} }},
		{"BUDGET_LIMIT_MISSING", func(p *BenchmarkProtocol) { p.BudgetLimit.MaxTotalCalls = 0 }},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			mut := suite
			tc.mutate(&mut.Protocol)
			report, err := EvaluateRoutingBenchmark(mut)
			if err != nil {
				t.Fatal(err)
			}
			got := report.SuperiorityRejectionReasons
			if report.ProtocolEligible || !slices.Equal(got, []string{tc.reason, "MEASURED_COMPARISON_REQUIRED"}) {
				t.Fatalf("reasons %v", got)
			}
		})
	}
}

func TestRoutingBenchmarkFiveCaseOfflinePilot(t *testing.T) {
	suite, err := LoadRoutingBenchmarkFixture("testdata/routing/matched_profiles_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	suite.Protocol.SamplePlan.MinMatchedCasesPerProfile = 5
	report, err := EvaluateRoutingBenchmark(suite)
	if err != nil {
		t.Fatal(err)
	}
	if report.SampleClass != BenchmarkSamplePilot || report.SuperiorityClaimAllowed || !slices.Contains(report.SuperiorityRejectionReasons, "SAMPLE_PLAN_INSUFFICIENT_MINIMUM") {
		t.Fatalf("n=5 sample=%s claim=%t", report.SampleClass, report.SuperiorityClaimAllowed)
	}
}

func TestRoutingBenchmarkInvalidEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, file, reason string
		mutate             func([]BenchmarkCaseRun)
	}{
		{"actual_reference_model", "matched_profiles", "reference route", func(r []BenchmarkCaseRun) { r[0].Attempts[0].Model = "other" }},
		{"actual_reference_effort", "matched_profiles", "reference route", func(r []BenchmarkCaseRun) { r[0].Attempts[0].Effort = "low" }},
		{"actual_reference_provider", "matched_profiles", "reference route", func(r []BenchmarkCaseRun) { r[0].Attempts[0].Provider = "other" }},
		{"policy_across_pairs", "matched_profiles", "repo_policy_version", func(r []BenchmarkCaseRun) {
			for i := 4; i < 8; i++ {
				r[i].RepoPolicyVersion = "other"
			}
		}},
		{"baseline_across_pairs", "matched_profiles", "reference_baseline", func(r []BenchmarkCaseRun) {
			for i := 4; i < 8; i++ {
				r[i].ReferenceBaseline = "other:gpt-5.4:high"
				if r[i].Profile == "reference" {
					r[i].Attempts[0].Provider = "other"
				}
			}
		}},
		{"mixed_units_scalar", "cost_and_missing_data", "different cost units", func(r []BenchmarkCaseRun) { r[0].Attempts[0].CostUnit = "quota_percent_weekly" }},
		{"omitted_failed_cost", "cost_and_missing_data", "reconcile", func(r []BenchmarkCaseRun) { r[0].ReportedCost = r[0].Attempts[1].ReportedCost }},
		{"terminal_disagreement", "matched_profiles", "verified_success", func(r []BenchmarkCaseRun) { r[0].VerifiedSuccess = false }},
		{"stale_numeric", "matched_profiles", "numeric", func(r []BenchmarkCaseRun) { r[0].QuotaProvenance.State = "STALE" }},
		{"missing_quota_account", "matched_profiles", "account and window", func(r []BenchmarkCaseRun) { r[0].QuotaProvenance.Account = "" }},
		{"missing_quota_window", "matched_profiles", "account and window", func(r []BenchmarkCaseRun) { r[0].QuotaProvenance.Window = "" }},
		{"provider_scope", "quota_pressure_offline", "blocked scope", func(r []BenchmarkCaseRun) { r[7].Attempts[0].RateLimitScope = "provider" }},
		{"model_scope", "quota_pressure_offline", "blocked scope", func(r []BenchmarkCaseRun) { r[7].Attempts[1].Model = r[7].Attempts[0].Model }},
		{"exhausted_account", "quota_pressure_offline", "blocked candidate", func(r []BenchmarkCaseRun) { r[0].Attempts[0].Account = "other" }},
		{"exhausted_window", "quota_pressure_offline", "blocked candidate", func(r []BenchmarkCaseRun) { r[0].Attempts[0].QuotaWindow = "other" }},
		{"exhausted_spawn", "quota_pressure_offline", "must not spawn", func(r []BenchmarkCaseRun) { r[0].Attempts[0].ProcessExitCode = new(int) }},
		{"normal_exhausted_spawn", "matched_profiles", "exhausted quota scope", func(r []BenchmarkCaseRun) {
			r[0].QuotaProvenance.State = "EXHAUSTED"
			r[0].QuotaProvenance.RemainingPercent = new(float64)
		}},
		{"half_open_route_only", "quota_pressure_offline", "half-open", func(r []BenchmarkCaseRun) { r[2].Attempts[0].RecoveryState = "" }},
		{"half_open_state_only", "quota_pressure_offline", "half-open", func(r []BenchmarkCaseRun) { r[2].Attempts[0].RouteStatus = "SELECTED" }},
		{"half_open_no_evidence", "quota_pressure_offline", "half-open", func(r []BenchmarkCaseRun) { r[2].Attempts[0].ProbeEvidence = nil }},
		{"half_open_before_cooldown", "quota_pressure_offline", "half-open", func(r []BenchmarkCaseRun) {
			e := r[2].Attempts[0].ProbeEvidence
			e.ProbeStartedAt = e.CooldownUntil.Add(-time.Second)
		}},
		{"half_open_still_open", "quota_pressure_offline", "half-open", func(r []BenchmarkCaseRun) { r[2].Attempts[0].ProbeEvidence.StateAfter = "OPEN" }},
		{"replay_missing_cost", "quota_pressure_offline", "zero replay cost", func(r []BenchmarkCaseRun) { r[4].Attempts[1].ReportedCost = nil }},
		{"replay_artifact", "quota_pressure_offline", "replay result", func(r []BenchmarkCaseRun) { r[4].Attempts[1].ProviderReply.ArtifactSHA256 = strings.Repeat("a", 64) }},
		{"replay_missing_artifact", "quota_pressure_offline", "replay result", func(r []BenchmarkCaseRun) {
			for i := range r[4].Attempts {
				r[4].Attempts[i].ProviderReply.ArtifactSHA256 = ""
			}
		}},
		{"replay_summary", "quota_pressure_offline", "replay result", func(r []BenchmarkCaseRun) { r[4].Attempts[1].ProviderReply.SummarySHA256 = strings.Repeat("a", 64) }},
		{"replay_verification", "quota_pressure_offline", "replay result", func(r []BenchmarkCaseRun) { r[4].Attempts[1].Verification.CheckResult.Status = "FAILED" }},
		{"replay_cost", "quota_pressure_offline", "zero replay cost", func(r []BenchmarkCaseRun) {
			*r[4].Attempts[1].ReportedCost = 0.02
			*r[4].ReportedCost += 0.02
		}},
		{"review_reply_artifact", "matched_profiles", "independent_verified", func(r []BenchmarkCaseRun) { r[8].Attempts[0].ProviderReply.ArtifactSHA256 = strings.Repeat("a", 64) }},
		{"replay_spawn", "quota_pressure_offline", "must not spawn", func(r []BenchmarkCaseRun) { r[4].Attempts[1].ProcessExitCode = new(int) }},
		{"third_worker", "quota_pressure_offline", "exactly", func(r []BenchmarkCaseRun) {
			a := r[4].Attempts[0]
			a.Number = 3
			r[4].Attempts = append(r[4].Attempts, a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite, err := LoadRoutingBenchmarkFixture("testdata/routing/" + tc.file + "_v1.json")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = EvaluateRoutingBenchmark(suite); err != nil {
				t.Fatal("invalid control:", err)
			}
			tc.mutate(suite.Runs)
			_, err = EvaluateRoutingBenchmark(suite)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("wanted %q, got %v", tc.reason, err)
			}
		})
	}
}

func TestRoutingBenchmarkTerminalCostAttribution(t *testing.T) {
	for _, terminalSuccess := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal_verified_%t", terminalSuccess), func(t *testing.T) {
			suite, err := LoadRoutingBenchmarkFixture("testdata/routing/cost_and_missing_data_v1.json")
			if err != nil {
				t.Fatal(err)
			}
			run := &suite.Runs[0]
			first, last := &run.Attempts[0], &run.Attempts[1]
			// An independently verified intermediate result on a different
			// account never contributes a second terminal success.
			first.ProcessExitCode = last.ProcessExitCode
			first.ProviderReply = last.ProviderReply
			first.Verification = last.Verification
			first.Account = "separate"
			first.CostEvidence.Account = first.Account
			last.Verification.IndependentVerified = terminalSuccess
			run.VerifiedSuccess = terminalSuccess
			report, err := EvaluateRoutingBenchmark(suite)
			if err != nil {
				t.Fatal(err)
			}
			profile := report.Profiles[BenchmarkProfileReference]
			var successes int
			for _, g := range profile.CostGroups {
				successes += g.VerifiedSuccessCount
				if g.CrossGroupRuns > 0 && g.CostPerVerifiedSuccess != nil {
					t.Fatalf("split scopes have a combined cost per success: %+v", g)
				}
				if g.Account == "separate" && (g.VerifiedSuccessCount != 0 || g.KnownTotalCost == nil || *g.KnownTotalCost != 0.05) {
					t.Fatalf("intermediate result: %+v", g)
				}
				if !terminalSuccess && g.Account == last.Account && (g.KnownFailedAttemptCost == nil || *g.KnownFailedAttemptCost != 0.10) {
					t.Fatalf("missing failed terminal cost: %+v", g)
				}
			}
			if successes != profile.VerifiedSuccessCount {
				t.Fatalf("group successes %d, profile %d", successes, profile.VerifiedSuccessCount)
			}
		})
	}
}

func TestRoutingBenchmarkIncompleteCostScope(t *testing.T) {
	for _, field := range []string{"account", "window", "stale quota", "missing retry cost"} {
		t.Run(field, func(t *testing.T) {
			suite, err := LoadRoutingBenchmarkFixture("testdata/routing/cost_and_missing_data_v1.json")
			if err != nil {
				t.Fatal(err)
			}
			run := &suite.Runs[0]
			a := &run.Attempts[0]
			switch field {
			case "account":
				a.Account = ""
				a.CostEvidence.Account = ""
			case "window":
				a.QuotaWindow = ""
			case "stale quota":
				a.CostUnit = "quota_percent_weekly"
				run.ReportedCost = nil
				run.QuotaProvenance.State = "STALE"
				run.QuotaProvenance.Fresh = false
				run.QuotaProvenance.RemainingPercent = nil
			case "missing retry cost":
				run.Attempts[1].ReportedCost = nil
				run.ReportedCost = nil
			}
			report, err := EvaluateRoutingBenchmark(suite)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range report.Profiles[BenchmarkProfileReference].CostGroups {
				if g.Account == a.Account && g.QuotaWindow == a.QuotaWindow && g.CostUnit == a.CostUnit {
					if g.Complete || g.KnownFailedAttemptCost == nil || *g.KnownFailedAttemptCost != 0.05 || g.CompleteTotalCost != nil || g.CostPerVerifiedSuccess != nil || g.MissingDataRatio == 0 {
						t.Fatalf("incomplete cost scope: %+v", g)
					}
					return
				}
			}
			t.Fatal("missing failed attempt group")
		})
	}
}

func TestRoutingBenchmarkCostPolicyScope(t *testing.T) {
	for _, policy := range []string{"METERED_API", "", "UNKNOWN"} {
		t.Run("policy_"+policy, func(t *testing.T) {
			suite, err := LoadRoutingBenchmarkFixture("testdata/routing/cost_and_missing_data_v1.json")
			if err != nil {
				t.Fatal(err)
			}
			for i := range suite.Runs[0].Attempts {
				suite.Runs[0].Attempts[i].CostEvidence.Policy = policy
			}
			report, err := EvaluateRoutingBenchmark(suite)
			if err != nil {
				t.Fatal(err)
			}
			for _, group := range report.Profiles[BenchmarkProfileReference].CostGroups {
				if group.Account != "codex-acct-a" {
					continue
				}
				complete := policy == "METERED_API"
				if group.VerifiedSuccessCount != 1 || group.CrossGroupRuns != 0 || group.KnownTotalCost == nil ||
					math.Abs(*group.KnownTotalCost-0.15) > 1e-9 || group.Complete != complete ||
					(group.CompleteTotalCost != nil) != complete || (group.CostPerVerifiedSuccess != nil) != complete {
					t.Fatalf("policy %q: %+v", policy, group)
				}
				return
			}
			t.Fatal("missing cost group")
		})
	}
}

func TestRoutingBenchmarkExhaustedQuotaScope(t *testing.T) {
	for _, scope := range []string{"matched", "other provider", "other account", "other window"} {
		t.Run(scope, func(t *testing.T) {
			suite, err := LoadRoutingBenchmarkFixture("testdata/routing/quota_pressure_offline_v1.json")
			if err != nil {
				t.Fatal(err)
			}
			// Attach the later fallback candidate's quota observation to the run.
			// Only an exact scope match can veto that worker.
			run := &suite.Runs[1]
			worker := run.Attempts[1]
			q := suite.Runs[0].QuotaProvenance
			q.Provider, q.Account, q.Window = worker.Provider, worker.Account, worker.QuotaWindow
			switch scope {
			case "other provider":
				q.Provider = "codex"
			case "other account":
				q.Account = "second-account"
			case "other window":
				q.Window = "five_hour"
			}
			run.QuotaProvenance = q
			_, err = EvaluateRoutingBenchmark(suite)
			if scope == "matched" {
				if err == nil || !strings.Contains(err.Error(), "exhausted quota scope") {
					t.Fatalf("expected exhausted quota scope rejection, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRoutingBenchmarkNumericQuotaRequiresExplicitProvider(t *testing.T) {
	suite, err := LoadRoutingBenchmarkFixture("testdata/routing/matched_profiles_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for i := range suite.Runs {
		run := &suite.Runs[i]
		if run.Profile != BenchmarkProfilePricePerformance || run.QuotaProvenance.State != "KNOWN" {
			continue
		}
		run.QuotaProvenance.Provider = ""
		if _, err := EvaluateRoutingBenchmark(suite); err == nil || !strings.Contains(err.Error(), "numeric quota requires provider") {
			t.Fatalf("numeric quota without an observed provider must be rejected, got %v", err)
		}
		return
	}
	t.Fatal("fixture lacks a numeric price-performance quota")
}
