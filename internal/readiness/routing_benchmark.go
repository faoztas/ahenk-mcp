// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	BenchmarkProfileReference        = "reference"
	BenchmarkProfilePricePerformance = "price_performance"
	BenchmarkProfileQuality          = "quality"
	BenchmarkProfilePrice            = "price"

	BenchmarkCostUnknown = "UNKNOWN"

	BenchmarkSamplePilot = "pilot"
	BenchmarkSampleFull  = "full"
)

var benchmarkProfiles = []string{
	BenchmarkProfileReference,
	BenchmarkProfilePricePerformance,
	BenchmarkProfileQuality,
	BenchmarkProfilePrice,
}

var benchmarkRoles = []string{
	"coding",
	"debugging",
	"code_review",
	"planning",
}

type BenchmarkSuite struct {
	FixtureVersion string             `json:"fixture_version"`
	SchemaVersion  string             `json:"schema_version"`
	Protocol       BenchmarkProtocol  `json:"protocol"`
	Runs           []BenchmarkCaseRun `json:"runs"`
}

type BenchmarkProtocol struct {
	PreRegistered    bool                     `json:"pre_registered"`
	Hypothesis       string                   `json:"hypothesis"`
	PrimaryMetric    string                   `json:"primary_metric"`
	DriftThresholds  BenchmarkDriftThresholds `json:"drift_thresholds"`
	BudgetLimit      BenchmarkBudgetLimit     `json:"budget_limit"`
	SamplePlan       BenchmarkSamplePlan      `json:"sample_plan"`
	RecordedAt       time.Time                `json:"recorded_at"`
	Environment      BenchmarkEnvironment     `json:"environment"`
	ClaimSuperiority bool                     `json:"claim_superiority,omitempty"`
}

type BenchmarkDriftThresholds struct {
	FPMaxAccuracyDropPP        float64 `json:"fp_max_accuracy_drop_pp"`
	FPMinMeasurableCostGainPct float64 `json:"fp_min_measurable_cost_gain_pct"`
	FPMaxP95LatencyIncreasePct float64 `json:"fp_max_p95_latency_increase_pct"`
	QualityMinSuccessRate      float64 `json:"quality_min_success_rate,omitempty"`
	PriceMaxExtraSpend         float64 `json:"price_max_extra_spend"`
}

type BenchmarkBudgetLimit struct {
	MaxTotalCalls             int     `json:"max_total_calls"`
	MaxPaidSpend              float64 `json:"max_paid_spend"`
	AllowLiveQuotaConsumption bool    `json:"allow_live_quota_consumption"`
}

type BenchmarkSamplePlan struct {
	MinMatchedCasesPerProfile int      `json:"min_matched_cases_per_profile"`
	MinCasesPerRole           int      `json:"min_cases_per_role,omitempty"`
	RequiredRoles             []string `json:"required_roles,omitempty"`
}

type BenchmarkEnvironment struct {
	CPU                   string `json:"cpu"`
	Platform              string `json:"platform"`
	Worktree              string `json:"worktree"`
	ProviderMode          string `json:"provider_mode"`
	StdoutJSONIngestionMS *int64 `json:"stdout_json_ingestion_ms,omitempty"`
	ObserverIOMS          *int64 `json:"observer_io_ms,omitempty"`
}

type BenchmarkAcceptanceRubric struct {
	RubricVersion       string   `json:"rubric_version"`
	GoCheck             string   `json:"go_check,omitempty"`
	ArtifactExpectation string   `json:"artifact_expectation,omitempty"`
	ExpectedVerdict     string   `json:"expected_verdict,omitempty"`
	ArtifactSHA256      string   `json:"artifact_sha256,omitempty"`
	RequireDAGValid     bool     `json:"require_dag_valid,omitempty"`
	RequiredPlanNodes   []string `json:"required_plan_nodes,omitempty"`
}

type BenchmarkPlanNode struct {
	Key       string   `json:"key"`
	DependsOn []string `json:"depends_on,omitempty"`
}

type BenchmarkQuotaProvenance struct {
	Provider         string      `json:"provider,omitempty"`
	Source           string      `json:"source"`
	Account          string      `json:"account,omitempty"`
	AccountBound     bool        `json:"account_bound"`
	Fresh            bool        `json:"fresh"`
	State            string      `json:"state"`
	Window           string      `json:"window,omitempty"`
	RemainingPercent *float64    `json:"remaining_percent,omitempty"`
	RouteQuota       *RouteQuota `json:"route_quota,omitempty"`
}

type BenchmarkProviderReply struct {
	Status          string `json:"status"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	SummarySHA256   string `json:"summary_sha256,omitempty"`
	ArtifactSHA256  string `json:"artifact_sha256,omitempty"`
	ReportedSuccess bool   `json:"reported_success,omitempty"`
}

type BenchmarkVerification struct {
	CheckResult         *model.CheckEvidence `json:"check_result,omitempty"`
	ReviewResult        *model.ReviewResult  `json:"review_result,omitempty"`
	PlanNodes           []BenchmarkPlanNode  `json:"plan_nodes,omitempty"`
	IndependentVerified bool                 `json:"independent_verified"`
}

// BenchmarkProbeEvidence records the circuit cooldown and the observed probe
// transition for the provider on this attempt.
type BenchmarkProbeEvidence struct {
	CooldownUntil  time.Time `json:"cooldown_until"`
	ProbeStartedAt time.Time `json:"probe_started_at"`
	StateAfter     string    `json:"state_after"`
}

type BenchmarkAttempt struct {
	// ObservedRoute records worker identity and effective scope independently of any pricing receipt.
	ObservedRoute      *model.CostRoute        `json:"observed_route,omitempty"`
	ProbeEvidence      *BenchmarkProbeEvidence `json:"probe_evidence,omitempty"`
	Number             int                     `json:"number"`
	RequestID          string                  `json:"request_id,omitempty"`
	JobID              string                  `json:"job_id,omitempty"`
	Provider           string                  `json:"provider"`
	Model              string                  `json:"model"`
	Effort             string                  `json:"effort,omitempty"`
	Account            string                  `json:"account,omitempty"`
	QuotaWindow        string                  `json:"quota_window,omitempty"`
	RouteStatus        string                  `json:"route_status,omitempty"`
	RateLimitScope     string                  `json:"rate_limit_scope,omitempty"`
	RecoveryState      string                  `json:"recovery_state,omitempty"`
	ProcessExitCode    *int                    `json:"process_exit_code,omitempty"`
	ProviderReply      BenchmarkProviderReply  `json:"provider_reply"`
	Verification       BenchmarkVerification   `json:"verification"`
	LatencyMS          int64                   `json:"latency_ms"`
	CostUnit           string                  `json:"cost_unit,omitempty"`
	ReportedCost       *float64                `json:"reported_cost,omitempty"`
	ReportedCostStatus string                  `json:"-"`
	CostEvidence       *model.CostEvidence     `json:"cost_evidence,omitempty"`
}

func (a *BenchmarkAttempt) UnmarshalJSON(data []byte) error {
	type alias BenchmarkAttempt
	var raw struct {
		alias
		ReportedCost json.RawMessage `json:"reported_cost,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing data in benchmark attempt")
	}
	cost, status, err := parseReportedCost(raw.ReportedCost)
	if err != nil {
		return err
	}
	*a = BenchmarkAttempt(raw.alias)
	a.ReportedCost = cost
	a.ReportedCostStatus = status
	return nil
}

