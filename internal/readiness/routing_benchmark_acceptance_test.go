package readiness

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestRoutingBenchmarkAcceptanceRequiresSampleAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*BenchmarkSuite)
		want   string
	}{
		{name: "registered offline control"},
		{name: "ingestion and observer time stay separate", mutate: func(s *BenchmarkSuite) {
			overhead := int64(60_000)
			s.Protocol.Environment.StdoutJSONIngestionMS = &overhead
			s.Protocol.Environment.ObserverIOMS = &overhead
		}},
		{"below registered sample", func(s *BenchmarkSuite) {
			s.Protocol.SamplePlan.MinMatchedCasesPerProfile = 21
		}, "PILOT_SAMPLE_SIZE_BELOW_PLAN"},
		{"underspecified sample plan", func(s *BenchmarkSuite) {
			s.Protocol.SamplePlan.MinMatchedCasesPerProfile = 5
		}, "SAMPLE_PLAN_INSUFFICIENT_MINIMUM"},
		{"unregistered policy", func(s *BenchmarkSuite) {
			s.Protocol.PreRegistered = false
		}, "PRE_REGISTRATION_MISSING"},
		{"invalid threshold", func(s *BenchmarkSuite) {
			s.Protocol.DriftThresholds.QualityMinSuccessRate = math.NaN()
		}, "invalid acceptance thresholds"},
		{"repeated task variant under another case ID", func(s *BenchmarkSuite) {
			for i := 20; i < 24; i++ {
				s.Runs[i].TaskVariant = s.Runs[0].TaskVariant
			}
		}, "duplicate matched task variant"},
		{"same job used for two matched runs", func(s *BenchmarkSuite) {
			s.Runs[20].Attempts[0].JobID = s.Runs[0].Attempts[0].JobID
		}, "job reused across benchmark runs"},
		{"unmatched variant", func(s *BenchmarkSuite) {
			s.Runs[1].TaskVariant += "-unmatched"
		}, "mismatched task_variant"},
		{"review below quality baseline despite overall floor", func(s *BenchmarkSuite) {
			s.Protocol.DriftThresholds.QualityMinSuccessRate = 0.8
			for i := range s.Runs {
				r := &s.Runs[i]
				if r.Profile == BenchmarkProfileQuality && r.Role == "code_review" {
					r.Attempts[0].Verification = BenchmarkVerification{}
					r.VerifiedSuccess = false
					break
				}
			}
		}, "quality: review approval"},
		{"price below quality floor", func(s *BenchmarkSuite) {
			for i := range s.Runs {
				r := &s.Runs[i]
				if r.Profile == BenchmarkProfilePrice {
					r.Attempts[0].Verification = BenchmarkVerification{}
					r.VerifiedSuccess = false
				}
			}
		}, "price: verified success"},
		{"price extra spend includes failed attempt", func(s *BenchmarkSuite) {
			addAcceptanceFailedAttempt(&s.Runs[3], 0.01)
		}, "price: extra spend"},
		{"budget rejects one call limit", func(s *BenchmarkSuite) {
			s.Protocol.BudgetLimit.MaxTotalCalls = 1
		}, "budget: 80 calls exceed"},
		{"budget counts failed call without exit code and its spend", func(s *BenchmarkSuite) {
			addAcceptanceFailedAttempt(&s.Runs[4], 0.125)
			s.Runs[4].Attempts[0].ProcessExitCode = nil
			s.Protocol.BudgetLimit.MaxTotalCalls = 80
			s.Protocol.BudgetLimit.MaxPaidSpend = 2.5
		}, "budget: 81 calls exceed"},
		{"budget includes failed spend from any profile", func(s *BenchmarkSuite) {
			addAcceptanceFailedAttempt(&s.Runs[2], 0.125)
			s.Protocol.BudgetLimit.MaxPaidSpend = 1.75
		}, "budget: paid spend"},
		{"budget accepts exact call and spend limits with a replay", func(s *BenchmarkSuite) {
			r := &s.Runs[2]
			replay := r.Attempts[0]
			replay.Number = 2
			replay.RouteStatus = "IDEMPOTENT_REPLAY"
			replay.ProviderReply.Status = "NOT_SPAWNED"
			replay.ProviderReply.HTTPStatus = 0
			replay.ProcessExitCode = nil
			replay.ReportedCost = new(float64)
			r.Scenario = "RETRY_IDEMPOTENCY"
			r.Attempts = append(r.Attempts, replay)
			s.Protocol.BudgetLimit.MaxTotalCalls = 80
			s.Protocol.BudgetLimit.MaxPaidSpend = 1.71875
		}, ""},
		{"budget rejects a different observed worker on replay", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			replay := r.Attempts[0]
			replay.Number = 2
			replay.RouteStatus = "IDEMPOTENT_REPLAY"
			replay.ProviderReply.Status = "NOT_SPAWNED"
			replay.ProviderReply.HTTPStatus = 0
			replay.ProcessExitCode = nil
			observed := *replay.ObservedRoute
			observed.Fingerprint = strings.Repeat("a", 64)
			replay.ObservedRoute = &observed
			r.Scenario = "RETRY_IDEMPOTENCY"
			r.Attempts = append(r.Attempts, replay)
		}, "contradictory NOT_SPAWNED"},
		{"budget includes a recorded USD total above the known attempt subtotal", func(s *BenchmarkSuite) {
			r := &s.Runs[2]
			addAcceptanceFailedAttempt(r, 0.125)
			r.Attempts[0].ProcessExitCode = nil
			r.Attempts[0].ProviderReply = BenchmarkProviderReply{Status: "NOT_SPAWNED"}
			r.Attempts[0].ReportedCost = nil
			s.Protocol.BudgetLimit.MaxTotalCalls = 80
			s.Protocol.BudgetLimit.MaxPaidSpend = 1.75
		}, "budget: paid spend"},
		{"budget rejects unknown charged spend even with known run total", func(s *BenchmarkSuite) {
			s.Runs[2].Attempts[0].ReportedCost = nil
		}, "budget: unknown paid spend"},
		{"budget rejects quota usage without verification of no extra charge", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			s.Runs[0].Attempts[0].CostEvidence.RuntimeVerified = "UNKNOWN"
		}, "budget: unknown paid spend"},
		{"budget rejects non-finite registered spend cap", func(s *BenchmarkSuite) {
			s.Protocol.BudgetLimit.MaxPaidSpend = math.Inf(1)
		}, "budget: invalid registered limits"},
		{"price accepts verified subscription inclusion", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			setAcceptanceSubscriptionRoute(r, "codex")
		}, ""},
		{"price rejects zero without cost evidence", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			r.CostEvidence, r.Attempts[0].CostEvidence = nil, nil
		}, "price: verified free or included"},
		{"price rejects run evidence without attempt evidence", func(s *BenchmarkSuite) {
			s.Runs[3].Attempts[0].CostEvidence = nil
		}, "price: verified free or included"},
		{"price rejects verified metered zero", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			evidence := *r.Attempts[0].CostEvidence
			evidence.Policy, evidence.Status = "METERED_API", "REPORTED_USAGE"
			r.Attempts[0].CostEvidence, r.CostEvidence = &evidence, &evidence
		}, "price: verified free or included"},
		{"price rejects catalog without runtime verification", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			evidence := *r.Attempts[0].CostEvidence
			evidence.RuntimeVerified = "UNKNOWN"
			r.Attempts[0].CostEvidence, r.CostEvidence = &evidence, &evidence
		}, "price: verified free or included"},
		{"price rejects subscription documentation without runtime verification", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			setAcceptanceSubscriptionRoute(r, "claude")
			r.Attempts[0].CostEvidence.RuntimeVerified = "UNKNOWN"
		}, "price: verified free or included"},
		{"price rejects evidence without account binding", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			evidence := *r.Attempts[0].CostEvidence
			evidence.Account = ""
			r.Attempts[0].CostEvidence, r.CostEvidence = &evidence, &evidence
		}, "price: verified free or included"},
		{"price rejects conflicting run cost evidence", func(s *BenchmarkSuite) {
			r := &s.Runs[3]
			evidence := *r.CostEvidence
			evidence.Policy = "METERED_API"
			r.CostEvidence = &evidence
		}, "price: verified free or included"},
		{"price rejects charge contradicting free evidence below spend cap", func(s *BenchmarkSuite) {
			s.Protocol.DriftThresholds.PriceMaxExtraSpend = 0.25
			addAcceptanceFailedAttempt(&s.Runs[3], 0.125)
		}, "price: verified free or included"},
		{"price unknown spend", func(s *BenchmarkSuite) {
			s.Runs[3].ReportedCost = nil
			s.Runs[3].Attempts[0].ReportedCost = nil
		}, "price: spend evidence"},
		{"different currency", func(s *BenchmarkSuite) {
			for i := range s.Runs {
				if r := &s.Runs[i]; r.Profile == BenchmarkProfilePricePerformance {
					r.CostUnit, r.Attempts[0].CostUnit = "eur", "eur"
				}
			}
		}, "price_performance: comparable cost"},
		{"matching quota units and scope", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
		}, ""},
		{"different quota units", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_burst")
		}, "price_performance: comparable cost"},
		{"different quota source", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			s.Runs[1].QuotaProvenance.Source = "different-source"
		}, "price_performance: comparable cost"},
		{"weekly quota labeled for a five-hour window", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			for i := range s.Runs {
				r := &s.Runs[i]
				if r.Profile == BenchmarkProfileReference || r.Profile == BenchmarkProfilePricePerformance {
					r.QuotaProvenance.Window = "five_hour"
					r.Attempts[0].QuotaWindow = "five_hour"
					r.QuotaProvenance.RouteQuota.Bucket = "five_hour"
				}
			}
		}, "price_performance: comparable cost"},
		{"weekly quota rejects a consistently mislabeled five-hour bucket", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			for i := range s.Runs {
				if quota := s.Runs[i].QuotaProvenance.RouteQuota; quota != nil {
					quota.Bucket = "five_hour"
				}
			}
		}, "price_performance: comparable cost"},
		{"quota bucket required even when both paired rows omit it", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			s.Runs[0].QuotaProvenance.RouteQuota.Bucket = ""
			s.Runs[1].QuotaProvenance.RouteQuota.Bucket = ""
		}, "price_performance: comparable cost"},
		{"quota reset required even when both paired rows omit it", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			s.Runs[0].QuotaProvenance.RouteQuota.ResetAt = nil
			s.Runs[1].QuotaProvenance.RouteQuota.ResetAt = nil
		}, "price_performance: comparable cost"},
		{"quota reset cannot shift between matched pairs", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			nextReset := s.Protocol.RecordedAt.Add(48 * time.Hour)
			s.Runs[4].QuotaProvenance.RouteQuota.ResetAt = &nextReset
			s.Runs[5].QuotaProvenance.RouteQuota.ResetAt = &nextReset
		}, "price_performance: comparable cost"},
		{"quota bucket cannot shift between matched pairs", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			s.Runs[4].QuotaProvenance.RouteQuota.Bucket = "other-weekly"
			s.Runs[5].QuotaProvenance.RouteQuota.Bucket = "other-weekly"
		}, "price_performance: comparable cost"},
		{"matching burst quota scopes", func(s *BenchmarkSuite) {
			setAcceptanceQuotaScopes(s, "quota_percent_burst", "five_hour")
		}, ""},
		{"different quota reset windows", func(s *BenchmarkSuite) {
			setAcceptanceQuotaUnits(s, "quota_percent_weekly")
			reset := s.Protocol.RecordedAt.Add(time.Hour)
			nextReset := reset.Add(time.Hour)
			s.Runs[0].QuotaProvenance.RouteQuota = &RouteQuota{Bucket: "weekly", ResetAt: &reset}
			s.Runs[1].QuotaProvenance.RouteQuota = &RouteQuota{Bucket: "weekly", ResetAt: &nextReset}
		}, "price_performance: comparable cost"},
		{"accuracy drop above limit", func(s *BenchmarkSuite) {
			for _, i := range []int{1, 5} {
				s.Runs[i].Attempts[0].Verification = BenchmarkVerification{}
				s.Runs[i].VerifiedSuccess = false
			}
		}, "price_performance: verified success"},
		{"missing cost gain", func(s *BenchmarkSuite) {
			for i := range s.Runs {
				if r := &s.Runs[i]; r.Profile == BenchmarkProfilePricePerformance {
					r.ReportedCost = s.Runs[0].ReportedCost
					r.Attempts[0].ReportedCost = r.ReportedCost
				}
			}
		}, "price_performance: cost gain"},
		{"p95 above registered limit", func(s *BenchmarkSuite) {
			s.Runs[1].LatencyMS = 126
			s.Runs[5].LatencyMS = 126
		}, "price_performance: p95"},
		{"unsupported superiority claim", func(s *BenchmarkSuite) {
			s.Protocol.ClaimSuperiority = true
		}, "unsupported superiority claim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite := acceptanceBenchmarkFixture(t)
			if tc.mutate != nil {
				tc.mutate(&suite)
			}
			report, err := VerifyRoutingBenchmarkAcceptance(suite)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("verification error = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if report.MatchedCaseCount != 20 || report.RepoPolicyVersion != suite.Runs[0].RepoPolicyVersion ||
				report.SampleClass != BenchmarkSamplePilot || report.ProtocolEligible || report.SuperiorityClaimAllowed {
				t.Fatalf("offline scope changed: cases=%d policy=%q sample=%q eligible=%t superiority=%t",
					report.MatchedCaseCount, report.RepoPolicyVersion, report.SampleClass,
					report.ProtocolEligible, report.SuperiorityClaimAllowed)
			}
			ref, fp := report.Profiles[BenchmarkProfileReference], report.Profiles[BenchmarkProfilePricePerformance]
			if ref.VerifiedSuccessCount != 20 || fp.VerifiedSuccessCount != 20 || fp.P95MS != 100 {
				t.Fatalf("reference/price-performance success = %d/%d, task p95 = %d ms", ref.VerifiedSuccessCount, fp.VerifiedSuccessCount, fp.P95MS)
			}
			if cost := fp.CostGroups[0].CostPerVerifiedSuccess; cost == nil || *cost != 0.0234375 {
				t.Fatalf("price-performance cost per verified success = %v, want 0.0234375", cost)
			}
		})
	}
}

