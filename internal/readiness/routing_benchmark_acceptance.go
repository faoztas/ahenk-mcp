package readiness

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

// VerifyRoutingBenchmarkAcceptance checks the registered thresholds against the
// supplied matched observations. The returned report retains B1's evidence
// scope and claim restrictions, including for a passing simulated suite.
func VerifyRoutingBenchmarkAcceptance(suite BenchmarkSuite) (BenchmarkReport, error) {
	report, err := EvaluateRoutingBenchmark(suite)
	if err != nil {
		return report, err
	}

	for _, reason := range report.SuperiorityRejectionReasons {
		switch reason {
		case "OFFLINE_SIMULATION", "MEASURED_COMPARISON_REQUIRED":
			// Simulation can exercise the acceptance rules.
		default:
			return report, fmt.Errorf("benchmark acceptance: %s", reason)
		}
	}

	d := report.Protocol.DriftThresholds
	if !validAcceptanceLimit(d.FPMaxAccuracyDropPP, 100) ||
		!validAcceptanceLimit(d.FPMinMeasurableCostGainPct, 100) ||
		!validAcceptanceLimit(d.FPMaxP95LatencyIncreasePct, math.MaxFloat64) ||
		!validAcceptanceLimit(d.QualityMinSuccessRate, 1) || d.QualityMinSuccessRate == 0 ||
		!validAcceptanceLimit(d.PriceMaxExtraSpend, math.MaxFloat64) {
		return report, errors.New("invalid acceptance thresholds")
	}
	if err := verifyBenchmarkPairing(suite); err != nil {
		return report, err
	}
	ref := report.Profiles[BenchmarkProfileReference]
	fp := report.Profiles[BenchmarkProfilePricePerformance]
	quality := report.Profiles[BenchmarkProfileQuality]
	price := report.Profiles[BenchmarkProfilePrice]
	return report, errors.Join(
		verifyBenchmarkBudget(suite),
		verifyBenchmarkPricePerformance(suite, ref, fp, d),
		verifyBenchmarkQuality(ref, quality, d.QualityMinSuccessRate),
		verifyBenchmarkPrice(suite, price, d),
	)
}

func validAcceptanceLimit(value, maximum float64) bool {
	return value >= 0 && value <= maximum
}

// B1 checks the pairing within each case. Distinct cases must also
// identify distinct task variants and worker jobs.
func verifyBenchmarkPairing(suite BenchmarkSuite) error {
	type task struct {
		role, variant string
	}
	variants := make(map[task]string)
	jobs := make(map[string]int)
	for i, r := range suite.Runs {
		if r.Profile == BenchmarkProfileReference {
			key := task{r.Role, strings.TrimSpace(r.TaskVariant)}
			if prior, exists := variants[key]; exists {
				return fmt.Errorf("benchmark acceptance: duplicate matched task variant in cases %s and %s", prior, r.CaseID)
			}
			variants[key] = r.CaseID
		}
		for _, a := range r.Attempts {
			job := strings.TrimSpace(a.JobID)
			if job == "" {
				continue
			}
			if prior, exists := jobs[job]; exists && prior != i {
				return fmt.Errorf("benchmark acceptance: job reused across benchmark runs (%s/%s and %s/%s)",
					suite.Runs[prior].CaseID, suite.Runs[prior].Profile, r.CaseID, r.Profile)
			}
			jobs[job] = i
		}
	}
	return nil
}

