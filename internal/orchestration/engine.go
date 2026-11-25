// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

type Engine struct {
	privateWriter    privateWriteExecutor
	cfg              model.Config
	db               *store.Store
	reg              providers.Registry
	ctx              context.Context
	stop             context.CancelFunc
	wg               sync.WaitGroup
	mu               sync.Mutex
	jobCond          *sync.Cond
	jobs             map[string]model.Job
	active           map[string]context.CancelFunc
	orphaned         map[string]string
	cooldown         map[string]time.Time
	transient        map[string][]time.Time
	openCodeHealth   map[openCodeHealthKey]openCodeHealth
	openCodeSuccess  map[openCodeIdentity]time.Time
	openCodeSequence uint64
	openCodeHistory  OpenCodeHealthHistory
	quotas           map[string][]providers.Quota
	quotaRefreshedAt time.Time
	quotaRefreshing  atomic.Bool
	readQuotas       func()
	agyHistory       []providers.Quota
	cb               *CircuitBreaker
	closed           bool
	fatal            error
	snapshots        string
	artifacts        string
	scratch          string
}

func New(c model.Config, db *store.Store, reg providers.Registry) (*Engine, error) {
	return newEngine(c, db, reg, nil)
}

func newEngine(c model.Config, db *store.Store, reg providers.Registry, writer privateWriteExecutor) (*Engine, error) {
	c.Repositories = maps.Clone(c.Repositories)
	ctx, stop := context.WithCancel(context.Background())
	e := &Engine{privateWriter: writer, cfg: c, db: db, reg: reg, ctx: ctx, stop: stop, jobs: map[string]model.Job{}, active: map[string]context.CancelFunc{},
		orphaned: map[string]string{}, cooldown: map[string]time.Time{}, transient: map[string][]time.Time{}, quotas: map[string][]providers.Quota{},
		snapshots: filepath.Join(db.Dir, "snapshots"), artifacts: filepath.Join(db.Dir, "artifacts"), scratch: filepath.Join(db.Dir, "scratch"),
		cb: NewCircuitBreaker()}
	e.readQuotas = e.readQuotasOnce
	e.jobCond = sync.NewCond(&e.mu)
	for _, d := range []string{e.snapshots, e.artifacts, e.scratch} {
		if err := os.Mkdir(d, 0700); err != nil && !os.IsExist(err) {
			stop()
			return nil, err
		}
		st, err := os.Lstat(d)
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
			stop()
			return nil, fmt.Errorf("unsafe private state directory")
		}
	}
	list, err := db.All()
	if err != nil {
		stop()
		return nil, err
	}
	var freeAttempts []model.Attempt
	for _, j := range list {
		if j.RoutingProfileSource == "" {
			j.RoutingProfileSource = "legacy"
		}
		e.jobs[j.ID] = j
		for _, attempt := range j.Attempts {
			if attempt.Provider == "opencode" && attempt.FinishedAt != nil {
				freeAttempts = append(freeAttempts, attempt)
			} else {
				e.rememberRateLimit(&attempt)
			}
		}
		if len(j.Attempts) > 0 {
			last := j.Attempts[len(j.Attempts)-1]
			dir := e.attemptDir(j.ID, last.Number)
			if last.FinishedAt == nil && (!execution.LeaseReleased(dir) || execution.RecoverWorktree(dir) != nil) {
				e.orphaned[j.ID] = dir
			}
		}
		if !model.Terminal(j.Status) && j.Status != model.Paused && j.Status != model.Interrupted && j.Status != model.Cancelling {
			j.Status = model.Interrupted
			j.Reason = "OWNER_RESTARTED"
			e.persist(j)
		}
	}
	slices.SortStableFunc(freeAttempts, func(a, b model.Attempt) int {
		return a.FinishedAt.Compare(*b.FinishedAt)
	})
	latest, err := db.PrunedOpenCodeCircuit()
	if err != nil {
		stop()
		return nil, err
	}
	for i := range freeAttempts {
		a := &freeAttempts[i]
		e.openCodeHistory.Apply(a)
		e.recordOpenCodeObservation(a)
		// Only an accepted decision can change the shared breaker. Legacy
		// observations continue to enforce their independent scoped gates.
		if d := a.OpenCodeCircuit; d != nil && d.Sequence > latest.Sequence {
			latest = *d
		}
	}
	e.openCodeSequence = latest.Sequence
	if latest.Until != nil {
		e.cb.cooldown["opencode"] = *latest.Until
	}
	imports, err := store.ReadOpenCodeCooldownImports(db.Dir)
	if err != nil {
		stop()
		return nil, err
	}
	for _, record := range imports {
		if !providers.OpenCodeIdentityMatches(record.Model, record.Fingerprint) {
			stop()
			return nil, errors.New("unverified OpenCode cooldown provenance")
		}
		retryAt := OpenCodeImportRetryAt(record)
		finished := record.FinishedAt
		a := model.Attempt{Provider: "opencode", Model: record.Model, Fingerprint: record.Fingerprint, Code: "RATE_LIMIT",
			OpenCodeAccount: record.OpenCodeAccount, OpenCodeTransport: record.OpenCodeTransport, OpenCodeEndpoint: record.Endpoint,
			OpenCodeRecheck: record.Recheck, FinishedAt: &finished, RetryAt: &retryAt, RetryAfterMS: record.RetryAfterMS,
			RetrySource: record.RetrySource, UpstreamStatus: record.UpstreamStatus, ProviderRateLimitScope: record.ProviderRateLimitScope}
		e.openCodeHistory.Apply(&a)
		e.recordOpenCodeObservation(&a)
	}
	if e.fatal != nil {
		return nil, e.fatal
	}
	if c.EnableCodexBar {
		e.loadQuotaHistory()
		e.wg.Go(e.refreshQuotas)
	}
	e.wg.Go(e.loop)
	return e, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	e.closed = true
	e.stop()
	e.mu.Unlock()
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range e.jobs {
		if !model.Terminal(j.Status) && j.Status != model.Interrupted && j.Status != model.Paused && j.Status != model.Cancelling {
			j.Status = model.Paused
			j.Reason = "OWNER_DISCONNECTED"
			e.persist(j)
		}
	}
	return e.fatal
}

func (e *Engine) Store() *store.Store {
	return e.db
}

func (e *Engine) CircuitBreaker() *CircuitBreaker {
	return e.cb
}

// IsExecutable returns true only if the provider has at least one executable/available candidate.
func (e *Engine) IsExecutable(provider string) bool {
	if e == nil {
		return false
	}
	norm := normalizeProvider(provider)
	for _, c := range e.reg.Candidates {
		if normalizeProvider(c.Provider) == norm {
			if c.Status == "available" {
				return true
			}
		}
	}
	return false
}

func (e *Engine) Repo(id string) (model.Repo, bool) {
	if e == nil {
		return model.Repo{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.cfg.Repositories[id]
	if ok {
		r.AllowedPaths = slices.Clone(r.AllowedPaths)
		r.Providers = slices.Clone(r.Providers)
	}
	return r, ok
}

func (e *Engine) HasRepo(id string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.cfg.Repositories[id]
	return ok
}

func (e *Engine) RepoProviders(id string) []string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.cfg.Repositories[id]
	if !ok {
		return nil
	}
	return slices.Clone(r.Providers)
}

func (e *Engine) LocalRunners() []model.LocalRunnerCapability {
	var xcodeCfg *model.XcodeSchemeRunnerConfig
	if e != nil {
		e.mu.Lock()
		if e.cfg.LocalXcodeRunner != nil {
			cp := *e.cfg.LocalXcodeRunner
			cp.Schemes = slices.Clone(cp.Schemes)
			cp.AcceptanceModes = slices.Clone(cp.AcceptanceModes)
			xcodeCfg = &cp
		}
		e.mu.Unlock()
	}
	swiftStatus := "unavailable"
	swiftReason := "DARWIN_SANDBOX_OR_SWIFT_UNAVAILABLE"
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err == nil {
			if _, err := os.Stat("/usr/bin/swift"); err == nil {
				swiftStatus = "available"
				swiftReason = "DIGEST_PINNED_SWIFTPM_HEAD_ONLY"
			}
		}
	}
	simStatus := "unavailable"
	simReason := "XCRUN_OR_SANDBOX_UNAVAILABLE"
	if swiftStatus == "available" {
		if _, err := os.Stat("/usr/bin/xcrun"); err == nil {
			simStatus = "available"
			simReason = "INNER_SIMULATOR_SANDBOX_VERIFIED_AT_DISPATCH"
		}
	}
	xcodeRunner := model.LocalRunnerCapability{
		Runner:          "xcode-scheme",
		Status:          "unavailable",
		Reason:          "XCODE_SCHEME_RUNNER_UNVERIFIED",
		NextAction:      "route_xcode_scheme_and_ui_acceptance_to_authorized_local_owner",
		NetworkBoundary: "UNVERIFIED",
	}
	if xcodeCfg != nil && xcodeCfg.Verified && len(xcodeCfg.Schemes) > 0 {
		xcodeRunner = model.LocalRunnerCapability{
			Runner:          "xcode-scheme",
			Status:          "available",
			Reason:          "VERIFIED_LOCAL_XCODE_SCHEME_RUNNER",
			NetworkBoundary: "VERIFIED_LOCAL_RUNNER",
			Schemes:         xcodeCfg.Schemes,
			AcceptanceModes: xcodeCfg.AcceptanceModes,
		}
	}
	return []model.LocalRunnerCapability{
		{
			Runner:          "swiftpm",
			Status:          swiftStatus,
			Reason:          swiftReason,
			NetworkBoundary: "HOST_IP_DENIED",
			AcceptanceModes: []string{"unit"},
		},
		{
			Runner:          "simulator-xctest",
			Status:          simStatus,
			Reason:          simReason,
			NetworkBoundary: "SIMULATOR_INNER_IP_DENIED",
			AcceptanceModes: []string{"standalone_xctest"},
		},
		xcodeRunner,
	}
}

func (e *Engine) SyncTerminalAudit(j model.Job) {
	if e != nil && e.db != nil {
		_ = e.db.SyncTerminalRoutingAudit(j)
	}
}

func (e *Engine) persist(j model.Job) {
	if e.fatal != nil {
		return
	}
	if e.db != nil {
		if err := e.db.Save(j); err != nil {
			e.fatal = err
			e.stop()
			return
		}
	}
	if e.jobs != nil {
		e.jobs[j.ID] = j
	}
	if e.jobCond != nil {
		e.jobCond.Broadcast()
	}
}

func (e *Engine) unavailable() error {
	if e.closed || e.fatal != nil {
		return model.Error("STATE_UNAVAILABLE", "scheduler is shutting down or storage is unavailable")
	}
	return nil
}

// ResolveDefaultRoutingProfile reads the operator preference for the given scope.
// An unreadable record returns the configured or legacy default alongside the error.
func ResolveDefaultRoutingProfile(stateDir, repoID string, repo model.Repo) (model.RoutingProfile, string, error) {
	var err error
	if stateDir != "" {
		var saved string
		saved, err = store.ReadRoutingPreference(stateDir, repoID, repo.Root)
		if err == nil && saved != "" {
			return model.RoutingProfile(saved), "operator", nil
		}
	}
	if repo.DefaultRoutingProfile != "" {
		return repo.DefaultRoutingProfile, "repo_config", err
	}
	return "", "legacy", err
}

// defaultRoutingProfile resolves one snapshot for the job or entire batch outside the scheduler lock.
func (e *Engine) defaultRoutingProfile(repoID string, repo model.Repo, batch model.RoutingProfile) (model.RoutingProfile, string, string) {
	if batch != "" {
		return batch, "batch", ""
	}
	cfg := e.routingPreferenceConfig()
	stateDir, unsafeState := routingPreferenceState(cfg)

	profile, source, err := ResolveDefaultRoutingProfile(stateDir, repoID, repo)
	if err != nil || unsafeState != nil {
		return profile, source, "INVALID_ROUTING_PREFERENCE"
	}
	return profile, source, ""
}

func (e *Engine) routingPreferenceConfig() model.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg := e.cfg
	cfg.Repositories = maps.Clone(e.cfg.Repositories)
	return cfg
}

func routingPreferenceState(cfg model.Config) (string, error) {
	if cfg.StateDir == "" {
		return "", nil
	}
	return store.RoutingPreferenceStateDir(cfg)
}

func (e *Engine) Submit(s model.Submission) (model.Job, error) {
	return e.SubmitRouted(s, nil)
}