type BenchmarkCaseRun struct {
	FixtureVersion     string                    `json:"fixture_version"`
	SchemaVersion      string                    `json:"schema_version"`
	CaseID             string                    `json:"case_id"`
	Role               string                    `json:"role"`
	TaskVariant        string                    `json:"task_variant"`
	RepoPolicyVersion  string                    `json:"repo_policy_version"`
	AcceptanceRubric   BenchmarkAcceptanceRubric `json:"acceptance_rubric"`
	ReferenceBaseline  string                    `json:"reference_baseline"`
	Profile            string                    `json:"profile"`
	RunOrder           int                       `json:"run_order"`
	Scenario           string                    `json:"scenario,omitempty"`
	QuotaProvenance    BenchmarkQuotaProvenance  `json:"quota_provenance"`
	Attempts           []BenchmarkAttempt        `json:"attempts"`
	VerifiedSuccess    bool                      `json:"verified_success"`
	LatencyMS          int64                     `json:"latency_ms"`
	CostUnit           string                    `json:"cost_unit"`
	ReportedCost       *float64                  `json:"reported_cost,omitempty"`
	ReportedCostStatus string                    `json:"-"`
	CostEvidence       *model.CostEvidence       `json:"cost_evidence,omitempty"`
}

func (r *BenchmarkCaseRun) UnmarshalJSON(data []byte) error {
	type alias BenchmarkCaseRun
	var raw struct {
		alias
		ReportedCost json.RawMessage `json:"reported_cost,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing data in benchmark case run")
	}
	cost, status, err := parseReportedCost(raw.ReportedCost)
	if err != nil {
		return err
	}
	*r = BenchmarkCaseRun(raw.alias)
	r.ReportedCost = cost
	r.ReportedCostStatus = status
	return nil
}

func parseReportedCost(raw json.RawMessage) (*float64, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, BenchmarkCostUnknown, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		if s != BenchmarkCostUnknown {
			return nil, "", fmt.Errorf("invalid reported_cost string %q", s)
		}
		return nil, BenchmarkCostUnknown, nil
	}
	var v float64
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return nil, "", fmt.Errorf("invalid reported_cost value: %w", err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil, "", fmt.Errorf("reported_cost must be a finite non-negative number")
	}
	return &v, "KNOWN", nil
}

type CostGroupSummary struct {
	Provider                    string   `json:"provider"`
	Profile                     string   `json:"profile"`
	CostUnit                    string   `json:"cost_unit"`
	Account                     string   `json:"account"`
	QuotaWindow                 string   `json:"quota_window,omitempty"`
	Policy                      string   `json:"policy,omitempty"`
	RawSampleCount              int      `json:"raw_sample_count"`
	AttemptCount                int      `json:"attempt_count"`
	VerifiedSuccessCount        int      `json:"verified_success_count"`
	VerifiedSuccesses           int      `json:"verified_successes"`
	FailedAttemptCount          int      `json:"failed_attempt_count"`
	KnownCostAttemptCount       int      `json:"known_cost_attempt_count"`
	UnknownCostAttemptCount     int      `json:"unknown_cost_attempt_count"`
	UnknownCostAttempts         int      `json:"unknown_cost_attempts"`
	CostStatus                  string   `json:"cost_status"`
	Complete                    bool     `json:"complete"`
	TotalMeasurableCost         *float64 `json:"total_measurable_cost,omitempty"`
	FailedAttemptMeasurableCost *float64 `json:"failed_attempt_measurable_cost,omitempty"`
	KnownTotalCost              *float64 `json:"known_total_cost,omitempty"`
	KnownFailedAttemptCost      *float64 `json:"known_failed_attempt_cost,omitempty"`
	CompleteTotalCost           *float64 `json:"complete_total_cost,omitempty"`
	CostPerVerifiedSuccess      *float64 `json:"cost_per_verified_success,omitempty"`
	CrossGroupRuns              int      `json:"cross_group_runs"`
	P50MS                       int64    `json:"p50_ms"`
	P95MS                       int64    `json:"p95_ms"`
	MissingDataCount            int      `json:"missing_data_count"`
	MissingDataRatio            float64  `json:"missing_data_ratio"`
}

type ProfileBenchmarkSummary struct {
	Profile               string             `json:"profile"`
	RawSampleCount        int                `json:"raw_sample_count"`
	VerifiedSuccessCount  int                `json:"verified_success_count"`
	VerifiedSuccessRate   float64            `json:"verified_success_rate"`
	SuccessRate           float64            `json:"success_rate"`
	QualityApprovedCases  int                `json:"quality_approved_cases"`
	QualityEvaluatedCases int                `json:"quality_evaluated_cases"`
	QualityApprovalRate   *float64           `json:"quality_approval_rate"`
	P50MS                 int64              `json:"p50_ms"`
	P95MS                 int64              `json:"p95_ms"`
	MissingDataCount      int                `json:"missing_data_count"`
	MissingDataRatio      float64            `json:"missing_data_ratio"`
	UnknownCostCount      int                `json:"unknown_cost_count"`
	UnknownQuotaCount     int                `json:"unknown_quota_count"`
	FailedAttemptCount    int                `json:"failed_attempt_count"`
	CostGroups            []CostGroupSummary `json:"cost_groups"`
}

type QuotaPressureSummary struct {
	Scenario                  string `json:"scenario"`
	RunCount                  int    `json:"run_count"`
	VerifiedSuccessCount      int    `json:"verified_success_count"`
	BlockedSpawnCount         int    `json:"blocked_spawn_count"`
	FallbackSuccessCount      int    `json:"fallback_success_count"`
	HalfOpenProbeSuccessCount int    `json:"half_open_probe_success_count"`
	IdempotentReplayCount     int    `json:"idempotent_replay_count"`
}

type AcceptancePolicy struct {
	MinSamplesPerProfile   int     `json:"min_samples_per_profile"`
	MinQualityFloor        float64 `json:"min_quality_floor"`
	FPMaxAccuracyDropPP    float64 `json:"fp_max_accuracy_drop_pp"`
	FPMinCostGainPct       float64 `json:"fp_min_cost_gain_pct"`
	FPMaxP95IncreasePct    float64 `json:"fp_max_p95_increase_pct"`
	PriceMaxExtraSpend     float64 `json:"price_max_extra_spend"`
	RequireSameCostUnit    bool    `json:"require_same_cost_unit"`
	RequirePreRegistration bool    `json:"require_pre_registration"`
}

type BenchmarkReport struct {
	FixtureVersion              string                             `json:"fixture_version"`
	SchemaVersion               string                             `json:"schema_version"`
	RepoPolicyVersion           string                             `json:"repo_policy_version"`
	Protocol                    BenchmarkProtocol                  `json:"protocol"`
	SampleClass                 string                             `json:"sample_class"`
	ProtocolEligible            bool                               `json:"protocol_eligible"`
	SuperiorityClaimAllowed     bool                               `json:"superiority_claim_allowed"`
	SuperiorityRejectionReasons []string                           `json:"superiority_rejection_reasons,omitempty"`
	MatchedCaseCount            int                                `json:"matched_case_count"`
	RolesCovered                []string                           `json:"roles_covered"`
	HasDefectiveReviewCase      bool                               `json:"has_defective_review_case"`
	HasCleanReviewCase          bool                               `json:"has_clean_review_case"`
	HasValidPlanningDAGCase     bool                               `json:"has_valid_planning_dag_case"`
	Profiles                    map[string]ProfileBenchmarkSummary `json:"profiles"`
	QuotaPressureScenarios      map[string]QuotaPressureSummary    `json:"quota_pressure_scenarios,omitempty"`
}

func LoadRoutingBenchmarkFixture(path string) (BenchmarkSuite, error) {
	f, err := os.Open(path)
	if err != nil {
		return BenchmarkSuite{}, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 4<<20))
	dec.DisallowUnknownFields()
	var suite BenchmarkSuite
	if err := dec.Decode(&suite); err != nil {
		return BenchmarkSuite{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return BenchmarkSuite{}, fmt.Errorf("unexpected trailing JSON data in %s", path)
	}
	return suite, nil
}

func EvaluateRoutingBenchmark(suite BenchmarkSuite) (BenchmarkReport, error) {
	if strings.TrimSpace(suite.FixtureVersion) == "" || strings.TrimSpace(suite.SchemaVersion) == "" {
		return BenchmarkReport{}, errors.New("fixture_version and schema_version are required")
	}
	if len(suite.Runs) == 0 {
		return BenchmarkReport{}, errors.New("benchmark suite has no runs")
	}
	if suite.Protocol.BudgetLimit.AllowLiveQuotaConsumption {
		return BenchmarkReport{}, errors.New("offline benchmark fixture must not consume live quota")
	}

	var caseOrder []string
	runsByCase := make(map[string][]BenchmarkCaseRun)
	roleSet := make(map[string]bool)
	repoPolicyVersion := suite.Runs[0].RepoPolicyVersion
	reference := suite.Runs[0].ReferenceBaseline

	for idx, run := range suite.Runs {
		if run.FixtureVersion != suite.FixtureVersion || run.SchemaVersion != suite.SchemaVersion {
			return BenchmarkReport{}, fmt.Errorf("run %d fixture/schema version mismatch", idx)
		}
		if !model.ValidID(run.CaseID) || !slices.Contains(benchmarkRoles, run.Role) ||
			strings.TrimSpace(run.TaskVariant) == "" || strings.TrimSpace(run.RepoPolicyVersion) == "" ||
			strings.TrimSpace(run.ReferenceBaseline) == "" || !slices.Contains(benchmarkProfiles, run.Profile) ||
			strings.TrimSpace(run.CostUnit) == "" || run.LatencyMS <= 0 {
			return BenchmarkReport{}, fmt.Errorf("run %d (%s/%s) has invalid required fields", idx, run.CaseID, run.Profile)
		}
		if repoPolicyVersion != run.RepoPolicyVersion {
			return BenchmarkReport{}, fmt.Errorf("case %s: mismatched repo_policy_version", run.CaseID)
		}
		if reference != run.ReferenceBaseline {
			return BenchmarkReport{}, fmt.Errorf("case %s: mismatched reference_baseline", run.CaseID)
		}
		if err := validateBenchmarkRun(run); err != nil {
			return BenchmarkReport{}, fmt.Errorf("case %s profile %s: %w", run.CaseID, run.Profile, err)
		}
		if _, exists := runsByCase[run.CaseID]; !exists {
			caseOrder = append(caseOrder, run.CaseID)
		}
		runsByCase[run.CaseID] = append(runsByCase[run.CaseID], run)
		roleSet[run.Role] = true
	}

	// Verify matched 4-profile structure and balanced run_order.
	runOrderCounts := make(map[string]map[int]int, len(benchmarkProfiles))
	for _, p := range benchmarkProfiles {
		runOrderCounts[p] = make(map[int]int, 4)
	}

	var hasDefectiveReview, hasCleanReview, hasValidPlanDAG bool

	for _, caseID := range caseOrder {
		caseRuns := runsByCase[caseID]
		if len(caseRuns) != len(benchmarkProfiles) {
			return BenchmarkReport{}, fmt.Errorf("case %s must have %d matched profile runs, got %d",
				caseID, len(benchmarkProfiles), len(caseRuns))
		}
		base := caseRuns[0]
		baseRubricDigest := model.Digest(base.AcceptanceRubric)
		seenProfiles := make(map[string]bool, 4)
		seenOrders := make(map[int]bool, 4)

		for _, r := range caseRuns {
			if seenProfiles[r.Profile] {
				return BenchmarkReport{}, fmt.Errorf("case %s has duplicate profile %s", caseID, r.Profile)
			}
			seenProfiles[r.Profile] = true
			if r.Role != base.Role {
				return BenchmarkReport{}, fmt.Errorf("case %s has mismatched role (%s vs %s)", caseID, r.Role, base.Role)
			}
			if r.TaskVariant != base.TaskVariant {
				return BenchmarkReport{}, fmt.Errorf("case %s has mismatched task_variant (%s vs %s)", caseID, r.TaskVariant, base.TaskVariant)
			}
			if r.RepoPolicyVersion != base.RepoPolicyVersion {
				return BenchmarkReport{}, fmt.Errorf("case %s has mismatched repo_policy_version (%s vs %s)", caseID, r.RepoPolicyVersion, base.RepoPolicyVersion)
			}
			if r.ReferenceBaseline != base.ReferenceBaseline {
				return BenchmarkReport{}, fmt.Errorf("case %s has mismatched reference_baseline (%s vs %s)", caseID, r.ReferenceBaseline, base.ReferenceBaseline)
			}
			if model.Digest(r.AcceptanceRubric) != baseRubricDigest {
				return BenchmarkReport{}, fmt.Errorf("case %s has mismatched acceptance_rubric across profiles", caseID)
			}
			if r.RunOrder < 1 || r.RunOrder > 4 || seenOrders[r.RunOrder] {
				return BenchmarkReport{}, fmt.Errorf("case %s has invalid or duplicate run_order %d", caseID, r.RunOrder)
			}
			seenOrders[r.RunOrder] = true
			runOrderCounts[r.Profile][r.RunOrder]++
		}

		for _, p := range benchmarkProfiles {
			if !seenProfiles[p] {
				return BenchmarkReport{}, fmt.Errorf("case %s is missing profile %s", caseID, p)
			}
		}

		switch base.Role {
		case "code_review":
			if base.AcceptanceRubric.ArtifactExpectation == "defective" {
				hasDefectiveReview = true
			} else if base.AcceptanceRubric.ArtifactExpectation == "clean" {
				hasCleanReview = true
			}
		case "planning":
			if base.AcceptanceRubric.RequireDAGValid {
				hasValidPlanDAG = true
			}
		}
	}

	numCases := len(caseOrder)
	if numCases >= 2 {
		minPosCount := numCases / 4
		maxPosCount := (numCases + 3) / 4
		for _, p := range benchmarkProfiles {
			for pos := 1; pos <= 4; pos++ {
				cnt := runOrderCounts[p][pos]
				if cnt < minPosCount || cnt > maxPosCount {
					return BenchmarkReport{}, fmt.Errorf("unbalanced run_order for profile %s at position %d: count=%d outside [%d,%d]",
						p, pos, cnt, minPosCount, maxPosCount)
				}
			}
		}
	}

	var rolesCovered []string
	for _, r := range benchmarkRoles {
		if roleSet[r] {
			rolesCovered = append(rolesCovered, r)
		}
	}

	// Aggregate per-profile and per-cost-group summaries without blending F/P, Quality, and Price.
	profiles := make(map[string]ProfileBenchmarkSummary, len(benchmarkProfiles))
	for _, p := range benchmarkProfiles {
		profiles[p] = aggregateProfileSummary(p, suite.Runs)
	}

	// Aggregate quota-pressure scenarios.
	scenarios := aggregateQuotaPressureScenarios(suite.Runs)

	// Per-role counts (distinct matched cases).
	roleCounts := make(map[string]int)
	for _, caseID := range caseOrder {
		roleCounts[runsByCase[caseID][0].Role]++
	}

	// Evaluate protocol pre-registration and sample size boundaries.
	sampleReasons := evaluateSamplePlan(suite.Protocol.SamplePlan, numCases, roleCounts)
	sampleClass := BenchmarkSampleFull
	if len(sampleReasons) > 0 || suite.Protocol.Environment.ProviderMode == "offline_simulated" {
		sampleClass = BenchmarkSamplePilot
	}
	rejectionReasons := append(protocolMetadataRejections(suite.Protocol), sampleReasons...)
	if suite.Protocol.Environment.ProviderMode == "offline_simulated" {
		rejectionReasons = append(rejectionReasons, "OFFLINE_SIMULATION")
	}
	protocolEligible := len(rejectionReasons) == 0
	// This protocol describes the observations. Profile-specific acceptance against
	// a comparable reference and measured thresholds belongs to the B2 evaluator.
	rejectionReasons = append(rejectionReasons, "MEASURED_COMPARISON_REQUIRED")
	if suite.Protocol.ClaimSuperiority {
		return BenchmarkReport{}, fmt.Errorf("unsupported superiority claim rejected: %s", strings.Join(rejectionReasons, ", "))
	}

	return BenchmarkReport{
		FixtureVersion:              suite.FixtureVersion,
		SchemaVersion:               suite.SchemaVersion,
		RepoPolicyVersion:           repoPolicyVersion,
		Protocol:                    suite.Protocol,
		SampleClass:                 sampleClass,
		ProtocolEligible:            protocolEligible,
		SuperiorityClaimAllowed:     false,
		SuperiorityRejectionReasons: rejectionReasons,
		MatchedCaseCount:            numCases,
		RolesCovered:                rolesCovered,
		HasDefectiveReviewCase:      hasDefectiveReview,
		HasCleanReviewCase:          hasCleanReview,
		HasValidPlanningDAGCase:     hasValidPlanDAG,
		Profiles:                    profiles,
		QuotaPressureScenarios:      scenarios,
	}, nil
}

func validateBenchmarkRun(run BenchmarkCaseRun) error {
	if len(run.Attempts) == 0 {
		return errors.New("at least one attempt is required")
	}
	if err := validateAcceptanceRubric(run.Role, run.AcceptanceRubric); err != nil {
		return err
	}
	if err := validateQuotaProvenance(run.QuotaProvenance); err != nil {
		return err
	}

	q := run.QuotaProvenance
	referenceRoute := strings.Split(run.ReferenceBaseline, ":")
	for i, a := range run.Attempts {
		if !knownIdentity(a.Provider) || !knownIdentity(a.Model) {
			return fmt.Errorf("attempt %d missing provider/model", a.Number)
		}
		if run.Profile == BenchmarkProfileReference {
			if len(referenceRoute) != 3 || !knownIdentity(referenceRoute[2]) || referenceRoute[0] != a.Provider || referenceRoute[1] != a.Model || referenceRoute[2] != a.Effort {
				return fmt.Errorf("attempt %d differs from fixed reference route %q", a.Number, run.ReferenceBaseline)
			}
		}
		if a.RouteStatus == "IDEMPOTENT_REPLAY" && run.Scenario != "RETRY_IDEMPOTENCY" {
			return errors.New("unexpected replay outside RETRY_IDEMPOTENCY")
		}
		if a.ReportedCost != nil && !knownIdentity(a.CostUnit) {
			return fmt.Errorf("attempt %d numeric cost requires an explicit cost_unit", a.Number)
		}
		if a.CostEvidence != nil && a.CostEvidence.Account != "" && a.CostEvidence.Account != a.Account {
			return fmt.Errorf("attempt %d cost evidence account mismatch", a.Number)
		}
		if a.Number != i+1 {
			return fmt.Errorf("attempt numbers must be sequential starting at 1, got %d at index %d", a.Number, i)
		}
		if a.LatencyMS <= 0 {
			return fmt.Errorf("attempt %d must have positive latency_ms", a.Number)
		}
		if a.ReportedCost != nil && (*a.ReportedCost < 0 || math.IsNaN(*a.ReportedCost) || math.IsInf(*a.ReportedCost, 0)) {
			return fmt.Errorf("attempt %d has invalid reported_cost", a.Number)
		}
		notSpawnedRoute := a.RouteStatus == "EXHAUSTED_0PCT" || a.RouteStatus == "NO_ELIGIBLE_ROUTE" ||
			a.RouteStatus == "IDEMPOTENT_REPLAY" || a.ProviderReply.Status == "NOT_SPAWNED"
		if notSpawnedRoute && a.ProcessExitCode != nil {
			return fmt.Errorf("attempt %d with route_status=%s / reply=%s must not spawn a worker process",
				a.Number, a.RouteStatus, a.ProviderReply.Status)
		}

		if q.State == "EXHAUSTED" && a.Provider == q.Provider && a.Account == q.Account &&
			a.QuotaWindow == q.Window && (a.ProviderReply.Status != "NOT_SPAWNED" || a.ProcessExitCode != nil) {
			return errors.New("spawn on exhausted quota scope")
		}

		verified, err := verifyAttemptOutcome(run, a, run.Attempts[:i])
		if err != nil {
			return fmt.Errorf("attempt %d: %w", a.Number, err)
		}
		if a.Verification.IndependentVerified && !verified {
			return fmt.Errorf("attempt %d claims independent_verified without passing verification checks", a.Number)
		}
	}

	if run.Scenario == "EXHAUSTED_0PCT" {
		first := run.Attempts[0]
		if q.Provider != first.Provider || q.Account != first.Account || q.Window != first.QuotaWindow {
			return errors.New("quota provider, account and window must match blocked candidate")
		}
		if run.QuotaProvenance.State != "EXHAUSTED" || !run.QuotaProvenance.AccountBound || !run.QuotaProvenance.Fresh ||
			first.RouteStatus != "EXHAUSTED_0PCT" || first.ProcessExitCode != nil || first.ProviderReply.Status != "NOT_SPAWNED" {
			return errors.New("EXHAUSTED_0PCT scenario requires fresh matched 0% quota and 0 process spawns on the exhausted candidate")
		}
	}

	if run.Scenario == "FALLBACK_429" {
		if len(run.Attempts) < 2 {
			return errors.New("FALLBACK_429 scenario requires initial 429 and successful fallback")
		}
		first := run.Attempts[0]
		validScopes := map[string]bool{"account": true, "model": true, "provider": true, "route": true}
		if first.RouteStatus != "FALLBACK_429" || first.ProviderReply.Status != "HTTP_429" || first.ProviderReply.HTTPStatus != 429 || !validScopes[first.RateLimitScope] {
			return errors.New("FALLBACK_429 scenario requires initial attempt failing with scoped HTTP 429")
		}
		fallbackSucceeded := false
		for i := 1; i < len(run.Attempts); i++ {
			a := run.Attempts[i]
			if fallbackBlocked(first, a) {
				return errors.New("FALLBACK_429 scenario requires fallback outside the blocked scope")
			}
			ok, _ := verifyAttemptOutcome(run, a, run.Attempts[:i])
			if a.ProviderReply.Status == "OK" && a.ProcessExitCode != nil && *a.ProcessExitCode == 0 && ok {
				fallbackSucceeded = true
			}
		}
		if !fallbackSucceeded || !run.VerifiedSuccess {
			return errors.New("FALLBACK_429 scenario requires successful fallback")
		}
	}

	if run.Scenario == "HALF_OPEN_RECOVERY" {
		if !run.VerifiedSuccess || !slices.ContainsFunc(run.Attempts, successfulHalfOpenProbe) {
			return errors.New("HALF_OPEN_RECOVERY requires evidence of completed half-open recovery")
		}
	}

	if run.Scenario == "NO_ELIGIBLE_ROUTE" {
		if run.VerifiedSuccess {
			return errors.New("NO_ELIGIBLE_ROUTE scenario must not succeed")
		}
		for _, a := range run.Attempts {
			if a.Verification.IndependentVerified {
				return errors.New("NO_ELIGIBLE_ROUTE scenario must have zero successful attempts")
			}
			if a.RouteStatus == "SELECTED" || a.ProviderReply.Status == "OK" {
				return errors.New("NO_ELIGIBLE_ROUTE scenario must not select an eligible route")
			}
		}
		last := run.Attempts[len(run.Attempts)-1]
		if last.RouteStatus != "NO_ELIGIBLE_ROUTE" || last.ProviderReply.Status != "NOT_SPAWNED" {
			return errors.New("NO_ELIGIBLE_ROUTE scenario requires terminal no-eligible-route failure")
		}
	}

	if run.Scenario == "RETRY_IDEMPOTENCY" {
		if len(run.Attempts) != 2 {
			return errors.New("RETRY_IDEMPOTENCY requires exactly one initial attempt and one replay")
		}
		first, replay := run.Attempts[0], run.Attempts[1]
		if first.RequestID == "" || first.JobID == "" || first.RequestID != replay.RequestID || first.JobID != replay.JobID ||
			first.Provider != replay.Provider || first.Model != replay.Model || first.Effort != replay.Effort || first.Account != replay.Account || first.QuotaWindow != replay.QuotaWindow || first.ProcessExitCode == nil ||
			replay.RouteStatus != "IDEMPOTENT_REPLAY" || replay.ProcessExitCode != nil || replay.ProviderReply.Status != "NOT_SPAWNED" {
			return errors.New("RETRY_IDEMPOTENCY scenario must preserve request_id, job_id, and first route decision without spawning a second worker")
		}
	}

	if err := validateReportedRunCost(run); err != nil {
		return err
	}
	if run.VerifiedSuccess != run.Attempts[terminalAttemptIndex(run)].Verification.IndependentVerified {
		return errors.New("verified_success differs from independently verified terminal outcome")
	}
	return nil
}

// A supplied scalar must reconcile with the attempt ledger in its own
// unit. Other-unit zeroes contribute nothing. Missing attempt costs remain
// missing even if the caller also supplied a run total.
func validateReportedRunCost(run BenchmarkCaseRun) error {
	if run.ReportedCost == nil {
		return nil
	}
	total := *run.ReportedCost
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return errors.New("invalid run reported_cost")
	}
	var subtotal float64
	complete := true
	matchedUnit := false
	for _, a := range run.Attempts {
		if a.CostUnit == run.CostUnit {
			matchedUnit = true
		}
		if a.ReportedCost == nil {
			complete = false
			continue
		}
		if a.CostUnit != run.CostUnit {
			if *a.ReportedCost != 0 {
				return errors.New("run reported_cost cannot combine different cost units")
			}
			continue
		}
		subtotal += *a.ReportedCost
	}
	tolerance := 1e-12 * max(1, total, subtotal)
	if !matchedUnit || subtotal-total > tolerance || (complete && math.Abs(subtotal-total) > tolerance) {
		return errors.New("run reported_cost does not reconcile with attempt costs")
	}
	return nil
}

func terminalAttemptIndex(run BenchmarkCaseRun) int {
	if run.Scenario == "RETRY_IDEMPOTENCY" {
		return 0
	}
	return len(run.Attempts) - 1
}

func fallbackBlocked(blocked, next BenchmarkAttempt) bool {
	if next.Provider != blocked.Provider {
		return false
	}
	switch blocked.RateLimitScope {
	case "provider":
		return true
	case "account":
		return !knownIdentity(blocked.Account) || !knownIdentity(next.Account) || next.Account == blocked.Account
	case "model":
		return next.Model == blocked.Model
	default:
		return next.Model == blocked.Model && next.Account == blocked.Account && next.Effort == blocked.Effort
	}
}

func knownIdentity(s string) bool {
	return strings.TrimSpace(s) != "" && !strings.EqualFold(strings.TrimSpace(s), BenchmarkCostUnknown)
}

func validateAcceptanceRubric(role string, rubric BenchmarkAcceptanceRubric) error {
	if strings.TrimSpace(rubric.RubricVersion) == "" {
		return errors.New("acceptance_rubric.rubric_version is required")
	}
	switch role {
	case "coding", "debugging":
		if !model.ValidGoCheck(rubric.GoCheck) {
			return fmt.Errorf("role %s requires a valid Go test check name, got %q", role, rubric.GoCheck)
		}
	case "code_review":
		if !slices.Contains([]string{"defective", "clean"}, rubric.ArtifactExpectation) {
			return fmt.Errorf("code_review rubric requires artifact_expectation 'defective' or 'clean', got %q", rubric.ArtifactExpectation)
		}
		wantVerdict := "APPROVED"
		if rubric.ArtifactExpectation == "defective" {
			wantVerdict = "CHANGES_REQUESTED"
		}
		if rubric.ExpectedVerdict != wantVerdict || !digest.MatchString(rubric.ArtifactSHA256) {
			return fmt.Errorf("code_review rubric has mismatched expected_verdict %q or invalid artifact_sha256", rubric.ExpectedVerdict)
		}
	case "planning":
		if !rubric.RequireDAGValid || len(rubric.RequiredPlanNodes) == 0 {
			return errors.New("planning rubric requires require_dag_valid and non-empty required_plan_nodes")
		}
	default:
		return fmt.Errorf("unsupported benchmark role %q", role)
	}
	return nil
}

func validateQuotaProvenance(q BenchmarkQuotaProvenance) error {
	if strings.TrimSpace(q.Source) == "" || !slices.Contains([]string{"KNOWN", "EXHAUSTED", "STALE", "UNKNOWN"}, q.State) {
		return fmt.Errorf("invalid quota_provenance source %q or state %q", q.Source, q.State)
	}
	if q.Provider != "" && !knownIdentity(q.Provider) {
		return errors.New("invalid quota provider")
	}
	if (q.State == "KNOWN" || q.State == "EXHAUSTED") && (!knownIdentity(q.Provider) || !knownIdentity(q.Account) || !knownIdentity(q.Window)) {
		return errors.New("numeric quota requires provider, account and window")
	}
	switch q.State {
	case "KNOWN":
		if !q.AccountBound || !q.Fresh || q.RemainingPercent == nil || math.IsNaN(*q.RemainingPercent) || *q.RemainingPercent <= 0 || *q.RemainingPercent > 100 {
			return errors.New("KNOWN quota state requires account_bound, fresh, and remaining_percent in (0, 100]")
		}
	case "EXHAUSTED":
		if !q.AccountBound || !q.Fresh || q.RemainingPercent == nil || *q.RemainingPercent != 0 {
			return errors.New("EXHAUSTED quota state requires account_bound, fresh, and remaining_percent == 0")
		}
	case "STALE", "UNKNOWN":
		if q.RemainingPercent != nil || (q.RouteQuota != nil && (q.RouteQuota.Remaining != nil || q.RouteQuota.ControllerRemaining != nil)) {
			return errors.New("stale or unknown quota must not carry numeric remaining_percent")
		}
	}
	return nil
}

func verifyAttemptOutcome(run BenchmarkCaseRun, a BenchmarkAttempt, prior []BenchmarkAttempt) (bool, error) {
	if a.RouteStatus == "IDEMPOTENT_REPLAY" {
		if len(prior) == 0 {
			return false, errors.New("IDEMPOTENT_REPLAY has no prior attempt")
		}
		first := prior[0]
		if a.ReportedCost == nil || *a.ReportedCost != 0 {
			return false, errors.New("idempotency requires an explicit zero replay cost")
		}
		if first.Verification.IndependentVerified && !digest.MatchString(first.ProviderReply.ArtifactSHA256) {
			return false, errors.New("verified replay result requires an original artifact digest")
		}
		if a.ProviderReply.ArtifactSHA256 != first.ProviderReply.ArtifactSHA256 ||
			a.ProviderReply.SummarySHA256 != first.ProviderReply.SummarySHA256 ||
			a.ProviderReply.ReportedSuccess != first.ProviderReply.ReportedSuccess ||
			!reflect.DeepEqual(a.Verification, first.Verification) {
			return false, errors.New("replay result differs from the original worker result")
		}
		// The original worker has already passed through this verifier.
		return first.Verification.IndependentVerified, nil
	}
	if a.ProcessExitCode == nil || *a.ProcessExitCode != 0 || a.ProviderReply.Status != "OK" || !a.ProviderReply.ReportedSuccess {
		return false, nil
	}
	switch run.Role {
	case "coding", "debugging":
		cr := a.Verification.CheckResult
		if cr == nil || cr.Test != run.AcceptanceRubric.GoCheck || cr.Status != "PASSED" || cr.ExitStatus != 0 {
			return false, nil
		}
		return true, nil
	case "code_review":
		rr := a.Verification.ReviewResult
		if rr == nil || rr.Verdict != run.AcceptanceRubric.ExpectedVerdict ||
			a.ProviderReply.ArtifactSHA256 != rr.ArtifactSHA256 || rr.ArtifactSHA256 != run.AcceptanceRubric.ArtifactSHA256 || rr.CheckStatus != "PASSED" {
			return false, nil
		}
		if run.AcceptanceRubric.ArtifactExpectation == "defective" && len(rr.Findings) == 0 {
			return false, nil
		}
		if run.AcceptanceRubric.ArtifactExpectation == "clean" && len(rr.Findings) != 0 {
			return false, nil
		}
		return true, nil
	case "planning":
		if err := validatePlanDAG(a.Verification.PlanNodes, run.AcceptanceRubric.RequiredPlanNodes); err != nil {
			return false, nil
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported role %q", run.Role)
	}
}

func validatePlanDAG(nodes []BenchmarkPlanNode, required []string) error {
	if len(nodes) == 0 {
		return errors.New("empty plan_nodes")
	}
	depsByKey := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		if !model.ValidID(n.Key) {
			return fmt.Errorf("invalid plan node key %q", n.Key)
		}
		if _, exists := depsByKey[n.Key]; exists {
			return fmt.Errorf("duplicate plan node key %q", n.Key)
		}
		depsByKey[n.Key] = n.DependsOn
	}
	for _, req := range required {
		if _, ok := depsByKey[req]; !ok {
			return fmt.Errorf("missing required plan node %q", req)
		}
	}
	for key, deps := range depsByKey {
		for _, dep := range deps {
			if dep == key {
				return fmt.Errorf("plan node %q depends on itself", key)
			}
			if _, ok := depsByKey[dep]; !ok {
				return fmt.Errorf("plan node %q depends on unknown node %q", key, dep)
			}
		}
	}
	state := make(map[string]int, len(nodes))
	var visit func(string) error
	visit = func(k string) error {
		switch state[k] {
		case 1:
			return fmt.Errorf("cycle detected in plan DAG at %q", k)
		case 2:
			return nil
		}
		state[k] = 1
		for _, dep := range depsByKey[k] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[k] = 2
		return nil
	}
	for _, n := range nodes {
		if err := visit(n.Key); err != nil {
			return err
		}
	}
	return nil
}

type costGroupKey struct {
	provider    string
	costUnit    string
	account     string
	quotaWindow string
	policy      string
}

type costGroupAccumulator struct {
	runIDs            map[string]bool
	verifiedRunIDs    map[string]bool
	latencies         []int64
	attemptCount      int
	failedAttempts    int
	knownCostCount    int
	unknownCostCount  int
	totalKnownCost    float64
	failedKnownCost   float64
	missingDataEvents int
	crossGroupRuns    int
}

func aggregateProfileSummary(profile string, allRuns []BenchmarkCaseRun) ProfileBenchmarkSummary {
	var runs []BenchmarkCaseRun
	for _, r := range allRuns {
		if r.Profile == profile {
			runs = append(runs, r)
		}
	}
	summary := ProfileBenchmarkSummary{
		Profile:        profile,
		RawSampleCount: len(runs),
	}
	if len(runs) == 0 {
		return summary
	}

	var latencies []int64
	groups := make(map[costGroupKey]*costGroupAccumulator)
	var groupOrder []costGroupKey

	for _, run := range runs {
		latencies = append(latencies, run.LatencyMS)
		if run.VerifiedSuccess {
			summary.VerifiedSuccessCount++
		}
		if run.Role == "code_review" {
			summary.QualityEvaluatedCases++
			if run.VerifiedSuccess {
				summary.QualityApprovedCases++
			}
		}
		quotaUnknown := run.QuotaProvenance.State == BenchmarkCostUnknown || run.QuotaProvenance.RemainingPercent == nil
		if quotaUnknown {
			summary.UnknownQuotaCount++
		}
		runHasUnknownCost := run.ReportedCost == nil
		runHasMissingCostEvidence := false
		groupsForRun := make(map[costGroupKey]bool)
		for i, a := range run.Attempts {
			if a.ReportedCost == nil {
				runHasUnknownCost = true
			}
			isFailed := !a.Verification.IndependentVerified && a.RouteStatus != "IDEMPOTENT_REPLAY"
			if isFailed {
				summary.FailedAttemptCount++
			}

			unit := a.CostUnit
			account := a.Account
			window := a.QuotaWindow
			policy := ""
			if a.CostEvidence != nil {
				policy = a.CostEvidence.Policy
			} else if run.CostEvidence != nil {
				policy = run.CostEvidence.Policy
			}

			gk := costGroupKey{
				provider:    a.Provider,
				costUnit:    unit,
				account:     account,
				quotaWindow: window,
				policy:      policy,
			}
			acc, ok := groups[gk]
			if !ok {
				acc = &costGroupAccumulator{

					runIDs:         make(map[string]bool),
					verifiedRunIDs: make(map[string]bool),
				}
				groups[gk] = acc
				groupOrder = append(groupOrder, gk)
			}
			if !acc.runIDs[run.CaseID] {
				acc.runIDs[run.CaseID] = true
				acc.latencies = append(acc.latencies, run.LatencyMS)
			}
			groupsForRun[gk] = true
			acc.attemptCount++
			if isFailed {
				acc.failedAttempts++
			}
			if run.VerifiedSuccess && i == terminalAttemptIndex(run) {
				acc.verifiedRunIDs[run.CaseID] = true
			}
			scopeKnown := knownIdentity(policy) && knownIdentity(unit) && knownIdentity(account) && knownIdentity(window)
			if strings.HasPrefix(unit, "quota_") {
				q := run.QuotaProvenance
				scopeKnown = scopeKnown && q.Fresh && q.AccountBound && q.RemainingPercent != nil && q.Account == account && q.Window == window && a.Provider == q.Provider
			}
			if !scopeKnown || a.ReportedCost == nil {
				acc.missingDataEvents++
				runHasMissingCostEvidence = true
			}
			if a.ReportedCost == nil {
				acc.unknownCostCount++
			} else {
				acc.knownCostCount++
				acc.totalKnownCost += *a.ReportedCost
				if isFailed {
					acc.failedKnownCost += *a.ReportedCost
				}
			}
		}
		if len(groupsForRun) > 1 {
			for gk := range groupsForRun {
				groups[gk].crossGroupRuns++
			}
		}

		if runHasUnknownCost {
			summary.UnknownCostCount++
		}
		if runHasUnknownCost || runHasMissingCostEvidence || quotaUnknown {
			summary.MissingDataCount++
		}
	}

	summary.VerifiedSuccessRate = float64(summary.VerifiedSuccessCount) / float64(summary.RawSampleCount)
	summary.SuccessRate = summary.VerifiedSuccessRate
	if summary.QualityEvaluatedCases > 0 {
		rate := float64(summary.QualityApprovedCases) / float64(summary.QualityEvaluatedCases)
		summary.QualityApprovalRate = &rate
	}
	summary.P50MS = nearestRankPercentile(latencies, 0.50)
	summary.P95MS = nearestRankPercentile(latencies, 0.95)
	summary.MissingDataRatio = float64(summary.MissingDataCount) / float64(summary.RawSampleCount)

	slices.SortFunc(groupOrder, func(a, b costGroupKey) int {
		return cmp.Or(
			cmp.Compare(a.provider, b.provider),
			cmp.Compare(a.costUnit, b.costUnit),
			cmp.Compare(a.account, b.account),
			cmp.Compare(a.quotaWindow, b.quotaWindow),
			cmp.Compare(a.policy, b.policy),
		)
	})

	for _, gk := range groupOrder {
		acc := groups[gk]
		cg := CostGroupSummary{
			Provider:                gk.provider,
			Profile:                 profile,
			CostUnit:                gk.costUnit,
			Account:                 gk.account,
			QuotaWindow:             gk.quotaWindow,
			Policy:                  gk.policy,
			RawSampleCount:          len(acc.runIDs),
			AttemptCount:            acc.attemptCount,
			VerifiedSuccessCount:    len(acc.verifiedRunIDs),
			VerifiedSuccesses:       len(acc.verifiedRunIDs),
			FailedAttemptCount:      acc.failedAttempts,
			KnownCostAttemptCount:   acc.knownCostCount,
			UnknownCostAttemptCount: acc.unknownCostCount,
			UnknownCostAttempts:     acc.unknownCostCount,
			CrossGroupRuns:          acc.crossGroupRuns,
			P50MS:                   nearestRankPercentile(acc.latencies, 0.50),
			P95MS:                   nearestRankPercentile(acc.latencies, 0.95),
			MissingDataCount:        acc.missingDataEvents,
		}
		if acc.attemptCount > 0 {
			cg.MissingDataRatio = float64(acc.missingDataEvents) / float64(acc.attemptCount)
		}
		if acc.knownCostCount > 0 {
			total := acc.totalKnownCost
			failed := acc.failedKnownCost
			cg.KnownTotalCost = &total
			cg.KnownFailedAttemptCost = &failed
			cg.TotalMeasurableCost = &total
			cg.FailedAttemptMeasurableCost = &failed
		}
		cg.Complete = acc.missingDataEvents == 0 && acc.knownCostCount > 0
		if cg.Complete {
			total := acc.totalKnownCost
			cg.CompleteTotalCost = &total
			cg.CostStatus = "KNOWN"
			if cg.VerifiedSuccessCount > 0 && acc.crossGroupRuns == 0 {
				perSuccess := total / float64(cg.VerifiedSuccessCount)
				cg.CostPerVerifiedSuccess = &perSuccess
			}
		} else {
			cg.CostStatus = BenchmarkCostUnknown
		}
		summary.CostGroups = append(summary.CostGroups, cg)
	}

	return summary
}

// Attempt verification has already validated the role-specific worker result.
func successfulHalfOpenProbe(a BenchmarkAttempt) bool {
	e := a.ProbeEvidence
	return a.RouteStatus == "HALF_OPEN_PROBE" && a.RecoveryState == "HALF_OPEN" &&
		e != nil && !e.CooldownUntil.IsZero() && !e.ProbeStartedAt.IsZero() && !e.ProbeStartedAt.Before(e.CooldownUntil) &&
		e.StateAfter == "CLOSED" && a.Verification.IndependentVerified
}

func aggregateQuotaPressureScenarios(runs []BenchmarkCaseRun) map[string]QuotaPressureSummary {
	out := make(map[string]QuotaPressureSummary)
	for _, r := range runs {
		scenario := r.Scenario
		if scenario == "" {
			scenario = "NORMAL"
		}
		s := out[scenario]
		s.Scenario = scenario
		s.RunCount++
		if r.VerifiedSuccess {
			s.VerifiedSuccessCount++
		}
		var saw429, sawHalfOpen bool
		for _, a := range r.Attempts {
			if (a.RouteStatus == "EXHAUSTED_0PCT" || a.RouteStatus == "NO_ELIGIBLE_ROUTE") &&
				a.ProcessExitCode == nil && a.ProviderReply.Status == "NOT_SPAWNED" {
				s.BlockedSpawnCount++
			}
			if a.ProviderReply.Status == "HTTP_429" || a.ProviderReply.HTTPStatus == 429 {
				saw429 = true
			}
			if successfulHalfOpenProbe(a) {
				sawHalfOpen = true
			}
			if a.RouteStatus == "IDEMPOTENT_REPLAY" && a.ProcessExitCode == nil && a.ProviderReply.Status == "NOT_SPAWNED" {
				s.IdempotentReplayCount++
			}
		}
		if saw429 && r.VerifiedSuccess {
			s.FallbackSuccessCount++
		}
		if sawHalfOpen && r.VerifiedSuccess {
			s.HalfOpenProbeSuccessCount++
		}
		out[scenario] = s
	}
	return out
}

func protocolMetadataRejections(p BenchmarkProtocol) []string {
	var reasons []string
	if !p.PreRegistered {
		reasons = append(reasons, "PRE_REGISTRATION_MISSING")
	}
	if strings.TrimSpace(p.Hypothesis) == "" {
		reasons = append(reasons, "HYPOTHESIS_MISSING")
	}
	if strings.TrimSpace(p.PrimaryMetric) == "" {
		reasons = append(reasons, "PRIMARY_METRIC_MISSING")
	}
	if p.DriftThresholds.FPMaxAccuracyDropPP <= 0 ||
		p.DriftThresholds.FPMinMeasurableCostGainPct <= 0 ||
		p.DriftThresholds.FPMaxP95LatencyIncreasePct <= 0 ||
		p.DriftThresholds.PriceMaxExtraSpend < 0 {
		reasons = append(reasons, "DRIFT_THRESHOLDS_MISSING")
	}
	if p.BudgetLimit.MaxTotalCalls <= 0 || p.BudgetLimit.MaxPaidSpend < 0 {
		reasons = append(reasons, "BUDGET_LIMIT_MISSING")
	}
	if p.RecordedAt.IsZero() {
		reasons = append(reasons, "RECORDED_AT_MISSING")
	}
	if strings.TrimSpace(p.Environment.CPU) == "" ||
		strings.TrimSpace(p.Environment.Platform) == "" ||
		strings.TrimSpace(p.Environment.Worktree) == "" ||
		(p.Environment.ProviderMode != "offline_simulated" && p.Environment.ProviderMode != "live") {
		reasons = append(reasons, "ENVIRONMENT_METADATA_MISSING")
	}
	return reasons
}

// With nearest-rank p95, fewer than twenty observations make p95 the maximum.
// This descriptive floor does not establish statistical power.
func evaluateSamplePlan(plan BenchmarkSamplePlan, matchedCaseCount int, roleCounts map[string]int) []string {
	var reasons []string
	minPerRole := max(1, plan.MinCasesPerRole)
	minimum := max(20, len(plan.RequiredRoles)*minPerRole)
	switch {
	case plan.MinMatchedCasesPerProfile <= 0:
		reasons = append(reasons, "SAMPLE_PLAN_MISSING")
	case plan.MinMatchedCasesPerProfile < minimum:
		reasons = append(reasons, "SAMPLE_PLAN_INSUFFICIENT_MINIMUM")
	}
	if matchedCaseCount < plan.MinMatchedCasesPerProfile {
		reasons = append(reasons, "PILOT_SAMPLE_SIZE_BELOW_PLAN")
	}
	// Required role coverage and per-role minimums.
	for _, role := range plan.RequiredRoles {
		if roleCounts[role] == 0 {
			reasons = append(reasons, "REQUIRED_ROLE_MISSING")
		} else if roleCounts[role] < minPerRole {
			reasons = append(reasons, "INSUFFICIENT_CASES_PER_ROLE")
		}
	}
	return reasons
}

func nearestRankPercentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	idx := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