func benchmarkAttemptDidNotSpawn(r BenchmarkCaseRun, a BenchmarkAttempt) bool {
	if a.ProcessExitCode != nil || a.ProviderReply.Status != "NOT_SPAWNED" || a.ProviderReply.HTTPStatus != 0 ||
		a.ReportedCost != nil && *a.ReportedCost != 0 || a.ProbeEvidence != nil || a.RateLimitScope != "" || a.RecoveryState != "" ||
		a.RouteStatus == "FALLBACK_429" || a.RouteStatus == "HALF_OPEN_PROBE" {
		return false
	}
	// B1 checks the echoed result. A replay can also repeat the original
	// observation or receipt, but cannot introduce a different worker.
	if a.RouteStatus == "IDEMPOTENT_REPLAY" {
		first := r.Attempts[0]
		return (a.ObservedRoute == nil || reflect.DeepEqual(a.ObservedRoute, first.ObservedRoute)) &&
			(a.CostEvidence == nil || a.CostEvidence.VerifiedRoute == nil || reflect.DeepEqual(a.CostEvidence, first.CostEvidence))
	}
	v := a.Verification
	return !a.ProviderReply.ReportedSuccess && a.ProviderReply.SummarySHA256 == "" && a.ProviderReply.ArtifactSHA256 == "" &&
		!v.IndependentVerified && v.CheckResult == nil && v.ReviewResult == nil && len(v.PlanNodes) == 0 &&
		a.ObservedRoute == nil && (a.CostEvidence == nil || a.CostEvidence.VerifiedRoute == nil)
}

func verifyBenchmarkBudget(suite BenchmarkSuite) error {
	limit := suite.Protocol.BudgetLimit
	if limit.MaxTotalCalls <= 0 || !validAcceptanceLimit(limit.MaxPaidSpend, math.MaxFloat64) {
		return errors.New("budget: invalid registered limits")
	}
	var calls int
	var spend float64
	var unknown, contradictory error
	for _, r := range suite.Runs {
		var runSpend float64
		for _, a := range r.Attempts {
			notSpawned := benchmarkAttemptDidNotSpawn(r, a)
			if !notSpawned {
				calls++
				if contradictory == nil && (a.ProviderReply.Status == "NOT_SPAWNED" || a.RouteStatus == "NO_ELIGIBLE_ROUTE" || a.RouteStatus == "EXHAUSTED_0PCT" || a.RouteStatus == "IDEMPOTENT_REPLAY") {
					contradictory = fmt.Errorf("budget: contradictory NOT_SPAWNED evidence for %s/%s attempt %d", r.CaseID, r.Profile, a.Number)
				}
			}
			switch {
			case a.CostUnit == "usd" && a.ReportedCost != nil:
				runSpend += *a.ReportedCost
			case notSpawned:
				// A blocked candidate or replay incurred no provider call.
			case a.ReportedCost != nil && a.CostUnit != "" &&
				benchmarkQuotaUnit(canonicalBenchmarkBucket(a.Provider, a.QuotaWindow)) == a.CostUnit && verifiedBenchmarkCostProtection(r, a):
				// Quota usage has no USD equivalent. Verified protection
				// establishes that this attempt incurred no extra charge.
			default:
				if unknown == nil {
					unknown = fmt.Errorf("budget: unknown paid spend for %s/%s attempt %d", r.CaseID, r.Profile, a.Number)
				}
			}
		}
		// A run total may expose extra spend beyond the known attempt subtotal.
		if r.CostUnit == "usd" && r.ReportedCost != nil {
			runSpend = max(runSpend, *r.ReportedCost)
		}
		spend += runSpend
	}
	var callErr, spendErr error
	if calls > limit.MaxTotalCalls {
		callErr = fmt.Errorf("budget: %d calls exceed registered limit of %d", calls, limit.MaxTotalCalls)
	}
	if spend > limit.MaxPaidSpend {
		spendErr = fmt.Errorf("budget: paid spend %.9g USD exceeds registered limit of %.9g USD", spend, limit.MaxPaidSpend)
	}
	return errors.Join(contradictory, callErr, spendErr, unknown)
}