// SubmitRouted admits client intent and its optional first decision atomically.
func (e *Engine) SubmitRouted(s model.Submission, initial *model.InitialRoute) (model.Job, error) {
	j, _, err := e.submitRouted(s, initial, false)
	return j, err
}

type routingProfileSelection struct {
	profile model.RoutingProfile
	source  string
	warning string
}

func (e *Engine) submissionRoutingProfile(s model.Submission, repo model.Repo) routingProfileSelection {
	if s.RoutingProfile != "" {
		return routingProfileSelection{profile: s.RoutingProfile, source: "request"}
	}
	profile, source, warning := e.defaultRoutingProfile(s.RepoID, repo, "")
	return routingProfileSelection{profile: profile, source: source, warning: warning}
}

// SubmitAutoRouted selects and persists the first decision using one preference read.
func (e *Engine) SubmitAutoRouted(s model.Submission, category string, complexity *float64) (model.Job, *model.InitialRoute, error) {
	if category != "" {
		s.Category = category
	}
	if complexity != nil {
		s.HasComplexity, s.Complexity = true, *complexity
	}
	return e.submitRouted(s, nil, true)
}

func (e *Engine) previewInitialRoute(j model.Job) *model.InitialRoute {
	var complexity *float64
	if j.Task.HasComplexity || j.Task.Complexity != 0 {
		complexity = &j.Task.Complexity
	}
	decision, candidate := e.evaluateAutoRoute(j.RepoID, j.Task.Category, complexity, j.Task, j.CreatedAt, j.RoutingProfile, j.QualityRole)
	if candidate == nil {
		return nil
	}
	return &model.InitialRoute{
		Provider: NormalizeProvider(candidate.Provider), Model: candidate.Model,
		SelectedModel: decision.SelectedModel, ResolvedEffort: decision.ResolvedEffort,
		CapacityState: decision.CapacityState, CostClass: decision.CostClass, ProfileScore: decision.ProfileScore,
		Reason: decision.Reason, TaskCategory: decision.TaskCategory, Stage: string(decision.Stage),
		QuotaPressure: decision.QuotaPressure, AutoRouted: true, LatencyMS: decision.Latency.Milliseconds(),
	}
}

func (e *Engine) submitRouted(s model.Submission, initial *model.InitialRoute, auto bool) (model.Job, *model.InitialRoute, error) {
	if err := e.resolveSubmissionRepository(&s); err != nil {
		return model.Job{}, initial, err
	}
	e.mu.Lock()
	repo, ok := e.cfg.Repositories[s.RepoID]
	if ok && s.RequestID == "" {
		_ = s.Normalize("", &e.cfg)
	}
	capPolicy := e.cfg.CapacityPolicy
	e.mu.Unlock()
	if !ok {
		return model.Job{}, initial, model.GateError("REPO_NOT_REGISTERED", "REPO_NOT_REGISTERED", "repository is not registered", "register_repository_in_config")
	}
	role, mode := routingRole(s.Task), routingMode(s.Strategy, capPolicy)
	if s.Decompose {
		role = "planning"
	}
	if fault := s.Task.Validate(repo); fault != nil {
		return model.Job{}, initial, e.submissionError(s, fault)
	}
	now := time.Now().UTC()
	j := model.Job{ID: newID(now), RepoID: s.RepoID, RequestID: s.RequestID, Task: s.Task, QualityRole: role, RoutingMode: mode, Status: model.Created,
		CreatedAt: now, DeadlineAt: now.Add(s.Task.Deadline()), PolicyDigest: repo.PolicyDigest()}
	profile := e.submissionRoutingProfile(s, repo)
	j.RoutingProfile, j.RoutingProfileSource, j.RoutingPreferenceWarning = profile.profile, profile.source, profile.warning
	if auto {
		initial = e.previewInitialRoute(j)
	}
	if !model.ValidID(s.RequestID) {
		return model.Job{}, initial, model.Error("INVALID_INPUT", "invalid request ID")
	}
	if s.ExecutionProfile == "review" {
		return model.Job{}, initial, model.Error("INVALID_INPUT", "artifact review requires a batch with an independent verified diff handoff; use analysis for standalone reading")
	}
	var audit *store.JobRoutingAudit
	if initial != nil {
		route := *initial
		j.InitialRoute = &route
		audit = &store.JobRoutingAudit{
			JobID: j.ID, AttemptNumber: 1, DispatchStatus: "DISPATCHED", CreatedAt: now,
			AssignedModel: route.SelectedModel, AssignedEffort: route.ResolvedEffort,
			RouteReason: route.Reason, TaskCategory: route.TaskCategory,
			QuotaPressureAtDispatch: route.QuotaPressure, LatencyMs: route.LatencyMS,
		}
	}
	var recovered *model.InitialRoute
	legacyMatch := func(digest string, job model.Job, audit *store.JobRoutingAudit) bool {
		matched, route := matchLegacySubmission(s, digest, job, audit)
		recovered = route
		return matched
	}
	accepted, err := e.insertRouted([]model.Job{j}, model.SubmissionIntentDigest(s), nil, audit, legacyMatch)
	if err == nil && recovered != nil {
		accepted.InitialRoute = recovered
	}
	if err != nil {
		return accepted, initial, err
	}
	return accepted, accepted.InitialRoute, nil
}

func (e *Engine) resolveSubmissionRepository(s *model.Submission) error {
	if s.RepoID != "" {
		return nil
	}
	repoID, err := e.ResolveRepo("")
	if err != nil {
		return model.Error("INVALID_INPUT", fmt.Sprintf("failed to resolve repository: %v", err))
	}
	s.RepoID = repoID
	return nil
}

// A revoked policy can also make a previously valid task fail validation.
func (e *Engine) submissionError(s model.Submission, cause error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if j, ok := e.requestJob(s.RepoID, s.RequestID); ok && !e.repoAllowed(j) {
		return model.Error("NOT_FOUND", "job not found under current repository policy")
	}
	return cause
}

// requestJob is called with e.mu held. The in-memory job table is bounded to 1000 entries.
func (e *Engine) requestJob(repo, request string) (model.Job, bool) {
	for _, j := range e.jobs {
		if j.RepoID == repo && j.RequestID == request && request != "" {
			return j, true
		}
	}
	return model.Job{}, false
}

// Earlier MCP versions hashed a validated task after applying the automatic selection.
// Only replay that transformation when the original audit and stored task agree.
func matchLegacySubmission(s model.Submission, digest string, job model.Job, a *store.JobRoutingAudit) (bool, *model.InitialRoute) {
	exact := model.Digest(s) == digest
	if a == nil {
		return exact, nil
	}
	provider, name, ok := strings.Cut(a.AssignedModel, "/")
	task := job.Task
	if !ok || name == "" || provider == "" || normalizeProvider(provider) != task.PreferredProvider ||
		legacyModelName(provider, name) != legacyModelName(provider, task.PreferredModel) || a.AssignedEffort == "" || a.AssignedEffort != task.TargetEffort || a.RouteReason == "" {
		return exact, nil
	}
	route := &model.InitialRoute{
		Provider: task.PreferredProvider, Model: task.PreferredModel, SelectedModel: a.AssignedModel, ResolvedEffort: a.AssignedEffort,
		Reason: a.RouteReason, TaskCategory: a.TaskCategory, QuotaPressure: a.QuotaPressureAtDispatch,
		AutoRouted: true, LatencyMS: a.LatencyMs,
	}
	if s.PreferredProvider != "" || s.PreferredModel != "" || (s.TargetEffort != "") != (a.RouteReason == "EXPLICIT_TARGET_EFFORT") {
		return false, nil
	}
	s.PreferredProvider = task.PreferredProvider
	s.PreferredModel = task.PreferredModel
	if s.TargetEffort == "" {
		s.TargetEffort = a.AssignedEffort
	}
	if model.Digest(s) != digest {
		return false, nil
	}
	return true, route
}