func acceptanceBenchmarkFixture(t *testing.T) BenchmarkSuite {
	t.Helper()
	suite, err := LoadRoutingBenchmarkFixture("testdata/routing/matched_profiles_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	rows := suite.Runs
	suite.Runs = nil
	suite.Protocol.SamplePlan.MinMatchedCasesPerProfile = 20
	suite.Protocol.BudgetLimit.MaxTotalCalls = 100

	// Extend the B1 fixture with matched synthetic variants and balanced order.
	// Costs and equal task latencies are inputs to the verifier.
	for batch := range 4 {
		for _, r := range rows {
			r.CaseID = fmt.Sprintf("%s-%d", r.CaseID, batch)
			r.TaskVariant = fmt.Sprintf("%s-%d", r.TaskVariant, batch)
			r.RunOrder = (r.RunOrder-1+batch)%4 + 1
			r.LatencyMS = 100
			a := r.Attempts[0]
			a.LatencyMS = 100
			a.RequestID = fmt.Sprintf("%s-%d", a.RequestID, batch)
			a.JobID = fmt.Sprintf("%s-%d", a.JobID, batch)
			cost := 0.03125
			if r.Profile == BenchmarkProfilePrice {
				a.Provider, a.Model, a.Effort = "opencode", "opencode/glm-5.3-flash-free", ""
				a.Account, a.QuotaWindow = model.OpenCodeAnonymous, "synthetic-window"
				route := model.CostRoute{
					Provider: a.Provider, Model: a.Model, Account: a.Account, Adapter: "tool_free",
					Fingerprint: "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82",
					Transport:   model.OpenCodeStateless, Endpoint: model.OpenCodeFreeEndpoint,
					Scope: model.AttemptScope{Mode: "read_only", DataClass: "synthetic", ExecutionProfile: "analysis"}}
				observed := route
				a.ObservedRoute = &observed
				a.CostEvidence = &model.CostEvidence{VerifiedRoute: &route, Policy: "FREE_ONLY", Status: "ZERO_PRICE_CATALOG",
					Documented: "VERIFIED", RuntimeVerified: "VERIFIED", Account: a.Account}
				r.QuotaProvenance = BenchmarkQuotaProvenance{
					Provider: a.Provider, Account: a.Account, Source: "offline_synthetic", State: BenchmarkCostUnknown,
				}
				cost = 0
			} else {
				ref := rows[0].Attempts[0]
				a.Provider, a.Model, a.Effort = ref.Provider, ref.Model, ref.Effort
				a.Account, a.QuotaWindow, a.CostEvidence = ref.Account, ref.QuotaWindow, ref.CostEvidence
				r.QuotaProvenance = rows[0].QuotaProvenance
				r.QuotaProvenance.Provider = a.Provider
				r.QuotaProvenance.Account = a.Account
				r.QuotaProvenance.Window = a.QuotaWindow
				if r.Profile == BenchmarkProfilePricePerformance {
					cost = 0.0234375
				}
			}
			a.CostUnit, a.ReportedCost = "usd", &cost
			a.ReportedCostStatus = ""
			r.Attempts = []BenchmarkAttempt{a}
			r.CostUnit, r.ReportedCost, r.ReportedCostStatus = "usd", &cost, ""
			r.CostEvidence = a.CostEvidence
			suite.Runs = append(suite.Runs, r)
		}
	}
	return suite
}

func addAcceptanceFailedAttempt(r *BenchmarkCaseRun, charge float64) {
	failed := r.Attempts[0]
	failed.ProviderReply.ReportedSuccess = false
	failed.Verification = BenchmarkVerification{}
	failed.ReportedCost = &charge
	r.Attempts[0].Number = 2
	r.Attempts = append([]BenchmarkAttempt{failed}, r.Attempts...)
	total := charge + *r.Attempts[1].ReportedCost
	r.ReportedCost = &total
	r.LatencyMS += failed.LatencyMS
}

func setAcceptanceSubscriptionRoute(r *BenchmarkCaseRun, provider string) {
	a := &r.Attempts[0]
	a.Provider, a.Model, a.Account = provider, "synthetic-cli-model", "synthetic-cli-account"
	r.QuotaProvenance.Provider, r.QuotaProvenance.Account = a.Provider, a.Account
	setAcceptanceIncludedEvidence(r)
}

func setAcceptanceIncludedEvidence(r *BenchmarkCaseRun) {
	a := &r.Attempts[0]
	route := model.CostRoute{Provider: a.Provider, Model: a.Model, Account: a.Account, Adapter: "cli", Fingerprint: strings.Repeat("b", 64),
		Scope: model.AttemptScope{Mode: "read_only", DataClass: "synthetic", ExecutionProfile: "analysis"}}
	observed := route
	a.ObservedRoute = &observed
	evidence := &model.CostEvidence{Policy: "SUBSCRIPTION_ONLY", Status: "ZERO_COST_VERIFIED", VerifiedRoute: &route,
		Documented: "VERIFIED", RuntimeVerified: "VERIFIED", Account: r.Attempts[0].Account}
	r.Attempts[0].CostEvidence, r.CostEvidence = evidence, evidence
}

func setAcceptanceQuotaUnits(s *BenchmarkSuite, comparisonUnit string) {
	setAcceptanceQuotaScopes(s, "quota_percent_weekly", "weekly")
	for i := range s.Runs {
		if r := &s.Runs[i]; r.Profile == BenchmarkProfilePricePerformance {
			r.CostUnit, r.Attempts[0].CostUnit = comparisonUnit, comparisonUnit
		}
	}
}

func setAcceptanceQuotaScopes(s *BenchmarkSuite, unit, window string) {
	reset := s.Protocol.RecordedAt.Add(24 * time.Hour)
	for i := range s.Runs {
		r := &s.Runs[i]
		if r.Profile != BenchmarkProfileReference && r.Profile != BenchmarkProfilePricePerformance {
			continue
		}
		r.CostUnit, r.Attempts[0].CostUnit = unit, unit
		r.Attempts[0].QuotaWindow = window
		r.QuotaProvenance = s.Runs[0].QuotaProvenance
		r.QuotaProvenance.Provider = r.Attempts[0].Provider
		r.QuotaProvenance.Account = r.Attempts[0].Account
		r.QuotaProvenance.Window = window
		r.QuotaProvenance.RouteQuota = &RouteQuota{Bucket: map[string]string{"weekly": "codex_weekly", "five_hour": "codex_5h_burst"}[window], ResetAt: &reset}
		setAcceptanceIncludedEvidence(r)
	}
}

func TestRoutingBenchmarkAcceptanceProductionQuotaBuckets(t *testing.T) {
	for _, bucket := range []string{"codex_5h_burst", "unknown-weekly", "gemini-weekly"} {
		t.Run(bucket, func(t *testing.T) {
			s := acceptanceBenchmarkFixture(t)
			setAcceptanceQuotaUnits(&s, "quota_percent_weekly")
			for _, r := range s.Runs {
				if q := r.QuotaProvenance.RouteQuota; q != nil {
					q.Bucket = bucket
				}
			}
			if _, err := VerifyRoutingBenchmarkAcceptance(s); err == nil || !strings.Contains(err.Error(), "price_performance: comparable cost") {
				t.Fatalf("weekly comparison accepted bucket %s: %v", bucket, err)
			}
		})
	}
}

func TestRoutingBenchmarkAcceptanceNoSpawnOutcome(t *testing.T) {
	for name, mutate := range map[string]func(*BenchmarkAttempt){
		"reported success": func(a *BenchmarkAttempt) { a.ProviderReply.ReportedSuccess = true },
		"summary":          func(a *BenchmarkAttempt) { a.ProviderReply.SummarySHA256 = strings.Repeat("a", 64) },
		"artifact":         func(a *BenchmarkAttempt) { a.ProviderReply.ArtifactSHA256 = strings.Repeat("b", 64) },
		"http":             func(a *BenchmarkAttempt) { a.ProviderReply.HTTPStatus = 429 },
		"review":           func(a *BenchmarkAttempt) { a.Verification.ReviewResult = &model.ReviewResult{} },
		"check":            func(a *BenchmarkAttempt) { a.Verification.CheckResult = &model.CheckEvidence{} },
		"plan":             func(a *BenchmarkAttempt) { a.Verification.PlanNodes = []BenchmarkPlanNode{{Key: "result"}} },
		"fallback 429":     func(a *BenchmarkAttempt) { a.RouteStatus = "FALLBACK_429" },
		"probe":            func(a *BenchmarkAttempt) { a.ProbeEvidence = &BenchmarkProbeEvidence{} },
		"observed worker":  func(a *BenchmarkAttempt) { a.ObservedRoute = &model.CostRoute{} },
		"charged":          func(a *BenchmarkAttempt) { x := 0.01; a.ReportedCost = &x },
	} {
		t.Run(name, func(t *testing.T) {
			s := acceptanceBenchmarkFixture(t)
			r := &s.Runs[2]
			addAcceptanceFailedAttempt(r, 0)
			a := &r.Attempts[0]
			a.ProcessExitCode = nil
			a.ProviderReply = BenchmarkProviderReply{Status: "NOT_SPAWNED"}
			mutate(a)
			total := *r.Attempts[1].ReportedCost + *a.ReportedCost
			r.ReportedCost = &total
			if _, err := VerifyRoutingBenchmarkAcceptance(s); err == nil || !strings.Contains(err.Error(), "NOT_SPAWNED") {
				t.Errorf("contradictory no-spawn accepted: %v", err)
			}
			s.Protocol.BudgetLimit.MaxTotalCalls = 80
			if err := verifyBenchmarkBudget(s); err == nil || !strings.Contains(err.Error(), "81 calls exceed") {
				t.Fatalf("worker outcome escaped call budget: %v", err)
			}
		})
	}
}

func TestRoutingBenchmarkAcceptancePriceReceipts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*BenchmarkCaseRun)
		accept bool
	}{
		{"codex", func(r *BenchmarkCaseRun) { setAcceptanceSubscriptionRoute(r, "codex") }, true},
		{"claude", func(r *BenchmarkCaseRun) { setAcceptanceSubscriptionRoute(r, "claude") }, true},
		{"anonymous subscription", func(r *BenchmarkCaseRun) {
			e := r.Attempts[0].CostEvidence
			e.Policy, e.Status = "SUBSCRIPTION_ONLY", "DOCUMENTED_COST_PROTECTION"
		}, false},
		{"unsupported subscription provider", func(r *BenchmarkCaseRun) {
			e := r.Attempts[0].CostEvidence
			e.Policy, e.Status = "SUBSCRIPTION_ONLY", "ZERO_COST_VERIFIED"
		}, false},
		{"missing independent observation", func(r *BenchmarkCaseRun) { r.Attempts[0].ObservedRoute = nil }, false},
		{"missing receipt route", func(r *BenchmarkCaseRun) { r.Attempts[0].CostEvidence.VerifiedRoute = nil }, false},
		{"wrong adapter", func(r *BenchmarkCaseRun) { r.Attempts[0].ObservedRoute.Adapter = "cli" }, false},
		{"different worker", func(r *BenchmarkCaseRun) { r.Attempts[0].ObservedRoute.Fingerprint = strings.Repeat("c", 64) }, false},
		{"different model", func(r *BenchmarkCaseRun) { r.Attempts[0].Model += "x" }, false},
		{"different execution scope", func(r *BenchmarkCaseRun) {
			r.Attempts[0].CostEvidence.VerifiedRoute.Scope.ExecutionProfile = "restricted"
		}, false},
		{"paid endpoint", func(r *BenchmarkCaseRun) {
			a := r.Attempts[0]
			a.ObservedRoute.Endpoint = "paid"
			a.CostEvidence.VerifiedRoute.Endpoint = "paid"
		}, false},
		{"ineligible free scope", func(r *BenchmarkCaseRun) {
			a := r.Attempts[0]
			a.ObservedRoute.Scope.NativeChild = true
			a.CostEvidence.VerifiedRoute.Scope.NativeChild = true
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acceptanceBenchmarkFixture(t)
			tc.change(&s.Runs[3])
			_, err := VerifyRoutingBenchmarkAcceptance(s)
			if tc.accept && err != nil || !tc.accept && (err == nil || !strings.Contains(err.Error(), "price: verified free or included usage required for "+s.Runs[3].CaseID)) {
				t.Fatalf("receipt acceptance = %v, want accepted %v", err, tc.accept)
			}
		})
	}
}

func TestRoutingBenchmarkAcceptanceSerializedReceipts(t *testing.T) {
	s := acceptanceBenchmarkFixture(t)
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var decoded BenchmarkSuite
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRoutingBenchmarkAcceptance(decoded); err != nil {
		t.Fatal(err)
	}
}