func verifyBenchmarkPricePerformance(suite BenchmarkSuite, ref, fp ProfileBenchmarkSummary, d BenchmarkDriftThresholds) error {
	dropPP := 100 * float64(ref.VerifiedSuccessCount-fp.VerifiedSuccessCount) / float64(ref.RawSampleCount)
	if dropPP > d.FPMaxAccuracyDropPP {
		return fmt.Errorf("price_performance: verified success dropped %.2f percentage points", dropPP)
	}
	p95Increase := 100 * float64(fp.P95MS-ref.P95MS) / float64(ref.P95MS)
	if p95Increase > d.FPMaxP95LatencyIncreasePct {
		return fmt.Errorf("price_performance: p95 increased %.2f%%", p95Increase)
	}

	// B1 can describe several scopes. B2.3 uses a single complete bucket
	// covering the entire matched sample so costs and successes share a denominator.
	if len(ref.CostGroups) != 1 || len(fp.CostGroups) != 1 {
		return errors.New("price_performance: comparable cost requires one complete scope per profile")
	}
	a, b := ref.CostGroups[0], fp.CostGroups[0]
	if a.CostPerVerifiedSuccess == nil || b.CostPerVerifiedSuccess == nil ||
		a.RawSampleCount != ref.RawSampleCount || b.RawSampleCount != fp.RawSampleCount ||
		a.CostUnit != b.CostUnit || a.Provider != b.Provider || a.Account != b.Account ||
		a.QuotaWindow != b.QuotaWindow || a.Policy != b.Policy ||
		(strings.HasPrefix(a.CostUnit, "quota_") && !matchingBenchmarkQuotaScopes(suite, a.CostUnit)) {
		return errors.New("price_performance: comparable cost requires matching units, scope, and verified success")
	}

	baseline, measured := *a.CostPerVerifiedSuccess, *b.CostPerVerifiedSuccess
	if baseline <= 0 || math.IsInf(baseline, 0) || math.IsInf(measured, 0) {
		return errors.New("price_performance: cost gain requires finite costs and a positive reference cost")
	}
	gain := 100 * (baseline - measured) / baseline
	if gain+1e-9 < d.FPMinMeasurableCostGainPct {
		return fmt.Errorf("price_performance: cost gain %.2f%% below %.2f%%", gain, d.FPMinMeasurableCostGainPct)
	}
	return nil
}

func benchmarkQuotaUnit(bucket string) string {
	switch bucket {
	case providers.BarCodexWeekly, "gemini-weekly", providers.BarAgyArgon:
		return "quota_percent_weekly"
	case providers.BarCodexBurst, providers.BarAgyGemini, providers.BarAgyOpus5h:
		return "quota_percent_burst"
	default:
		return ""
	}
}

// Map the provider's observed bucket and declared window to the same
// measurement. Different AGY families can have windows of the same duration.
func canonicalBenchmarkBucket(provider, name string) string {
	switch provider {
	case "codex":
		switch name {
		case "weekly", providers.BarCodexWeekly:
			return providers.BarCodexWeekly
		case "burst", "five_hour", providers.BarCodexBurst:
			return providers.BarCodexBurst
		}
	case "agy":
		switch name {
		case "gemini-5h", providers.BarAgyGemini:
			return providers.BarAgyGemini
		case "3p-5h", providers.BarAgyOpus5h:
			return providers.BarAgyOpus5h
		case "gemini-weekly", providers.BarAgyArgon:
			return name
		}
	}
	return ""
}

func matchingBenchmarkQuotaScopes(suite BenchmarkSuite, unit string) bool {
	var pooled BenchmarkQuotaProvenance
	for _, r := range suite.Runs {
		if r.Profile != BenchmarkProfileReference && r.Profile != BenchmarkProfilePricePerformance {
			continue
		}
		q := r.QuotaProvenance
		// B1 matches the attempt provider, account and window to quota
		// provenance before computing a complete cost per verified success.
		bucket := canonicalBenchmarkBucket(q.Provider, q.Window)
		if bucket == "" || benchmarkQuotaUnit(bucket) != unit || !knownIdentity(q.Source) ||
			q.RouteQuota == nil || !knownIdentity(q.RouteQuota.Bucket) ||
			q.RouteQuota.ResetAt == nil || q.RouteQuota.ResetAt.IsZero() {
			return false
		}
		if bucket != canonicalBenchmarkBucket(q.Provider, q.RouteQuota.Bucket) {
			return false
		}
		if pooled.RouteQuota == nil {
			pooled = q
			continue
		}
		if q.Source != pooled.Source || q.RouteQuota.Bucket != pooled.RouteQuota.Bucket ||
			!q.RouteQuota.ResetAt.Equal(*pooled.RouteQuota.ResetAt) {
			return false
		}
	}
	return pooled.RouteQuota != nil
}