func detectGitRoot(dir string) (string, error) {
	gitPath := "/usr/bin/git"
	if _, err := os.Stat(gitPath); err != nil {
		if p, err := exec.LookPath("git"); err == nil {
			gitPath = p
		}
	}
	cmd := exec.Command(gitPath, "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	res := strings.TrimSpace(string(out))
	if res == "" {
		return "", errors.New("empty git root")
	}
	return res, nil
}

func (e *Engine) ResolveRepo(cwd string) (string, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := detectGitRoot(cwd)
	if err != nil {
		root = cwd
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err == nil {
		root = canonicalRoot
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for id, r := range e.cfg.Repositories {
		rRoot, _ := filepath.EvalSymlinks(r.Root)
		if rRoot == root || r.Root == root {
			return id, nil
		}
	}

	base := filepath.Base(root)
	h := sha256.Sum256([]byte(root))
	dynamicID := fmt.Sprintf("local-%s-%x", base, h[:4])

	providersList := []string{"agy", "opencode", "codex", "claude"}
	if e.cfg.EnableFixture {
		providersList = append(providersList, "fixture-a", "fixture-b", "fixture-c")
	}

	if e.cfg.Repositories == nil {
		e.cfg.Repositories = make(map[string]model.Repo)
	}

	dynamicRepo := model.Repo{
		Root:         root,
		AllowedPaths: []string{"."},
		Providers:    providersList,
		Limits: model.ProjectLimits{
			MaxActive:          2,
			MaxDeadlineSeconds: 1800,
		},
	}

	e.cfg.Repositories[dynamicID] = dynamicRepo
	return dynamicID, nil
}

func (e *Engine) Wait(ctx context.Context, jobID string, timeout time.Duration) (model.Job, error) {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	deadline := time.Now().Add(timeout)
	e.mu.Lock()
	defer e.mu.Unlock()

	for {
		j, ok := e.jobs[jobID]
		if !ok {
			return model.Job{}, model.Error("NOT_FOUND", "job not found")
		}
		if model.Terminal(j.Status) || j.Status == model.Paused {
			return j, nil
		}
		if time.Now().After(deadline) {
			return j, nil
		}
		if err := ctx.Err(); err != nil {
			return j, err
		}

		e.mu.Unlock()
		select {
		case <-ctx.Done():
			e.mu.Lock()
			return e.jobs[jobID], ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		e.mu.Lock()
	}
}

func (e *Engine) WaitJobs(ctx context.Context, jobIDs []string, mode string, timeout time.Duration) ([]model.Job, error) {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	if mode == "" {
		mode = "all"
	}
	deadline := time.Now().Add(timeout)
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, id := range jobIDs {
		j, ok := e.jobs[id]
		if !ok || !e.repoAllowed(j) {
			return nil, model.Error("NOT_FOUND", fmt.Sprintf("job not found: %s", id))
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			results := make([]model.Job, 0, len(jobIDs))
			for _, id := range jobIDs {
				results = append(results, e.jobs[id])
			}
			return results, err
		}

		allDone := true
		anyDone := false

		for _, id := range jobIDs {
			j := e.jobs[id]
			done := model.Terminal(j.Status) || j.Status == model.Paused
			if done {
				anyDone = true
			} else {
				allDone = false
			}
		}

		if mode == "any" && anyDone {
			break
		}
		if mode == "all" && allDone {
			break
		}
		if time.Now().After(deadline) {
			break
		}

		e.mu.Unlock()
		select {
		case <-ctx.Done():
			e.mu.Lock()
			results := make([]model.Job, 0, len(jobIDs))
			for _, id := range jobIDs {
				results = append(results, e.jobs[id])
			}
			return results, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		e.mu.Lock()
	}

	results := make([]model.Job, 0, len(jobIDs))
	for _, id := range jobIDs {
		results = append(results, e.jobs[id])
	}
	return results, nil
}

func newID(t time.Time) string {
	return fmt.Sprintf("%020d-%x", t.UnixNano(), randBytes())
}

func randBytes() []byte {
	b := make([]byte, 12)
	rand.Read(b)
	return b
}

func (e *Engine) SubmitBatch(b model.Batch) (model.Job, error) {
	return e.submitBatch(b, nil, nil)
}

func (e *Engine) SubmitM1Batch(b model.Batch, routes []model.OpenCodeRoute, admissionErr error) (model.Job, error) {
	if admissionErr == nil && len(routes) == 0 {
		admissionErr = model.Error("M1_ROUTE_NOT_VERIFIED", "no OpenCode model has verified M1 admission")
	}
	return e.submitBatch(b, routes, admissionErr)
}

func (e *Engine) submitBatch(b model.Batch, routes []model.OpenCodeRoute, admissionErr error) (model.Job, error) {
	e.mu.Lock()
	repo, ok := e.cfg.Repositories[b.RepoID]
	capPolicy := e.cfg.CapacityPolicy
	e.mu.Unlock()
	if !ok {
		return model.Job{}, model.GateError("REPO_NOT_REGISTERED", "REPO_NOT_REGISTERED", "repository is not registered", "register_repository_in_config")
	}
	if repo.PrivateSource != nil {
		if !repo.PrivateSource.WriterIsolationVerified || !repo.PrivateSource.AllowWrite {
			return model.Job{}, model.GateError("POLICY_DENIED", "WRITER_ISOLATION_UNVERIFIED", "private source batch requires verified writer isolation and explicit write admission", "verify_writer_isolation_and_enable_allow_write")
		}
		if b.Independence != "distinct_provider" || (b.WorkspaceMode != "" && b.WorkspaceMode != "head_only") || len(b.UntrackedPaths) > 0 {
			return model.Job{}, model.GateError("POLICY_DENIED", "INDEPENDENT_REVIEW_REQUIRED", "private source write batch requires head_only workspace and distinct_provider review", "use_distinct_provider_batch_with_independent_review")
		}
	}
	if b.RoutingProfile != "" && !b.RoutingProfile.Valid() {
		return model.Job{}, model.Error("INVALID_INPUT", "invalid batch routing_profile")
	}
	if !model.ValidID(b.RequestID) || len(b.Items) < 1 || len(b.Items) > 8 || b.DeadlineSeconds < 0 || b.DeadlineSeconds > 1800 ||
		(b.Independence != "" && b.Independence != "none" && b.Independence != "distinct_provider") {
		return model.Job{}, model.Error("INVALID_INPUT", "invalid batch")
	}
	if l := repo.Limits; l.MaxBatchTasks > 0 && len(b.Items) > l.MaxBatchTasks || l.MaxDeadlineSeconds > 0 && b.DeadlineSeconds > l.MaxDeadlineSeconds {
		return model.Job{}, model.Error("POLICY_DENIED", "batch exceeds project limits")
	}
	if b.RequireSimultaneous && len(b.Items) > 1 {
		return model.Job{}, model.Error("RESOURCE_LIMIT", "simultaneous batch admission is unavailable in this release")
	}
	keys := map[string]int{}
	for i, it := range b.Items {
		if _, exists := keys[it.Key]; exists || !model.ValidID(it.Key) {
			return model.Job{}, model.Error("INVALID_INPUT", "invalid or duplicate item key")
		}
		keys[it.Key] = i
	}
	if err := validateDAG(b.Items, keys); err != nil {
		return model.Job{}, err
	}
	if err := validateReviews(b, keys); err != nil {
		return model.Job{}, err
	}
	now := time.Now().UTC()
	parent := model.Job{ID: newID(now), RepoID: b.RepoID, RequestID: b.RequestID, Status: model.Created, CreatedAt: now,
		PolicyDigest: repo.PolicyDigest(), Distinct: b.Independence == "distinct_provider"}
	parent.RoutingProfile, parent.RoutingProfileSource, parent.RoutingPreferenceWarning = e.defaultRoutingProfile(b.RepoID, repo, b.RoutingProfile)
	parent.Task.WorkspaceMode = b.WorkspaceMode
	if parent.Task.WorkspaceMode == "" {
		parent.Task.WorkspaceMode = "head_only"
	}
	if repo.PrivateSource != nil {
		parent.Task.DataClass = model.PrivateSourceData
	}
	parent.Task.UntrackedPaths = b.UntrackedPaths
	jobs := []model.Job{parent}
	ids := make(map[string]string, len(keys))
	for _, it := range b.Items {
		ids[it.Key] = newID(now)
	}
	// Normalize a private copy so retrying the same batch retains the original item intent.
	b.Items = slices.Clone(b.Items)
	var longest time.Duration
	for i := range b.Items {
		it := &b.Items[i]
		if it.OpenCodeRecheck || it.RoleCalibration || it.Decompose {
			return model.Job{}, model.Error("POLICY_DENIED", "diagnostics and automatic decomposition require a standalone task")
		}
		if it.WorkspaceMode != "" || len(it.UntrackedPaths) != 0 {
			return model.Job{}, model.Error("INVALID_INPUT", "select workspace at batch level")
		}
		role, mode := routingRole(it.Task), routingMode(it.Strategy, capPolicy)
		if fault := it.Task.Validate(repo); fault != nil {
			return model.Job{}, fault
		}
		it.WorkspaceMode = parent.Task.WorkspaceMode
		longest = max(longest, it.Task.Deadline())
		child := model.Job{ID: ids[it.Key], RepoID: b.RepoID, Parent: parent.ID, Key: it.Key, Task: it.Task, QualityRole: role, RoutingMode: mode, Status: model.Created,
			CreatedAt: now, DeadlineAt: now.Add(it.Task.Deadline()), PolicyDigest: parent.PolicyDigest}
		child.RoutingProfile, child.RoutingProfileSource = parent.RoutingProfile, parent.RoutingProfileSource
		if it.RoutingProfile != "" {
			child.RoutingProfile, child.RoutingProfileSource = it.RoutingProfile, "request"
		}
		for _, k := range it.DependsOn {
			child.DependsOn = append(child.DependsOn, ids[k])
		}
		if err := validateHandoffs(*it); err != nil {
			return model.Job{}, err
		}
		child.HandoffFrom = slices.Clone(it.HandoffFrom)
		if child.Key == "receive" {
			child.M1OpenCodeRoutes = slices.Clone(routes)
		}
		parent.Children = append(parent.Children, child.ID)
		for _, p := range it.AllowedPaths {
			if !slices.Contains(parent.Task.AllowedPaths, p) {
				parent.Task.AllowedPaths = append(parent.Task.AllowedPaths, p)
			}
		}
		jobs = append(jobs, child)
	}
	if len(b.UntrackedPaths) > 64 || (parent.Task.WorkspaceMode != "head_only" && parent.Task.WorkspaceMode != "include_working_changes") ||
		(parent.Task.WorkspaceMode == "head_only" && len(b.UntrackedPaths) > 0) {
		return model.Job{}, model.Error("INVALID_INPUT", "invalid batch workspace selection")
	}
	for _, p := range b.UntrackedPaths {
		if !model.SafePath(p) || !model.InScope(p, parent.Task.AllowedPaths) {
			return model.Job{}, model.Error("POLICY_DENIED", "batch untracked path is outside scope")
		}
	}
	if parent.Distinct {
		families := map[string]bool{}
		for _, c := range e.reg.Candidates {
			if c.Status == "available" && repoAllowsProvider(repo, c.Provider) {
				families[providers.Family(c.Provider)] = true
			}
		}
		if len(families) < len(b.Items) && admissionErr == nil {
			admissionErr = model.Error("NO_ELIGIBLE_ROUTE", "insufficient distinct providers")
		}
	}
	if b.DeadlineSeconds != 0 {
		longest = time.Duration(b.DeadlineSeconds) * time.Second
	}
	parent.DeadlineAt = now.Add(longest)
	for i := 1; i < len(jobs); i++ {
		if jobs[i].DeadlineAt.After(parent.DeadlineAt) {
			jobs[i].DeadlineAt = parent.DeadlineAt
		}
	}
	jobs[0] = parent
	legacyMatch := func(digest string, job model.Job, _ *store.JobRoutingAudit) bool {
		// Older batches stored explicit zero and omission identically.
		return len(job.Children) > 0 && digest == model.Digest(b)
	}
	return e.insertRouted(jobs, model.BatchIntentDigest(b), admissionErr, nil, legacyMatch)
}

func validateDAG(items []model.Item, keys map[string]int) error {
	state := make([]int, len(items))
	var visit func(int) error
	visit = func(i int) error {
		if state[i] == 1 {
			return model.Error("INVALID_INPUT", "batch dependency cycle")
		}
		if state[i] == 2 {
			return nil
		}
		state[i] = 1
		for _, name := range items[i].DependsOn {
			j, ok := keys[name]
			if !ok {
				return model.Error("INVALID_INPUT", "unknown dependency")
			}
			if err := visit(j); err != nil {
				return err
			}
		}
		state[i] = 2
		return nil
	}
	for i := range items {
		if err := visit(i); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) insertRouted(jobs []model.Job, digest string, admissionErr error, audit *store.JobRoutingAudit, legacyMatch store.LegacyMatch) (model.Job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.unavailable(); err != nil {
		return model.Job{}, err
	}
	first := jobs[0]
	if existing, ok := e.requestJob(first.RepoID, first.RequestID); ok {
		if !e.repoAllowed(existing) {
			return model.Job{}, model.Error("NOT_FOUND", "job not found under current repository policy")
		}
	} else if len(e.jobs)+len(jobs) > 1000 {
		admissionErr = model.Error("RESOURCE_LIMIT", "local job capacity reached")
	}
	id, inserted, err := e.db.InsertRouted(jobs, digest, admissionErr, audit, legacyMatch)
	if errors.Is(err, store.ErrConflict) {
		return model.Job{}, model.Error("IDEMPOTENCY_CONFLICT", "request ID already has different input")
	}
	if err != nil {
		return model.Job{}, err
	}
	if inserted {
		for _, j := range jobs {
			e.jobs[j.ID] = j
		}
	}
	return e.jobs[id], nil
}

func (e *Engine) Get(id string) (model.Job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.getLocked(id)
}

// GetWithRoutingAudit reads the legacy rows under the same lock as the job so
// the rows and execution evidence reflect the same scheduler transition.
func (e *Engine) GetWithRoutingAudit(id string) (model.Job, []store.JobRoutingAudit, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	job, err := e.getLocked(id)
	if err != nil || e.db == nil {
		return job, nil, err
	}
	history, err := e.db.GetRoutingAudit(id)
	return job, history, err
}

func (e *Engine) getLocked(id string) (model.Job, error) {
	j, ok := e.jobs[id]
	if !ok || !e.repoAllowed(j) {
		return model.Job{}, model.Error("NOT_FOUND", "job not found under current repository policy")
	}
	return j, nil
}

func (e *Engine) repoAllowed(j model.Job) bool {
	r, ok := e.cfg.Repositories[j.RepoID]
	return ok && j.PolicyDigest == r.PolicyDigest()
}

func (e *Engine) List(repo, status, cursor string, limit int) ([]model.Job, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := make([]model.Job, 0)
	for _, j := range e.jobs {
		if e.repoAllowed(j) && (repo == "" || j.RepoID == repo) && (status == "" || j.Status == status) && j.ID > cursor {
			rows = append(rows, j)
		}
	}
	slices.SortFunc(rows, func(a, b model.Job) int { return strings.Compare(a.ID, b.ID) })
	if len(rows) > limit {
		return rows[:limit], rows[limit-1].ID
	}
	return rows, ""
}

func (e *Engine) Cancel(id string) (model.Job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok || !e.repoAllowed(j) {
		return model.Job{}, model.Error("NOT_FOUND", "job not found")
	}
	if err := e.unavailable(); err != nil {
		return j, err
	}
	if model.Terminal(j.Status) {
		return j, nil
	}
	targets := make([]model.Job, 0, len(j.Children)+1)
	for _, id := range append(slices.Clone(j.Children), j.ID) {
		job := e.jobs[id]
		if model.Terminal(job.Status) {
			continue
		}
		job.Status, job.Reason = model.Cancelled, "CLIENT_CANCELLED"
		if e.active[id] != nil {
			job.Status = model.Cancelling
		}
		if id == j.ID && slices.ContainsFunc(targets, func(c model.Job) bool { return c.Status == model.Cancelling }) {
			job.Status = model.Cancelling
		}
		targets = append(targets, job)
	}
	if err := e.db.Save(targets...); err != nil {
		return j, err
	}
	for _, job := range targets {
		e.jobs[job.ID] = job
		if cancel := e.active[job.ID]; cancel != nil {
			cancel()
		}
	}
	if e.jobCond != nil {
		e.jobCond.Broadcast()
	}
	return e.jobs[id], nil
}

func (e *Engine) Resume(id, key string) (model.Job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok || !e.repoAllowed(j) {
		return model.Job{}, model.Error("NOT_FOUND", "job not found under current repository policy")
	}
	if err := e.unavailable(); err != nil {
		return j, err
	}
	if !model.ValidID(key) {
		return j, model.Error("INVALID_INPUT", "invalid request ID")
	}
	if j.Task.DataClass == model.PrivateSourceData {
		return j, model.Error("POLICY_DENIED", "private source jobs require a fresh request after interruption")
	}
	if key == j.ResumeRequest {
		return j, nil
	}
	if j.Status == model.Failed && j.Reason == "INVALID_OUTPUT" && j.ResumeRequest == "" && canRetryReview(j) {
		return e.retryReview(j, key)
	}
	if j.Status != model.Paused && j.Status != model.Interrupted {
		return j, model.Error("INVALID_STATE", "job is not resumable")
	}
	if time.Now().After(j.DeadlineAt) {
		j.Status = model.TimedOut
		j.Reason = "DEADLINE_EXPIRED"
		e.persist(j)
		return j, model.Error("INVALID_STATE", "original deadline expired")
	}
	targets := []model.Job{j}
	for _, id := range j.Children {
		child := e.jobs[id]
		if child.Status == model.Interrupted || child.Status == model.Paused {
			targets = append(targets, child)
		}
	}
	for _, job := range targets {
		if job.SnapshotID != "" {
			if _, err := execution.LoadSnapshot(e.snapshots, job.SnapshotID); err != nil {
				return j, model.Error("INVALID_STATE", "snapshot is unavailable")
			}
		}
		if len(job.Attempts) > 0 {
			last := job.Attempts[len(job.Attempts)-1]
			if (last.FinishedAt == nil || last.Checkpoint != nil) && !execution.LeaseReleased(e.attemptDir(job.ID, last.Number)) {
				return j, model.Error("INVALID_STATE", "worker cleanup has not been confirmed")
			}
		}
	}
	for i := range targets {
		job := &targets[i]
		job.Reason = ""
		job.Status = model.Created
		if job.SnapshotID != "" {
			job.Status = model.Queued
		}
		if i == 0 {
			job.ResumeRequest = key
		}
	}
	if err := e.db.Save(targets...); err != nil {
		return j, err
	}
	for _, job := range targets {
		e.jobs[job.ID] = job
	}
	return targets[0], nil
}

func (e *Engine) retryReview(j model.Job, key string) (model.Job, error) {
	parent := e.jobs[j.Parent]
	if parent.Status != model.Failed || !parent.Distinct || !e.repoAllowed(parent) ||
		!time.Now().Before(j.DeadlineAt) || !time.Now().Before(parent.DeadlineAt) || e.active[j.ID] != nil ||
		!execution.LeaseReleased(e.attemptDir(j.ID, j.Attempts[0].Number)) {
		return j, model.Error("INVALID_STATE", "review cannot be retried under its original batch and deadline")
	}
	if _, err := e.resolveHandoffs(j); err != nil {
		return j, err
	}
	if _, err := execution.LoadSnapshot(e.snapshots, j.SnapshotID); err != nil {
		return j, model.Error("INVALID_STATE", "review snapshot is unavailable")
	}
	j.Status, j.Reason, j.ResumeRequest = model.Queued, "", key
	parent.Status, parent.Reason = model.Queued, ""
	if err := e.db.Save(parent, j); err != nil {
		return model.Job{}, err
	}
	e.jobs[parent.ID], e.jobs[j.ID] = parent, j
	return j, nil
}

func (e *Engine) loop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastGC := time.Time{}
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			e.mu.Lock()
			e.schedule(now)
			if now.Sub(lastGC) > time.Hour {
				e.collect(now)
				lastGC = now
			}
			e.mu.Unlock()
		}
	}
}

func (e *Engine) schedule(now time.Time) {
	if e.unavailable() != nil {
		return
	}
	for id, dir := range e.orphaned {
		if execution.LeaseReleased(dir) && execution.RecoverWorktree(dir) == nil {
			delete(e.orphaned, id)
		}
	}
	var eligible []model.Job
	for _, j := range e.jobs {
		if model.Terminal(j.Status) {
			continue
		}
		if cancel := e.active[j.ID]; cancel != nil {
			if !now.Before(j.DeadlineAt) {
				cancel()
			}
			continue
		}
		if j.Status == model.Cancelling {
			if len(j.Children) > 0 {
				e.aggregate(j)
			} else if e.orphaned[j.ID] == "" {
				j.Status, j.Reason = model.Cancelled, "CLIENT_CANCELLED"
				e.persist(j)
			}
			continue
		}
		if !now.Before(j.DeadlineAt) {
			j.Status, j.Reason = model.TimedOut, "DEADLINE_EXPIRED"
			e.persist(j)
			continue
		}
		if j.Status == model.Paused || j.Status == model.Interrupted {
			continue
		}
		if len(j.Children) > 0 && j.SnapshotID != "" {
			e.aggregate(j)
			continue
		}
		if j.Status != model.Created && j.Status != model.Queued && j.Status != model.RetryWait {
			continue
		}
		if now.Before(j.ReadyAt) {
			continue
		}
		if !e.repoAllowed(j) {
			j.Status, j.Reason = model.Blocked, "REPO_POLICY_CHANGED"
			e.persist(j)
			continue
		}
		if j.Parent != "" {
			p := e.jobs[j.Parent]
			if p.Status != model.Queued {
				continue
			}
			j.SnapshotID, j.BaseSHA, j.Dirty = p.SnapshotID, p.BaseSHA, p.Dirty
		}
		wait, blocked := e.dependencies(j)
		if blocked {
			j.Status, j.Reason = model.Blocked, "DEPENDENCY_FAILED"
			e.persist(j)
		}
		if wait || blocked {
			continue
		}
		eligible = append(eligible, j)
	}
	slices.SortFunc(eligible, func(a, b model.Job) int {
		score := func(j model.Job) int {
			p := map[string]int{"high": 2, "normal": 1}[j.Task.Priority]
			return p + int(now.Sub(j.CreatedAt)/time.Minute)
		}
		if score(a) != score(b) {
			return score(b) - score(a)
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(e.orphaned) != 0 {
		return
	}
	for _, j := range eligible {
		if len(e.active) >= 2 || e.fatal != nil {
			break
		}
		if j.Task.NativeChild && len(e.active) != 0 || e.nativeChildInFlight() {
			continue
		}
		if e.writerConflict(j) || e.projectSlotsFull(j) {
			continue
		}
		e.persist(j)
		if e.fatal != nil {
			break
		}
		ctx, cancel := context.WithDeadline(e.ctx, j.DeadlineAt)
		e.active[j.ID] = cancel
		e.wg.Go(func() { defer cancel(); e.run(ctx, j) })
	}
}

func (e *Engine) nativeChildInFlight() bool {
	for id := range e.active {
		if e.jobs[id].Task.NativeChild {
			return true
		}
	}
	return false
}

func (e *Engine) dependencies(j model.Job) (waiting, blocked bool) {
	for _, id := range j.DependsOn {
		status := e.jobs[id].Status
		if status != model.Completed {
			if model.Terminal(status) {
				return false, true
			}
			waiting = true
		}
	}
	return waiting, false
}

func (e *Engine) projectSlotsFull(j model.Job) bool {
	repo := e.cfg.Repositories[j.RepoID]
	limit := repo.Limits.MaxActive
	for _, other := range e.cfg.Repositories {
		if other.Root == repo.Root && other.Limits.MaxActive > 0 && (limit == 0 || other.Limits.MaxActive < limit) {
			limit = other.Limits.MaxActive
		}
	}
	if limit == 0 {
		return false
	}
	n := 0
	for id := range e.active {
		other := e.cfg.Repositories[e.jobs[id].RepoID]
		if other.Root == repo.Root {
			n++
		}
	}
	return n >= limit
}

func (e *Engine) writerConflict(j model.Job) bool {
	if j.Task.Mode != "patch" {
		return false
	}
	for id := range e.active {
		other := e.jobs[id]
		if other.Task.Mode != "patch" || other.RepoID != j.RepoID && e.cfg.Repositories[other.RepoID].Root != e.cfg.Repositories[j.RepoID].Root {
			continue
		}
		for _, p := range j.Task.AllowedPaths {
			if model.InScope(p, other.Task.AllowedPaths) {
				return true
			}
			for _, q := range other.Task.AllowedPaths {
				if model.InScope(q, []string{p}) {
					return true
				}
			}
		}
	}
	return false
}

func (e *Engine) aggregate(j model.Job) {
	if j.Status != model.Queued && j.Status != model.Cancelling {
		return
	}
	out := model.Completed
	reviewPending := false
	for _, id := range j.Children {
		child := e.jobs[id]
		state := child.Status
		if !model.Terminal(state) {
			return
		}
		if state != model.Completed {
			out = model.Failed
		}
		reviewPending = reviewPending || child.Result != nil && child.Result.Review != nil && child.Result.Review.Verdict != "APPROVED"
	}
	if j.Status == model.Cancelling {
		j.Status = model.Cancelled
		e.persist(j)
		return
	}
	j.Status = out
	if out != model.Completed {
		j.Reason = "BATCH_CHILD_FAILED"
	} else if reviewPending {
		j.Status, j.Reason = model.Blocked, "INDEPENDENT_REVIEW_NOT_APPROVED"
	} else if j.Task.DataClass == model.PrivateSourceData && !e.privateBatchReviewsBound(j) {
		j.Status, j.Reason = model.Blocked, "HANDOFF_INVALID"
	}
	if j.Task.Decompose && j.Status == model.Completed {
		summary := fmt.Sprintf("Completed %d planned tasks. Use the child job results to retrieve summaries, checks and artifacts.", len(j.Children))
		j.Result = &model.Result{Summary: summary, SummarySHA256: model.Hash([]byte(summary)), Verification: []string{"BOUNDED_DAG_COMPLETED"}}
	}
	e.persist(j)
}

func (e *Engine) run(ctx context.Context, j model.Job) {
	defer func() {
		e.mu.Lock()
		delete(e.active, j.ID)
		e.mu.Unlock()
	}()
	var s execution.Snapshot
	var err error
	repo, _ := e.Repo(j.RepoID)
	if j.Task.DataClass == model.PrivateSourceData {
		// Do not persist copies of private source. An automatic retry may only
		// use the same approved snapshot, even after unrelated HEAD changes.
		s, err = execution.Capture(ctx, repo, j.Task)
		if err == nil && j.SnapshotID != "" && s.ID != j.SnapshotID {
			err = model.Error("SNAPSHOT_CHANGED", "private source snapshot changed during retry")
		}
	} else if j.SnapshotID == "" {
		s, err = execution.Capture(ctx, repo, j.Task)
		if err == nil {
			e.mu.Lock()
			if err = checkCapacity(e.snapshots, int64(16<<20)); err == nil {
				if _, statErr := os.Stat(filepath.Join(e.snapshots, s.ID+".json")); os.IsNotExist(statErr) {
					err = execution.SaveJSON(e.snapshots, s.ID+".json", s)
				} else {
					err = statErr
				}
			}
			e.mu.Unlock()
		}
	} else {
		s, err = execution.LoadSnapshot(e.snapshots, j.SnapshotID)
	}
	e.mu.Lock()
	j = e.jobs[j.ID]
	if err != nil || ctx.Err() != nil {
		e.finish(ctx, j, err)
		e.mu.Unlock()
		return
	}
	j.SnapshotID, j.BaseSHA, j.Dirty = s.ID, s.BaseSHA, s.Dirty
	j.Status = model.Queued
	e.persist(j)
	if len(j.Children) > 0 {
		e.mu.Unlock()
		return
	}
	planning := j.Task.Decompose
	requiredReview := j.Task.Mode == "patch" && repo.Limits.RequireIndependentReview
	if !planning && requiredReview && !e.independentReviewRequested(j) {
		j.Status, j.Reason = model.Blocked, "INDEPENDENT_REVIEW_REQUIRED"
		e.persist(j)
		e.mu.Unlock()
		return
	}
	trusted := !planning && j.Task.ExecutionProfile == model.TrustedLocalProfile
	implementation := !planning && (j.Task.ExecutionProfile == "implementation" || (j.Task.Mode == "patch" && (j.Task.PreferredProvider == "codex" || j.Task.PreferredProvider == "claude")))
	privateWrite := !planning && j.Task.DataClass == model.PrivateSourceData && j.Task.Mode == "patch"
	if j.Task.DataClass == model.PrivateSourceData && j.Task.Mode == "patch" && e.privateWriter == nil {
		j.Status, j.Reason = model.Blocked, "WRITER_ISOLATION_UNVERIFIED"
		e.persist(j)
		e.mu.Unlock()
		return
	}
	subagent := implementation || (!trusted && !privateWrite && j.Task.Mode == "patch" && slices.ContainsFunc(e.reg.Candidates, func(c model.Capability) bool {
		return (c.Provider == "codex" || c.Provider == "claude") && c.Status == "available" && repoAllowsProvider(repo, c.Provider)
	}))
	if implementation && !privateWrite && !slices.ContainsFunc(e.reg.Candidates, func(c model.Capability) bool {
		if c.Mode == "fixture" && (j.Task.PreferredProvider == "" || c.Provider == j.Task.PreferredProvider) {
			return true
		}
		if (c.Provider == "codex" || c.Provider == "claude") && c.Status == "available" && (j.Task.PreferredProvider == "" || c.Provider == j.Task.PreferredProvider) {
			return true
		}
		return false
	}) {
		j.Status, j.Reason = model.Blocked, "WRITER_ISOLATION_UNVERIFIED"
		e.persist(j)
		e.mu.Unlock()
		return
	}
	hasCheck := slices.ContainsFunc(j.Task.Acceptance, func(c model.Check) bool { return c.CheckID != "" })
	if !planning && !trusted && !privateWrite {
		if (j.Task.Risk == "critical" && !hasCheck) || (hasCheck && !subagent) {
			j.Status, j.Reason = model.Blocked, "DETERMINISTIC_CHECK_NOT_CONFIGURED"
			e.persist(j)
			e.mu.Unlock()
			return
		}
	}
	if trusted && (repo.TrustedLocal == nil || len(j.HandoffFrom) != 0 || j.Task.Risk == "critical" && !e.independentReviewRequested(j)) {
		j.Status, j.Reason = model.Blocked, "TRUSTED_LOCAL_POLICY_REQUIRED"
		e.persist(j)
		e.mu.Unlock()
		return
	}
	handoffs, err := e.resolveHandoffs(j)
	if err == nil && j.Task.ExecutionProfile == "review" &&
		(len(handoffs) != 1 || handoffs[0].Review == nil) {
		err = model.Error("HANDOFF_INVALID", "artifact review requires one independent verified diff handoff")
	}
	if err != nil {
		e.finish(ctx, j, err)
		e.mu.Unlock()
		return
	}
	now := time.Now()
	routed := j
	if planning {
		routed.Task = planningTask(j.Task, requiredReview || j.Task.Risk == "critical")
		routed.QualityRole = "planning"
	}
	c, budget, probeID := e.routeWithMode(&routed, now, false)
	j.Route = routed.Route
	if planning && len(j.Attempts) >= 2 {
		if e.cb != nil && c.Provider != "" {
			e.cb.Release(c.Provider, probeID)
		}
		c = model.Capability{}
	}
	if c.Provider == "" {
		finishWithoutRoute(&j)
		e.recordCapacityCodes(&j, now)
		e.persist(j)
		e.mu.Unlock()
		return
	}
	if e.fatal != nil {
		if e.cb != nil && c.Provider != "" {
			e.cb.Release(c.Provider, probeID)
		}
		e.mu.Unlock()
		return
	}
	checkpoint := latestCheckpoint(j)
	var selected model.Decision
	for _, d := range j.Route {
		if d.Reason == "SELECTED" {
			selected = d
			j.Task.AGYWeeklyFloor = d.WeeklyFloor
			break
		}
	}
	// Persist attempt intent before starting a subprocess.
	n := len(j.Attempts) + 1
	at := time.Now().UTC()
	dir := e.attemptDir(j.ID, n)
	if err = execution.NewAttemptDir(dir); err != nil {
		if e.cb != nil && c.Provider != "" {
			e.cb.Release(c.Provider, probeID)
		}
		e.finish(ctx, j, err)
		e.mu.Unlock()
		return
	}
	dispatchTask := routed.Task
	dispatchTask.AGYWeeklyFloor = j.Task.AGYWeeklyFloor
	if j.RoutingProfile != "" {
		dispatchTask.RoutingProfile = j.RoutingProfile
		dispatchTask.TargetEffort = selected.ResolvedEffort
	} else if matchesInitialRoute(j.InitialRoute, c) && dispatchTask.TargetEffort == "" {
		dispatchTask.TargetEffort = j.InitialRoute.ResolvedEffort
	}
	scope := dispatchTask.AttemptScope()
	j.Attempts = append(slices.Clone(j.Attempts), model.Attempt{
		EffectiveScope: &scope, ResolvedEffort: providers.AdapterEffort(c, dispatchTask), AGYWeeklyBuffer: selected.WeeklyBuffer,
		Number: n, ProbeID: probeID, Provider: c.Provider, Model: c.Model, Fingerprint: c.Fingerprint,
		OpenCodeAccount: c.OpenCodeAccount, OpenCodeTransport: c.OpenCodeTransport, OpenCodeEndpoint: c.OpenCodeEndpoint, OpenCodeRecheck: j.Task.OpenCodeRecheck, StartedAt: at,
		DeadlineAt: at.Add(budget), HeartbeatAt: at, Handoffs: handoffManifests(handoffs), Cost: c.Cost})
	if checkpoint != nil {
		j.Attempts[len(j.Attempts)-1].ResumedCheckpoint = checkpoint.ArtifactID
	}
	j.Status, j.Reason = model.Running, ""
	e.recordCapacityCodes(&j, now)
	e.persist(j)
	if e.fatal != nil {
		if e.cb != nil && c.Provider != "" {
			e.cb.Release(c.Provider, probeID)
		}
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	runCtx, stop := context.WithDeadline(ctx, at.Add(budget))
	files, err := s.Context(j.Task.AllowedPaths)
	var out execution.FixtureInput
	var worktree *execution.WorktreeResult
	var checks []model.CheckEvidence
	providerTask := dispatchTask
	if planning {
		files = nil
	}
	if j.Task.RoleCalibration {
		providerTask.Objective, providerTask.Acceptance, files = rolePrompt, nil, nil
	}
	if err == nil {
		if trusted {
			out, worktree, checks, err = e.runTrustedImplementation(runCtx, dir, c, dispatchTask, s, files, repo.TrustedLocal, checkpoint)
		} else if privateWrite {
			out, worktree, checks, err = e.runPrivateWriteImplementation(runCtx, dir, c, dispatchTask, s, files, handoffs, repo.PrivateSource)
		} else if implementation || (dispatchTask.Mode == "patch" && (c.Provider == "codex" || c.Provider == "claude" || hasCheck)) {
			if c.Provider == "codex" || c.Provider == "claude" || hasCheck || providers.UsesFixtureHarness(c, dispatchTask) {
				out, worktree, checks, err = e.runSubagentImplementation(runCtx, dir, c, dispatchTask, s, files, handoffs, repo)
			} else {
				out, worktree, err = e.runFixtureImplementation(runCtx, dir, c, dispatchTask, s, files, handoffs)
			}
		} else if providerTask.DataClass == model.PrivateSourceData {
			out, err = e.reg.RunPrivate(runCtx, dir, c, providerTask, files, handoffs, repo.PrivateSource)
		} else {
			out, err = e.reg.Run(runCtx, dir, c, providerTask, files, handoffs)
		}
	}
	if err == nil && out.Failure != "" {
		err = model.Error(out.Failure, "provider fixture failed")
	}
	if err == nil {
		err = runCtx.Err()
		if deadline, ok := runCtx.Deadline(); err == nil && ok && !time.Now().Before(deadline) {
			err = context.DeadlineExceeded
		}
	}
	stop()
	var planChildren []model.Job
	if planning && err == nil {
		planChildren, err = buildPlan(j, repo, out.Summary, time.Now().UTC())
	}

	e.mu.Lock()
	j = e.jobs[j.ID]
	if err == nil && ctx.Err() == nil && j.Status != model.Cancelling {
		j.Status = model.Verifying
		e.persist(j)
	}
	e.mu.Unlock()

	verification := []string{"SCOPED_CONTEXT", "PROCESS_EXIT"}
	if c.Mode == "fixture" {
		verification = append(verification, "FIXTURE_OUTPUT")
	} else if c.Provider == "opencode" {
		verification = append(verification, "ZERO_PRICE_CATALOG")
		if !trusted && !j.Task.NativeChild {
			verification = append(verification, "TOOL_FREE_TEXT")
		}
	} else if c.Provider == "agy" {
		verification = append(verification, "AGY_PROFILE_VERIFIED", "DOCUMENTED_COST_PROTECTION")
	}
	if j.Task.NativeChild && err == nil {
		var child *model.NativeChildEvidence
		if out.AGY != nil {
			child = out.AGY.NativeChild
		} else if out.OpenCode != nil {
			child = out.OpenCode.NativeChild
		}
		if child == nil || !child.ChildFinished {
			err = model.Error("INVALID_OUTPUT", "native child completion evidence is missing")
		} else {
			verification = append(verification, "NATIVE_CHILD_TERMINAL")
		}
	}
	if j.Task.FollowUp != "" && err == nil {
		var session *model.SessionEvidence
		if out.AGY != nil {
			session = out.AGY.Session
		} else if out.OpenCode != nil {
			session = out.OpenCode.Session
		}
		if session == nil || len(session.Turns) != 2 {
			err = model.Error("INVALID_OUTPUT", "native continuation did not verify both turns")
		} else {
			verification = append(verification, "NATIVE_SESSION_CONTINUATION")
		}
	}
	result := &model.Result{Summary: out.Summary, SummarySHA256: model.Hash([]byte(out.Summary)), Verification: verification, Cost: c.Cost, Checks: checks}
	if j.Task.DataClass == model.PrivateSourceData && repo.PrivateSource != nil {
		result.GrantDigest = model.Digest(repo.PrivateSource)
		result.SnapshotID = s.ID
		result.Verification = append(result.Verification, "PRIVATE_GRANT_BOUND")
	}
	if err == nil && c.Provider == "opencode" && c.Mode == "tool_free" && providers.OpenCodeIdentityMatches(c.Model, c.Fingerprint) {
		result.Cost = &model.CostEvidence{Policy: "FREE_ONLY", Status: "ZERO_PRICE_CATALOG", Documented: "VERIFIED",
			RuntimeVerified: "VERIFIED", Account: model.OpenCodeAnonymous}
	}
	if out.AGY != nil {
		result.Cost = out.AGY.Cost
	}
	if err == nil && j.Task.RoleCalibration {
		result.RoleCalibration, err = verifyRoleCalibration(out.Summary)
		if err == nil {
			result.Verification = append(result.Verification, "DETERMINISTIC_ROLE_CALIBRATION")
		}
	}

	if err == nil && ctx.Err() == nil {
		if out.Summary == "" || len(out.Summary) > 16384 || model.ContainsSecret([]byte(out.Summary)) {
			err = model.Error("INVALID_OUTPUT", "invalid or sensitive summary")
		}
		if err == nil && len(handoffs) == 1 && handoffs[0].Review != nil {
			result.Review, err = reviewResult(out.Summary, handoffs[0], j.Task.AllowedPaths)
			if err == nil {
				result.Verification = append(result.Verification, "ARTIFACT_BOUND_REVIEW")
			}
		}
		if err == nil && !planning && j.Task.Mode == "patch" {
			if worktree != nil {
				result.Preimages, result.Postimages = worktree.Preimages, worktree.Postimages
				result.Verification = append(result.Verification, "PRIVATE_WORKTREE", "SOURCE_SCOPE_AND_INDEX_UNCHANGED")
				if trusted {
					result.Verification = append(result.Verification, "TRUSTED_LOCAL_NATIVE_EDIT", "DETERMINISTIC_CHECK_PASSED")
					if checkpoint != nil {
						result.Verification = append(result.Verification, "CHECKPOINT_PREIMAGES_AND_ARTIFACT_VERIFIED")
					}
				} else if privateWrite {
					result.Verification = append(result.Verification, "SYNTHETIC_PRIVATE_WRITE", "DETERMINISTIC_CHECK_PASSED")
				} else {
					if len(checks) > 0 {
						result.Verification = append(result.Verification, "DETERMINISTIC_CHECK_PASSED")
					} else {
						result.Verification = append(result.Verification, "DETERMINISTIC_CHECK_NOT_RUN")
					}
				}
			} else {
				result.Preimages, err = execution.VerifyPatch(ctx, e.scratch, s, j.Task, []byte(out.Patch))
			}
			if err == nil {
				result.ArtifactID = j.ID
				if worktree != nil {
					result.ArtifactSHA256 = worktree.ArtifactSHA256
				} else {
					result.ArtifactSHA256 = model.Hash([]byte(out.Patch))
				}
				result.Verification = append(result.Verification, "PATCH_SCOPE", "PATCH_APPLIES_TO_SNAPSHOT")
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	j = e.jobs[j.ID]
	attemptDeadline := j.Attempts[len(j.Attempts)-1].DeadlineAt
	beforeSave := time.Now()
	if err == nil && ctx.Err() == nil && j.Status != model.Cancelling && result.ArtifactID != "" && beforeSave.Before(j.DeadlineAt) && beforeSave.Before(attemptDeadline) {
		err = e.saveArtifact(result.ArtifactID, []byte(out.Patch))
	}
	end := time.Now().UTC()
	if err == nil {
		switch {
		case j.Status == model.Cancelling:
			err = model.Error("CLIENT_CANCELLED", "job was cancelled before acceptance")
		case e.closed || (e.ctx != nil && e.ctx.Err() != nil):
			err = model.Error("OWNER_DISCONNECTED", "owner disconnected before acceptance")
		case !end.Before(j.DeadlineAt) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			err = model.Error("DEADLINE_EXPIRED", "deadline expired before acceptance")
		case ctx.Err() != nil:
			err = ctx.Err()
		case !end.Before(attemptDeadline):
			err = context.DeadlineExceeded
		case planning && len(e.jobs)+len(planChildren) > 1000:
			err = model.Error("RESOURCE_LIMIT", "planned tasks exceed local job capacity")
		}
	}
	j.Attempts = slices.Clone(j.Attempts)
	a := &j.Attempts[len(j.Attempts)-1]
	a.FinishedAt = &end
	a.HeartbeatAt = end
	a.Code = faultCode(err)
	a.Cost = result.Cost
	a.Checks = checks
	if trusted && worktree != nil && err != nil && !errors.Is(err, execution.ErrCleanup) {
		artifactID := fmt.Sprintf("%s-checkpoint-%d", j.ID, a.Number)
		if saveErr := e.saveArtifact(artifactID, worktree.Patch); saveErr == nil {
			a.Checkpoint = &model.Checkpoint{ArtifactID: artifactID, SHA256: worktree.ArtifactSHA256,
				SnapshotID: s.ID, SourceFingerprint: worktree.SourceFingerprint, Preimages: worktree.Preimages}
		} else if faultCode(err) == "CHECKPOINT_READY" {
			err = model.Error("ARTIFACT_SAVE_FAILED", "checkpoint could not be stored durably")
			a.Code = faultCode(err)
		}
	}
	a.OpenCode = out.OpenCode
	if out.Codex != nil {
		a.Codex = out.Codex
	}
	if out.Claude != nil {
		a.Claude = out.Claude
	}
	if out.AGY != nil {
		a.AGY, a.Cost = out.AGY, out.AGY.Cost
	}
	var fault *model.Fault
	if errors.As(err, &fault) {
		a.RetryAfterMS, a.RetrySource, a.RetryHints = fault.RetryAfterMS, fault.RetrySource, fault.RetryHints
		a.HTTPStatus, a.UpstreamStatus, a.ProviderReason = fault.HTTPStatus, fault.UpstreamStatus, fault.ProviderReason
		a.ProviderRateLimitScope = fault.ProviderRateLimitScope
	}
	if errors.Is(err, execution.ErrCleanup) {
		a.FinishedAt = nil
		e.orphaned[j.ID] = dir
	}
	if a.Provider == "opencode" && c.Mode != "fixture" {
		e.openCodeHistory.Apply(a)
	} else {
		e.rememberRateLimit(a)
	}
	if ctx.Err() == nil && !e.closed && e.settleEarlyAttempt(&j, a, end) {
		return
	}
	if err == nil {
		if planning {
			for _, child := range planChildren {
				j.Children = append(j.Children, child.ID)
			}
			j.Status, j.Reason = model.Queued, ""
			j.PlanSHA256 = model.Hash([]byte(out.Summary))
			e.persistFinalAttempt(j, planChildren)
			return
		}
		j.Result = result
		if out.Patch != "" && (result.ArtifactID != "" || j.Diff == "") {
			j.Diff = out.Patch
			j.Artifacts = maps.Clone(j.Artifacts)
			if j.Artifacts == nil {
				j.Artifacts = make(map[string]string)
			}
			if result.ArtifactID != "" {
				j.Artifacts[result.ArtifactID] = out.Patch
			}
		}
		j.Status, j.Reason = model.Completed, ""
	} else {
		j = e.failureState(ctx, j, err)
	}
	e.persistFinalAttempt(j, nil)
}

// persistFinalAttempt publishes worker health only after its final job snapshot
// (and any planner children) has committed. The caller holds the engine lock.
func (e *Engine) persistFinalAttempt(j model.Job, children []model.Job) {
	a := &j.Attempts[len(j.Attempts)-1]
	save := func() error {
		if len(children) == 0 {
			e.persist(j)
			return e.fatal
		}
		if err := e.db.Expand(j, children); err != nil {
			e.fatal = err
			e.stop()
			return err
		}
		e.jobs[j.ID] = j
		for _, child := range children {
			e.jobs[child.ID] = child
		}
		e.jobCond.Broadcast()
		return nil
	}
	if a.Provider == "opencode" {
		if err := e.commitOpenCodeCircuit(a, save); err == nil {
			e.recordOpenCodeObservation(a)
		}
		return
	}
	if err := save(); err == nil && e.cb != nil {
		if a.Code == "" {
			e.cb.probeSucceeded(a.Provider, a.ProbeID)
		} else {
			e.cb.Release(a.Provider, a.ProbeID)
		}
	}
}

func (e *Engine) settleEarlyAttempt(j *model.Job, a *model.Attempt, end time.Time) bool {
	if a.Code == reserveReason && j.Status != model.Cancelling && end.Before(j.DeadlineAt) {
		j.Status, j.Reason, j.ReadyAt = model.RetryWait, reserveReason, end
		e.persistFinalAttempt(*j, nil)
		return true
	}
	if a.Checkpoint != nil && j.Status != model.Cancelling && end.Before(j.DeadlineAt) {
		j.Status, j.Reason = model.Paused, "CHECKPOINT_AVAILABLE"
		e.persistFinalAttempt(*j, nil)
		return true
	}
	return false
}

func (e *Engine) attemptDir(id string, n int) string {
	return filepath.Join(e.scratch, fmt.Sprintf("%s-%d", id, n))
}

func matchesInitialRoute(r *model.InitialRoute, c model.Capability) bool {
	return r != nil && r.AutoRouted && normalizeProvider(c.Provider) == normalizeProvider(r.Provider) && c.Model == r.Model
}

type candidate struct {
	capability       model.Capability
	estimate, budget time.Duration
	decision         model.Decision
}

// eligibleCandidates evaluates a single locked snapshot, without reserving a probe.
// Callers supply the repository policy (nil for an unscoped auto-route preview).
func (e *Engine) eligibleCandidates(j *model.Job, now time.Time, repo *model.Repo) ([]candidate, string) {
	var options []candidate
	j.Route = nil
	controller := controllerWindow(e.quotas["codex"], weeklyMinutes, now)
	var controllerRemaining *float64
	if verifiedQuota(controller) {
		controllerRemaining = controller.Remaining
	}
	category := normalizedRoutingCategory(j.Task.Category)
	opusHealth := e.opusAdmissionHealth(now)
	recoveryFamily := checkpointProvider(*j)
	role := jobQualityRole(*j)
	liveRepo := repo != nil && e.cfg.EnableFixture && (repoAllowsProvider(*repo, "agy") || repoAllowsProvider(*repo, "opencode"))

	headrooms := map[string]HeadroomResult{}
	headroomFor := func(family string) (HeadroomResult, bool) {
		if h, ok := headrooms[family]; ok {
			return h, true
		}
		h, known := e.familyHeadroom(family, now)
		if known {
			headrooms[family] = h
		}
		return h, known
	}

	for _, c := range e.reg.Candidates {
		reason := ""
		normProvider := normalizeProvider(c.Provider)
		sameAccount := e.quotaAccountMatches(c)
		workerOpusHealth := 1.0
		if sameAccount {
			workerOpusHealth = opusHealth
		}
		admissionErr := ValidateModelAdmission(category, normProvider+"/"+c.Model, workerOpusHealth)
		quality := e.routeQuality(c, role, now)
		free := c.Provider == "opencode" && c.Mode != "fixture"
		family := ""
		if normProvider == "agy" {
			family = providers.AGYModelFamily(c.Model)
		}
		estimate := providers.Estimate(c, j.Task)
		budget, fits := providers.Budget(now, j.DeadlineAt, j.Task, estimate)
		switch {
		case repo != nil && !repoAllowsProvider(*repo, c.Provider):
			reason = "REPO_PROVIDER_POLICY"
		case c.Status != "available":
			reason = c.Reason
			if reason == "" {
				reason = "PROVIDER_UNAVAILABLE"
			}
		case j.Task.RoleCalibration && (c.Provider != j.Task.PreferredProvider || c.Model != j.Task.PreferredModel ||
			c.Provider == "opencode" && c.OpenCodeTransport != model.OpenCodeNativeReadAsk):
			reason = "ROLE_CALIBRATION_ROUTE_REQUIRED"
		case j.Task.OpenCodeRecheck && (c.Provider != "opencode" || c.Model != j.Task.PreferredModel ||
			c.OpenCodeTransport != model.OpenCodeNativeReadAsk || !providers.OpenCodeIdentityMatches(c.Model, c.Fingerprint)):
			reason = "OPENCODE_RECHECK_ROUTE_REQUIRED"
		case j.Task.DataClass == model.PrivateSourceData && repo == nil:
			reason = "PRIVATE_SOURCE_ROUTE_DENIED"
		case j.Task.DataClass == model.PrivateSourceData && !repo.PrivateSource.Allows(c, j.Task, now):
			_, reason = repo.PrivateSource.RouteDecision(c, j.Task, now)
			if reason == "" {
				reason = "PRIVATE_SOURCE_ROUTE_DENIED"
			}
		case !workerCostAllowed(c):
			reason = "ZERO_COST_UNVERIFIED"
		case quality.Tier == 0 || repo != nil && quality.Tier < repo.Limits.MinimumQualityTier:
			reason = "QUALITY_EVIDENCE_REQUIRED"
		case admissionErr != nil:
			reason = admissionErr.Error()
		case !providers.AllowsNativeChild(c, j.Task):
			reason = "NATIVE_CHILD_PROVIDER_UNAVAILABLE"
		case !providers.AllowsContinuation(c, j.Task):
			reason = "SESSION_PROVIDER_UNAVAILABLE"
		case !providers.AuditRouteAllows(c, j.Task, now):
			reason = "AUDIT_ROLE_OR_FRESHNESS_GATE"
		case len(j.M1OpenCodeRoutes) != 0 && (!free || !slices.ContainsFunc(j.M1OpenCodeRoutes, func(r model.OpenCodeRoute) bool {
			return r.Fingerprint == c.Fingerprint && OpenCodeGateApplies("model", r.Model, r.Account, r.OpenCodeTransport, c)
		})):
			reason = "M1_ROUTE_NOT_VERIFIED"
		case len(j.HandoffFrom) != 0 && !providers.AllowsHandoff(c, j.Task):
			reason = "HANDOFF_PROVIDER_POLICY"
		case j.Task.ExecutionProfile == model.TrustedLocalProfile && (repo == nil || repo.TrustedLocal == nil || !providers.AllowsTrustedLocal(c) ||
			repo.TrustedLocal.Provider != "" && c.Provider != repo.TrustedLocal.Provider):
			reason = "TRUSTED_LOCAL_PROVIDER_UNAVAILABLE"
		case j.Task.ExecutionProfile == "implementation" && j.Task.DataClass != model.PrivateSourceData &&
			!(c.Mode == "fixture" && (j.Task.PreferredProvider == "" || c.Provider == j.Task.PreferredProvider)) &&
			!((c.Provider == "codex" || c.Provider == "claude") && c.Status == "available" && (j.Task.PreferredProvider == "" || c.Provider == j.Task.PreferredProvider)):
			reason = "WRITER_ISOLATION_UNVERIFIED"
		case c.Provider == "agy" && !providers.AllowsAGYProfile(c, j.Task):
			reason = "AGY_PROFILE_UNVERIFIED"
		case liveRepo && c.Mode == "fixture" && j.Task.PreferredProvider != c.Provider && j.Task.Strategy != "offline_privacy_first":
			reason = "FIXTURE_ROUTE_REQUIRES_EXPLICIT_OFFLINE_POLICY"
		case c.Mode == "fixture" && j.Task.DataClass != "synthetic" && j.Task.DataClass != model.PrivateSourceData:
			reason = "FIXTURE_REQUIRES_SYNTHETIC_DATA"
		case c.Provider == "opencode" && j.Task.ExecutionProfile != model.TrustedLocalProfile && (j.Task.DataClass != "synthetic" && j.Task.DataClass != model.PrivateSourceData || j.Task.Mode != "read_only"):
			reason = "SYNTHETIC_READ_ONLY_PILOT"
		case j.Task.Strategy == "offline_privacy_first" && c.Mode != "fixture":
			reason = "OFFLINE_PROVIDER_REQUIRED"
		case e.distinctTaken(*j, c.Provider):
			reason = "DISTINCT_PROVIDER_REQUIRED"
		case recoveryFamily != "" && providers.Family(c.Provider) == recoveryFamily:
			reason = "CHECKPOINT_DISTINCT_PROVIDER_REQUIRED"
		case e.cb.stateAt(c.Provider, now) == CircuitOpen:
			reason = "CIRCUIT_BREAKER_OPEN"
		}
		d := model.Decision{Provider: c.Provider, Model: c.Model, Fingerprint: c.Fingerprint, Cost: c.Cost,
			OpenCodeAccount: c.OpenCodeAccount, OpenCodeTransport: c.OpenCodeTransport, EstimateSeconds: int(estimate.Seconds()),
			ControllerRemaining: controllerRemaining, TaskClass: j.Task.TaskClass, QualityRole: role, QualityTier: quality.Tier, QualitySource: quality.Source,
			FPTier: providers.FPTier(normProvider, c.Model)}
		if reason != "" {
			d.Reason = reason
			j.Route = append(j.Route, d)
			continue
		}
		d.PolicyEligible = true
		rows := e.quotas[normProvider]
		if !sameAccount {
			rows = nil
		}
		quota := workerQuota(rows, normProvider, now)
		var reserve reserveAdmission
		if family != "" {
			quota = tightestQuota(rows, family, 0, now)
			for _, bucket := range providers.AGYFamilyBuckets(family) {
				if !slices.ContainsFunc(rows, func(q providers.Quota) bool {
					return (q.Provider == "" || q.Provider == normProvider) && observerBucket(q.Bucket) == observerBucket(bucket) && q.Family == family && verifiedQuota(q.At(now))
				}) {
					d.WarningCodes = append(d.WarningCodes, "CAPACITY_UNVERIFIED")
					break
				}
			}
			reserve = e.agyReserve(*j, family, now, budget, controller, sameAccount)
			d.WeeklyFloor, d.WeeklyThreshold, d.WeeklyBuffer, d.ReserveOverride = reserve.floor, reserve.threshold, reserve.buffer, reserve.override
		}
		var healthScope string
		var gate openCodeHealth
		var recheckBlocked bool
		if free {
			healthScope, gate = e.openCodeGate(c, now)
			d.RecoveryState = e.openCodeRecovery(c, now)
			if healthScope != "" {
				d.CooldownScope = openCodeScope(gate.assertedScope)
			}
			if j.Task.OpenCodeRecheck {
				ok, retry := e.openCodeRecheck(c, now)
				if ok {
					healthScope = ""
					d.RecoveryState = "REVALIDATING"
				} else if healthScope == "" {
					recheckBlocked = true
					d.RetryAt = retry
				}
			}
		}
		d.CapacityState = quotaState(quota, e.cfg.CapacityPolicy.Defaults())
		switch {
		case !fits:
			reason = "DEADLINE_BUDGET"
		case verifiedQuota(quota) && *quota.Remaining == 0:
			reason = "QUOTA_EXHAUSTED"
			d.RetryAt = quota.ResetAt
		case reserve.reason != "":
			reason, quota, d.CapacityState = reserve.reason, reserve.weekly, "PROTECTED_RESERVE"
			d.WarningCodes = append(d.WarningCodes, reserveReason)
			d.RetryAt = quota.ResetAt
		case recheckBlocked:
			reason = "OPENCODE_RECHECK_DEFERRED"
		case healthScope != "":
			reason = "COOLDOWN"
			d.HealthScope, d.RetryAt, d.RetrySource = healthScope, &gate.until, gate.source
			d.QuotaObservedAt = timePointer(gate.observed)
			d.CapacityState = "UNAVAILABLE"
			if gate.code == "RATE_LIMIT" {
				d.CapacityState = "RATE_LIMITED"
			}
			d.WarningCodes = []string{"OPENCODE_FREE_UNAVAILABLE"}
		case latestCheckpoint(*j) == nil && !(j.ResumeRequest != "" && canRetryReview(*j)) && slices.ContainsFunc(j.Attempts, func(a model.Attempt) bool {
			return a.Provider == c.Provider && a.Model == c.Model &&
				(!free || OpenCodeGateApplies("model", a.Model, a.OpenCodeAccount, a.OpenCodeTransport, c))
		}):
			reason = "ALREADY_ATTEMPTED"
		case family != "" && e.agyFamilyInFlight(family):
			reason = "AGY_FAMILY_IN_FLIGHT"
			retry := now.Add(time.Second)
			d.RetryAt, d.HealthScope = &retry, "family"
		case free && e.openCodeInFlight(openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint)):
			reason = "OPENCODE_MODEL_IN_FLIGHT"
			retry := now.Add(time.Second)
			d.RetryAt, d.HealthScope = &retry, "model"
		case c.Provider != "opencode" && e.cooldown[providers.Family(c.Provider)].After(now):
			reason = "COOLDOWN"
			retry := e.cooldown[providers.Family(c.Provider)]
			d.RetryAt, d.HealthScope, d.CapacityState = &retry, "provider", "RATE_LIMITED"
		}
		d.QuotaBucket, d.QuotaRemaining, d.QuotaResetAt = quota.Bucket, quota.Remaining, quota.ResetAt
		if !quota.ObservedAt.IsZero() {
			d.QuotaObservedAt = timePointer(quota.ObservedAt)
		}
		// Usage-aware margin: blend the family's 5h and weekly windows into
		// H_effective. Below the critical threshold the family is a hard
		// barrier; below the degraded threshold it keeps working but loses
		// reserve priority.
		if family != "" {
			if h, known := headroomFor(family); sameAccount && known {
				d.HeadroomPercent, d.HeadroomState = h.Effective*100, h.State
				if reason == "" && h.Effective < quotaCriticalHeadroom {
					reason = "QUOTA_CRITICAL_HEADROOM"
					d.RetryAt = h.ResetsAt
				} else if reason == "" && h.Effective < quotaDegradedHeadroom {
					d.WarningCodes = append(d.WarningCodes, "HEADROOM_DEGRADED")
				}
			}
		} else if free && reason == "" {
			// The free pool has no numeric quota; an eligible route with no
			// active gate reports full headroom for F/P scoring.
			d.HeadroomPercent, d.HeadroomState = 100, headroomHealthy
		}
		if reason != "" {
			d.Reason = reason
			j.Route = append(j.Route, d)
			continue
		}
		options = append(options, candidate{c, estimate, budget, d})
	}
	// REMOVED: the rigid QUALITY_TIER_BELOW_BEST elimination that rejected
	// cheap/free models whenever any higher-tier model existed in the pool.
	// REPLACED WITH: explicit target tier gating and continuous F/P scoring.
	targetTier := effectiveTargetTier(j.Task)
	fallbackCheap := false
	if targetTier == "" && j.RoutingProfile == "" && len(j.Attempts) > 0 {
		// Fast fallback: an OpenCode HTTP 429 hands the job to the cheap
		// tier immediately instead of waiting out the free pool's cooldown.
		if last := j.Attempts[len(j.Attempts)-1]; last.Provider == "opencode" && last.Code == "RATE_LIMIT" {
			targetTier = "cheap"
			fallbackCheap = true
		}
	}
	if targetTier != "" {
		reject := func(c candidate, code string) {
			d := c.decision
			d.PolicyEligible, d.Reason, d.RetryAt, d.WarningCodes = false, code, nil, nil
			j.Route = append(j.Route, d)
		}
		options = slices.DeleteFunc(options, func(c candidate) bool {
			if code := targetTierViolation(targetTier, c.decision.FPTier, c.decision.QualityTier); code != "" {
				reject(c, code)
				return true
			}
			return false
		})
		if fallbackCheap {
			for i := range options {
				options[i].decision.WarningCodes = append(options[i].decision.WarningCodes, "FALLBACK_TO_CHEAP_TIER")
			}
		}
	}
	return options, targetTier
}

func (e *Engine) routeWithMode(j *model.Job, now time.Time, dryRun bool) (model.Capability, time.Duration, uint64) {
	repo := e.cfg.Repositories[j.RepoID]
	options, targetTier := e.eligibleCandidates(j, now, &repo)
	if len(options) == 0 || len(j.Attempts) >= 3 {
		return model.Capability{}, 0, 0
	}
	if j.RoutingProfile != "" {
		options = e.rankProfile(j, options, now)
	} else {
		e.rankLegacy(j, options, targetTier, now)
	}

	var probeID uint64
	var selected candidate
	for _, c := range options {
		d := c.decision
		d.Reason = "FALLBACK_CANDIDATE"
		if selected.capability.Provider == "" {
			allowed := true
			if !dryRun {
				probeID, allowed = e.cb.acquireAt(c.capability.Provider, now)
			}
			if allowed {
				selected = c
				d.Reason = "SELECTED"
			} else {
				d.Reason = "CIRCUIT_BREAKER_OPEN"
			}
		}
		j.Route = append(j.Route, d)
	}
	return selected.capability, selected.budget, probeID
}

func (e *Engine) rankLegacy(j *model.Job, options []candidate, targetTier string, now time.Time) {
	mode := j.RoutingMode
	if mode == "" {
		mode = routingMode(j.Task.Strategy, e.cfg.CapacityPolicy)
	}
	preference := func(c model.Capability) int {
		if j.Task.PreferredProvider == "" && j.Task.PreferredModel == "" && matchesInitialRoute(j.InitialRoute, c) {
			return 3
		}
		n := 0
		if c.Provider == j.Task.PreferredProvider || normalizeProvider(c.Provider) == normalizeProvider(j.Task.PreferredProvider) {
			n += 2
		}
		prefModel := strings.TrimPrefix(j.Task.PreferredModel, c.Provider+"/")
		if j.Task.PreferredProvider != "" {
			prefModel = strings.TrimPrefix(prefModel, j.Task.PreferredProvider+"/")
		}
		if parts := strings.SplitN(j.Task.PreferredModel, "/", 2); len(parts) == 2 {
			if c.Model == parts[1] {
				n++
				return n
			}
		}
		if c.Model == prefModel || c.Model == j.Task.PreferredModel {
			n++
		}
		return n
	}
	isFree := func(c model.Capability) bool { return c.Provider == "opencode" && c.Mode != "fixture" }
	reliability := map[[3]string]int{}
	for _, c := range options {
		k := [3]string{c.capability.Provider, c.capability.Model, c.capability.OpenCodeTransport}
		reliability[k] = e.workerReliability(c.capability, now)
	}
	// Explicit target tiers and auto routing alike rank by the
	// price/performance score:
	// Score(c) = wH*H_effective + wQ*Q_role - wC*Cost + ReliabilityBonus.
	fpScore := func(c candidate) float64 {
		wH, wQ, wC := fpWeights(targetTier)
		hEff := c.decision.HeadroomPercent / 100
		if c.decision.HeadroomState == "" {
			hEff = 1
		}
		return wH*hEff + wQ*float64(c.decision.QualityTier)/2 - wC*fpCost(c.capability) +
			0.05*float64(reliability[[3]string{c.capability.Provider, c.capability.Model, c.capability.OpenCodeTransport}])
	}
	slices.SortStableFunc(options, func(a, b candidate) int {
		if j.Task.Strategy == "latency_first" && a.estimate != b.estimate {
			return cmp.Compare(a.estimate, b.estimate)
		}
		preferFree := func() int {
			if isFree(a.capability) == isFree(b.capability) {
				return 0
			}
			if isFree(a.capability) {
				return -1
			}
			return 1
		}
		if mode != "conserve" {
			if diff := cmp.Compare(preference(b.capability), preference(a.capability)); diff != 0 {
				return diff
			}
		}
		if diff := cmp.Compare(fpScore(b), fpScore(a)); diff != 0 {
			return diff
		}
		key := func(c model.Capability) [3]string { return [3]string{c.Provider, c.Model, c.OpenCodeTransport} }
		if diff := cmp.Compare(reliability[key(b.capability)], reliability[key(a.capability)]); diff != 0 {
			return diff
		}
		if mode == "balanced" && a.estimate != b.estimate {
			return cmp.Compare(a.estimate, b.estimate)
		}
		if diff := preferFree(); diff != 0 {
			return diff
		}
		if mode == "conserve" {
			if diff := cmp.Compare(a.decision.WeeklyBuffer, b.decision.WeeklyBuffer); diff != 0 {
				return diff
			}
			if diff := cmp.Compare(preference(b.capability), preference(a.capability)); diff != 0 {
				return diff
			}
		}
		if a.estimate != b.estimate {
			return cmp.Compare(a.estimate, b.estimate)
		}
		if j.Task.Strategy == "latency_first" && a.capability.AuditLatencyMS > 0 && b.capability.AuditLatencyMS > 0 {
			if diff := cmp.Compare(a.capability.AuditLatencyMS, b.capability.AuditLatencyMS); diff != 0 {
				return diff
			}
		}
		return strings.Compare(a.capability.Provider, b.capability.Provider)
	})
}

func finishWithoutRoute(j *model.Job) {
	if len(j.Attempts) < 3 && !j.Task.OpenCodeRecheck && !j.Task.RoleCalibration {
		var retryAt time.Time
		var reason string
		for _, decision := range j.Route {
			if decision.RetryAt != nil && (retryAt.IsZero() || decision.RetryAt.Before(retryAt)) {
				estimate := time.Duration(decision.EstimateSeconds) * time.Second
				if _, fits := providers.Budget(*decision.RetryAt, j.DeadlineAt, j.Task, estimate); fits {
					retryAt, reason = *decision.RetryAt, decision.Reason
				}
			}
		}
		if !retryAt.IsZero() {
			j.Status, j.Reason, j.ReadyAt = model.RetryWait, reason, retryAt
			return
		}
	}
	j.Status, j.Reason = model.NotRun, "NO_ELIGIBLE_ROUTE"
	if len(j.Attempts) > 0 {
		last := j.Attempts[len(j.Attempts)-1]
		j.Status = model.Failed
		if last.Code != "" {
			j.Reason = last.Code
		}
		if last.HealthScope != "" || last.Code == reserveReason {
			j.Status = model.Blocked
		}
		return
	}
	if !slices.ContainsFunc(j.Route, func(d model.Decision) bool { return d.PolicyEligible }) &&
		slices.ContainsFunc(j.Route, func(d model.Decision) bool { return d.Reason == "QUALITY_EVIDENCE_REQUIRED" }) {
		j.Status, j.Reason = model.Blocked, "QUALITY_EVIDENCE_REQUIRED"
		return
	}
	for _, decision := range j.Route {
		switch decision.Reason {
		case "ZERO_COST_UNVERIFIED", "HANDOFF_PROVIDER_POLICY", "AGY_PROFILE_UNVERIFIED", "WRITER_ISOLATION_UNVERIFIED", "TRUSTED_LOCAL_PROVIDER_UNAVAILABLE", "BINARY_CHANGED",
			"COOLDOWN", "OPENCODE_MODEL_IN_FLIGHT", "OPENCODE_RECHECK_DEFERRED", "OPENCODE_RECHECK_ROUTE_REQUIRED", "ROLE_CALIBRATION_ROUTE_REQUIRED", "QUOTA_EXHAUSTED", "M1_ROUTE_NOT_VERIFIED", "QUALITY_EVIDENCE_REQUIRED", "PRICE_EVIDENCE_REQUIRED", "QUOTA_CRITICAL_HEADROOM", reserveReason, "AGY_WEEKLY_PACING",
			"ACCOUNT_ATTESTATION_REQUIRED", "ACCOUNT_MISMATCH", "OWNER_CONSENT_REQUIRED", "GRANT_EXPIRED", "DATA_HANDLING_UNREVIEWED", "PRIVATE_SOURCE_ROUTE_DENIED":
			j.Status, j.Reason = model.Blocked, decision.Reason
			return
		}
	}
}

func (e *Engine) distinctTaken(j model.Job, providerID string) bool {
	if j.Parent == "" {
		return false
	}
	p := e.jobs[j.Parent]
	if p.Task.Decompose && j.Task.ExecutionProfile == "review" {
		for _, id := range j.DependsOn {
			other := e.jobs[id]
			if other.Task.Mode == "patch" && slices.ContainsFunc(other.Attempts, func(a model.Attempt) bool { return providers.Family(a.Provider) == providers.Family(providerID) }) {
				return true
			}
		}
		return false
	}
	if !p.Distinct {
		return false
	}
	for _, child := range p.Children {
		if child != j.ID && slices.ContainsFunc(e.jobs[child].Attempts, func(a model.Attempt) bool { return providers.Family(a.Provider) == providers.Family(providerID) }) {
			return true
		}
	}
	return false
}

func faultCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, execution.ErrCleanup) {
		return "CLEANUP_UNCONFIRMED"
	}
	var f *model.Fault
	if errors.As(err, &f) {
		return f.Code
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "ATTEMPT_TIMEOUT"
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	default:
		return "EXECUTION_ERROR"
	}
}

func (e *Engine) finish(ctx context.Context, j model.Job, err error) {
	j = e.failureState(ctx, j, err)
	if e.cb != nil && len(j.Attempts) > 0 {
		a := j.Attempts[len(j.Attempts)-1]
		e.cb.Release(a.Provider, a.ProbeID)
	}
	e.persist(j)
}

func (e *Engine) failureState(ctx context.Context, j model.Job, err error) model.Job {
	code := faultCode(err)
	j.Status, j.Reason = model.Failed, code
	var fault *model.Fault
	if errors.As(err, &fault) && fault.Gate != "" && code == "POLICY_DENIED" {
		j.Reason = fault.Gate
	}
	switch {
	case code == "CLEANUP_UNCONFIRMED":
		j.Status = model.Blocked
	case e.jobs != nil && e.jobs[j.ID].Status == model.Cancelling:
		j.Status, j.Reason = model.Cancelled, "CLIENT_CANCELLED"
	case e.closed || (e.ctx != nil && e.ctx.Err() != nil):
		j.Status, j.Reason = model.Paused, "OWNER_DISCONNECTED"
	case !time.Now().Before(j.DeadlineAt) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		j.Status, j.Reason = model.TimedOut, "DEADLINE_EXPIRED"
	case (j.Task.OpenCodeRecheck || j.Task.RoleCalibration) && len(j.Attempts) > 0:
		if code == "RATE_LIMIT" || code == "ZERO_COST_UNVERIFIED" {
			j.Status = model.Blocked
		}
	case len(j.Attempts) > 0 && j.Attempts[len(j.Attempts)-1].HealthScope != "":
		j.Status = model.Blocked
		if len(j.Attempts) < 3 {
			j.Status, j.ReadyAt = model.RetryWait, time.Now()
		}
	case code == "DETERMINISTIC_CHECK_NOT_CONFIGURED" || code == "POLICY_DENIED" || code == "PRIVATE_SOURCE_ROUTE_DENIED" || code == "PERMISSION_DENIED" || code == "AGY_INIT_MISMATCH" || code == "AGY_INIT_INVALID" || code == "AGY_TOOL_SCOPE_UNVERIFIED" || code == "AGY_PROFILE_UNVERIFIED" || code == "QUOTA_EXHAUSTED" || code == "ZERO_COST_UNVERIFIED" || code == "HANDOFF_INVALID" || code == "REPO_MISMATCH" || code == "SNAPSHOT_CHANGED" || code == "WORKSPACE_ENTRY_UNSUPPORTED" || code == "BINARY_UNVERIFIED" || code == "BINARY_CHANGED" || code == "AUTH_REQUIRED" || code == "MODEL_UNVERIFIED":
		j.Status = model.Blocked
	case (code == "RATE_LIMIT" || code == "ATTEMPT_TIMEOUT" || code == "TEMPORARY_PROVIDER_ERROR") && len(j.Attempts) < 3 && len(j.Attempts) > 0:
		p := providers.Family(j.Attempts[len(j.Attempts)-1].Provider)
		now := time.Now()
		if code != "RATE_LIMIT" {
			history := slices.DeleteFunc(e.transient[p], func(t time.Time) bool { return now.Sub(t) > 5*time.Minute })
			e.transient[p] = append(history, now)
			if len(e.transient[p]) >= 3 {
				until := now.Add(time.Minute)
				last := j.Attempts[len(j.Attempts)-1]
				if (e.cb == nil || e.cb.tripUntil(last.Provider, until, last.ProbeID, true)) && until.After(e.cooldown[p]) {
					e.cooldown[p] = until
				}
			}
		}
		j.Status, j.ReadyAt = model.RetryWait, now.Add(time.Duration(len(j.Attempts))*time.Second)
	}
	return j
}

func (e *Engine) rememberRateLimit(attempt *model.Attempt) {
	if attempt.Provider == "opencode" || attempt.Code != "RATE_LIMIT" || attempt.FinishedAt == nil || attempt.RateLimitSuppressed {
		return
	}
	family := providers.Family(attempt.Provider)
	delay := max(time.Minute, providerRetryDelay(attempt.RetryAfterMS))
	until := attempt.FinishedAt.Add(delay)
	if e.cb != nil && !e.cb.tripUntil(attempt.Provider, until, attempt.ProbeID, true) {
		// A rejected observation must not become a new cooldown after restart.
		attempt.RateLimitSuppressed = true
		return
	}
	if until.After(e.cooldown[family]) {
		e.cooldown[family] = until
	}
}

func (e *Engine) saveArtifact(id string, b []byte) error {
	if err := checkCapacity(e.artifacts, int64(len(b))); err != nil {
		return err
	}
	return execution.Save(e.artifacts, id+".diff", b)
}

func checkCapacity(dir string, additional int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	total := additional
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
	}
	if total > 512<<20 {
		return model.Error("RESOURCE_LIMIT", "artifact capacity reached")
	}
	return nil
}

func (e *Engine) Artifact(ctx context.Context, j model.Job, id string) ([]byte, bool, error) {
	p, err := e.readVerifiedDiff(j, id, execution.MaxArtifact)
	if err != nil {
		return nil, false, err
	}
	s, err := execution.LoadSnapshot(e.snapshots, j.SnapshotID)
	if err != nil {
		return p, true, nil
	}
	return p, execution.SourceDrift(ctx, s, j.Result.Preimages), nil
}

func (e *Engine) collect(now time.Time) {
	for id, j := range e.jobs {
		if model.Terminal(j.Status) && now.Sub(j.CreatedAt) > 30*24*time.Hour {
			if err := e.db.Delete(id); err != nil {
				e.fatal = err
				e.stop()
				return
			}
			delete(e.jobs, id)
		}
	}
	referenced := map[string]bool{}
	for _, j := range e.jobs {
		if !model.Terminal(j.Status) || now.Sub(j.CreatedAt) < 7*24*time.Hour {
			referenced[j.SnapshotID+".json"] = true
			if j.Result != nil {
				referenced[j.Result.ArtifactID+".diff"] = true
			}
			for _, attempt := range j.Attempts {
				if attempt.Checkpoint != nil {
					referenced[attempt.Checkpoint.ArtifactID+".diff"] = true
				}
			}
		} else if j.Task.Objective != "" || j.Result != nil && j.Result.Summary != "" {
			j.Task.Objective = ""
			j.Task.FollowUp = ""
			j.Task.Acceptance = nil
			j.Task.AllowedPaths = nil
			j.Task.UntrackedPaths = nil
			if j.Result != nil {
				out := *j.Result
				out.Summary = ""
				out.Preimages = nil
				j.Result = &out
			}
			e.persist(j)
		}
	}
	for _, d := range []string{e.snapshots, e.artifacts} {
		entries, _ := os.ReadDir(d)
		for _, f := range entries {
			st, err := f.Info()
			if err == nil && st.Mode().IsRegular() && !referenced[f.Name()] && now.Sub(st.ModTime()) > 7*24*time.Hour {
				os.Remove(filepath.Join(d, f.Name()))
			}
		}
	}
}

// CompleteJobForTest completes a job with the given summary for testing purposes.
func (e *Engine) CompleteJobForTest(jobID string, summary string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if j, ok := e.jobs[jobID]; ok {
		j.Status = model.Completed
		j.Result = &model.Result{Summary: summary}
		e.persist(j)
	}
}

// CompleteRunningJobsForTest completes all non-terminal jobs for testing purposes.
func (e *Engine) CompleteRunningJobsForTest(summary string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range e.jobs {
		if !model.Terminal(j.Status) {
			j.Status = model.Completed
			j.Result = &model.Result{Summary: summary}
			e.persist(j)
		}
	}
}

// SetQuotaForTest sets cached provider quotas for testing purposes.
// Rows with Remaining != nil and zero ObservedAt default to time.Now().UTC()
// so test fixtures remain fresh under Quota.At(now).
func (e *Engine) SetQuotaForTest(provider string, rows []providers.Quota) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	cloned := slices.Clone(rows)
	for i := range cloned {
		if cloned[i].Remaining != nil && cloned[i].ObservedAt.IsZero() {
			cloned[i].ObservedAt = now
		}
	}
	e.rememberQuota(provider, cloned, now)
}