func verifyBenchmarkQuality(ref, quality ProfileBenchmarkSummary, floor float64) error {
	if quality.VerifiedSuccessRate < floor {
		return fmt.Errorf("quality: verified success %.2f below floor %.2f", quality.VerifiedSuccessRate, floor)
	}
	if ref.QualityApprovalRate == nil || quality.QualityApprovalRate == nil ||
		*quality.QualityApprovalRate < max(floor, *ref.QualityApprovalRate) {
		return errors.New("quality: review approval below the registered floor or matched reference")
	}
	return nil
}

func verifyBenchmarkPrice(suite BenchmarkSuite, price ProfileBenchmarkSummary, d BenchmarkDriftThresholds) error {
	if price.VerifiedSuccessRate < d.QualityMinSuccessRate {
		return fmt.Errorf("price: verified success %.2f below floor %.2f", price.VerifiedSuccessRate, d.QualityMinSuccessRate)
	}
	var spend float64
	var policyErr error
	for _, r := range suite.Runs {
		if r.Profile != BenchmarkProfilePrice {
			continue
		}
		for _, a := range r.Attempts {
			if a.CostUnit != "usd" || a.ReportedCost == nil {
				return errors.New("price: spend evidence requires an explicit USD amount for every attempt")
			}
			spend += *a.ReportedCost
			if policyErr == nil && (*a.ReportedCost != 0 ||
				(!benchmarkAttemptDidNotSpawn(r, a) && !verifiedBenchmarkCostProtection(r, a))) {
				policyErr = fmt.Errorf("price: verified free or included usage required for %s attempt %d", r.CaseID, a.Number)
			}
		}
	}
	var spendErr error
	if spend > d.PriceMaxExtraSpend {
		spendErr = fmt.Errorf("price: extra spend %.6g USD exceeds %.6g USD", spend, d.PriceMaxExtraSpend)
	}
	return errors.Join(policyErr, spendErr)
}

func verifiedBenchmarkCostProtection(r BenchmarkCaseRun, a BenchmarkAttempt) bool {
	e, route := a.CostEvidence, a.ObservedRoute
	if route == nil || e == nil || !knownIdentity(a.Account) || e.Account != a.Account ||
		route.Account != a.Account || route.Provider != a.Provider || route.Model != a.Model ||
		!knownIdentity(route.Fingerprint) || (r.CostEvidence != nil && !reflect.DeepEqual(r.CostEvidence, e)) {
		return false
	}
	scope := route.Scope
	if scope.NativeChild || scope.FollowUp {
		return false
	}
	task := model.Task{Mode: scope.Mode, ExecutionProfile: scope.ExecutionProfile, DataClass: scope.DataClass}
	worker := model.Capability{Provider: route.Provider, Model: route.Model, Mode: route.Adapter, Fingerprint: route.Fingerprint,
		AttestedAccount: route.Account, OpenCodeTransport: route.Transport, OpenCodeEndpoint: route.Endpoint, Cost: e}
	if route.Provider != "opencode" {
		return route.Transport == "" && route.Endpoint == "" && providers.VerifiedIncludedRoute(worker, task)
	}
	worker.OpenCodeAccount = route.Account
	return route.Transport != "" && providers.FreeRouteIdentity(worker) && providers.MatchesCostReceipt(worker, task) &&
		e.Policy == "FREE_ONLY" && e.Status == "ZERO_PRICE_CATALOG" && e.Documented == "VERIFIED" && e.RuntimeVerified == "VERIFIED" &&
		scope.Mode == "read_only" && scope.DataClass == "synthetic" && slices.Contains([]string{"", "analysis", "restricted"}, scope.ExecutionProfile)
}
