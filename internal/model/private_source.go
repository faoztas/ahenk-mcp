// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

const PrivateSourceData = "repository_internal"
const PrivateSourceMaxBytes = 32 << 10

// PrivateSourcePolicy is an operator grant, never a field of a submitted task.
type PrivateSourcePolicy struct {
	OwnerConsent             *bool             `json:"owner_consent,omitempty"`
	WriterIsolationVerified  bool              `json:"writer_isolation_verified,omitempty"`
	AllowWrite               bool              `json:"allow_write,omitempty"`
	RequireIndependentReview bool              `json:"require_independent_review,omitempty"`
	GoChecks                 map[string]string `json:"go_checks,omitempty"`
	ExpiresAt                time.Time         `json:"expires_at"`
	Files                    map[string]string `json:"files"`
	Briefs                   []string          `json:"approved_brief_sha256"`
	Routes                   []PrivateRoute    `json:"routes"`
}

type PrivateRoute struct {
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	Fingerprint           string `json:"binary_sha256"`
	Transport             string `json:"transport,omitempty"`
	ExpectedAccount       string `json:"expected_account,omitempty"`
	ExpectedAccountSHA256 string `json:"expected_account_sha256,omitempty"`
	DataHandlingReviewed  bool   `json:"data_handling_reviewed"`
}

func validSHA256(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && strings.ToLower(value) == value
}

func (p *PrivateSourcePolicy) ConsentApproved() bool {
	return p != nil && (p.OwnerConsent == nil || *p.OwnerConsent)
}

func (p *PrivateSourcePolicy) validate(c Config, r Repo) error {
	if p.ExpiresAt.IsZero() || len(p.Files) == 0 || len(p.Files) > 64 || len(p.Routes) == 0 || len(p.Routes) > 8 ||
		len(p.Briefs) == 0 || len(p.Briefs) > 64 ||
		r.TrustedLocal != nil || r.AllowAGYReserveOverride {
		return fmt.Errorf("invalid private source grant")
	}
	for name, hash := range p.Files {
		if !ValidTrustedLocalPath(name) || !InScope(name, r.AllowedPaths) || !validSHA256(hash) {
			return fmt.Errorf("private source grant requires exact file paths and SHA-256 digests")
		}
	}
	for _, brief := range p.Briefs {
		if !validSHA256(brief) {
			return fmt.Errorf("private source grant requires approved brief SHA-256 digests")
		}
	}
	if p.AllowWrite {
		if !p.WriterIsolationVerified || !p.RequireIndependentReview || !r.Limits.RequireIndependentReview || len(p.GoChecks) == 0 {
			return fmt.Errorf("private source write admission requires verified writer isolation, registered Go checks and independent review")
		}
		for id, name := range p.GoChecks {
			if !ValidID(id) || !ValidGoCheck(name) {
				return fmt.Errorf("invalid registered Go check in private source grant")
			}
		}
	}
	for _, route := range p.Routes {
		if !route.DataHandlingReviewed || !slices.Contains(r.Providers, route.Provider) ||
			!validSHA256(route.Fingerprint) {
			return fmt.Errorf("invalid or unreviewed private source route")
		}
		if route.ExpectedAccountSHA256 != "" && !validSHA256(route.ExpectedAccountSHA256) {
			return fmt.Errorf("invalid expected_account_sha256 in private source route")
		}
		if route.Provider == "agy" {
			if route.ExpectedAccount == "" && route.ExpectedAccountSHA256 == "" {
				return fmt.Errorf("private AGY is unavailable until the worker can attest its account")
			}
			if !slices.ContainsFunc(c.Providers, func(candidate ProviderConfig) bool {
				return candidate.ID == "agy" && candidate.Model == route.Model &&
					candidate.ExpectedBinarySHA256 == route.Fingerprint && candidate.EnableSynthetic
			}) {
				return fmt.Errorf("private AGY route is not pinned in the provider configuration")
			}
			continue
		}
		if c.EnableFixture && slices.Contains([]string{"fixture-a", "fixture-b", "fixture-c"}, route.Provider) {
			if route.Model != "synthetic" {
				return fmt.Errorf("fixture private route requires model synthetic")
			}
			continue
		}
		id, ok := strings.CutPrefix(route.Model, "opencode/")
		if route.Provider != "opencode" || !ok || !ValidID(id) || route.Transport != OpenCodeNativeReadAsk {
			return fmt.Errorf("private source requires an exact native read-only OpenCode route")
		}
		if !slices.ContainsFunc(c.Providers, func(candidate ProviderConfig) bool {
			return candidate.ID == route.Provider && candidate.Model == route.Model &&
				candidate.ExpectedBinarySHA256 == route.Fingerprint && candidate.OpenCodeTransport == route.Transport &&
				candidate.EnableSynthetic
		}) {
			return fmt.Errorf("private source route is not pinned in the provider configuration")
		}
	}
	return nil
}

func (p *PrivateSourcePolicy) Active(now time.Time) bool {
	return p != nil && p.ConsentApproved() && now.Before(p.ExpiresAt)
}

// PrivateBriefDigest binds the complete free-text payload in a private task.
func PrivateBriefDigest(objective string, acceptance []Check) string {
	return Digest(struct {
		Objective  string  `json:"objective"`
		Acceptance []Check `json:"acceptance"`
	}{objective, acceptance})
}

func (p *PrivateSourcePolicy) CheckRequest(t Task, now time.Time) *Fault {
	if p == nil {
		return GateError("POLICY_DENIED", "PRIVATE_GRANT_REQUIRED", "private source requires an explicit operator grant", "configure_private_source_grant")
	}
	if !p.ConsentApproved() {
		return GateError("POLICY_DENIED", "OWNER_CONSENT_REQUIRED", "private source requires explicit owner consent", "set_owner_consent_in_private_source_grant")
	}
	if !now.Before(p.ExpiresAt) {
		return GateError("POLICY_DENIED", "GRANT_EXPIRED", "private source grant has expired", "renew_private_source_grant")
	}
	if t.DataClass != PrivateSourceData {
		return GateError("POLICY_DENIED", "DATA_CLASS_MISMATCH", "private repository requires data_class repository_internal", "use_repository_internal_data_class")
	}
	if ContainsSecret([]byte(t.Objective)) || slices.ContainsFunc(t.Acceptance, func(check Check) bool {
		return ContainsSecret([]byte(check.Description))
	}) {
		return GateError("POLICY_DENIED", "SECRET_DETECTED", "private source task contains a secret pattern", "remove_secret_before_retry")
	}
	if ContainsPersonalData([]byte(t.Objective)) || slices.ContainsFunc(t.Acceptance, func(check Check) bool {
		return ContainsPersonalData([]byte(check.Description))
	}) {
		return GateError("POLICY_DENIED", "PERSONAL_DATA_CLASSIFIED", "private source task contains classified personal data", "remove_personal_data_before_retry")
	}
	if t.Mode == "patch" {
		if !p.WriterIsolationVerified || !p.AllowWrite {
			return GateError("POLICY_DENIED", "WRITER_ISOLATION_UNVERIFIED", "private write admission is blocked until writer isolation is verified", "verify_writer_isolation_and_enable_allow_write")
		}
		if !t.privateWriteAllowed(p) {
			return GateError("POLICY_DENIED", "DETERMINISTIC_CHECK_REQUIRED", "private write requires a scoped HEAD patch with a registered deterministic Go check", "configure_registered_go_check_for_private_patch")
		}
	} else if !t.privateReadOnly() {
		return GateError("POLICY_DENIED", "PRIVATE_MODE_UNSUPPORTED", "private source requires a scoped HEAD read-only or verified patch task", "use_scoped_head_only_private_task")
	}
	if t.ExecutionProfile != "review" && !slices.Contains(p.Briefs, PrivateBriefDigest(t.Objective, t.Acceptance)) {
		return GateError("POLICY_DENIED", "BRIEF_NOT_APPROVED", "private source requires the exact approved task brief digest", "approve_brief_digest_with_private_brief_digest")
	}
	seen := map[string]bool{}
	for _, name := range t.AllowedPaths {
		if _, ok := p.Files[name]; !ok || seen[name] {
			return GateError("POLICY_DENIED", "PATH_NOT_APPROVED", "private source requires unique exact approved file paths", "restrict_allowed_paths_to_approved_files")
		}
		seen[name] = true
	}
	if !slices.ContainsFunc(p.Routes, func(route PrivateRoute) bool {
		return route.DataHandlingReviewed &&
			(t.PreferredProvider == "" || route.Provider == t.PreferredProvider) &&
			(t.PreferredModel == "" || route.Model == t.PreferredModel)
	}) {
		return GateError("POLICY_DENIED", "PRIVATE_SOURCE_ROUTE_DENIED", "private source has no reviewed grant for the requested route", "pin_and_review_route_in_private_source_grant")
	}
	return nil
}

func (t Task) privateReadOnly() bool {
	return t.Mode == "read_only" && (t.WorkspaceMode == "" || t.WorkspaceMode == "head_only") &&
		(t.ExecutionProfile == "" || t.ExecutionProfile == "restricted" || t.ExecutionProfile == "analysis" || t.ExecutionProfile == "review") &&
		len(t.UntrackedPaths) == 0 && !t.Decompose && !t.NativeChild && !t.NativeChildBackground &&
		!t.OpenCodeRecheck && !t.RoleCalibration && t.FollowUp == "" && t.CheckpointObjective == "" &&
		t.AGYReserveOverride == "" && t.Risk != "critical" && !slices.ContainsFunc(t.Acceptance, func(check Check) bool {
		return check.CheckID != ""
	})
}

func (t Task) privateWriteAllowed(p *PrivateSourcePolicy) bool {
	if t.Mode != "patch" || (t.WorkspaceMode != "" && t.WorkspaceMode != "head_only") ||
		(t.ExecutionProfile != "" && t.ExecutionProfile != "implementation") ||
		len(t.UntrackedPaths) != 0 || t.Decompose || t.NativeChild || t.NativeChildBackground ||
		t.OpenCodeRecheck || t.RoleCalibration || t.FollowUp != "" || t.CheckpointObjective != "" ||
		t.AGYReserveOverride != "" || p == nil || !p.WriterIsolationVerified || !p.AllowWrite || len(p.GoChecks) == 0 {
		return false
	}
	hasCheck := false
	for _, check := range t.Acceptance {
		if check.CheckID != "" {
			if p.GoChecks[check.CheckID] == "" {
				return false
			}
			hasCheck = true
		}
	}
	return hasCheck
}

func (r PrivateRoute) AccountMatches(c Capability) (bool, string) {
	want := r.ExpectedAccount
	wantSHA := r.ExpectedAccountSHA256
	if want == "" && wantSHA == "" {
		if c.Provider == "opencode" && c.OpenCodeAccount == OpenCodeAnonymous {
			return true, ""
		}
		if c.Mode == "fixture" {
			return true, ""
		}
		return false, "ACCOUNT_ATTESTATION_REQUIRED"
	}
	actual := c.AttestedAccount
	if actual == "" && c.Provider == "opencode" {
		actual = c.OpenCodeAccount
	}
	actualSHA := c.AttestedAccountSHA256
	if actualSHA == "" && actual != "" {
		actualSHA = Hash([]byte(actual))
	}
	if actual == "" && actualSHA == "" {
		return false, "ACCOUNT_ATTESTATION_REQUIRED"
	}
	if want != "" && actual != want {
		return false, "ACCOUNT_MISMATCH"
	}
	if wantSHA != "" && actualSHA != wantSHA {
		return false, "ACCOUNT_MISMATCH"
	}
	return true, ""
}

func (p *PrivateSourcePolicy) RouteDecision(c Capability, t Task, now time.Time) (bool, string) {
	if p == nil {
		return false, "PRIVATE_SOURCE_ROUTE_DENIED"
	}
	if !p.ConsentApproved() {
		return false, "OWNER_CONSENT_REQUIRED"
	}
	if !now.Before(p.ExpiresAt) {
		return false, "GRANT_EXPIRED"
	}
	if t.DataClass != PrivateSourceData || c.AuditValidUntil != nil ||
		(t.PreferredProvider != "" && c.Provider != t.PreferredProvider) ||
		(t.PreferredModel != "" && c.Model != t.PreferredModel) {
		return false, "PRIVATE_SOURCE_ROUTE_DENIED"
	}
	if t.Mode == "patch" {
		if !p.WriterIsolationVerified || !p.AllowWrite {
			return false, "WRITER_ISOLATION_UNVERIFIED"
		}
		if !t.privateWriteAllowed(p) {
			return false, "DETERMINISTIC_CHECK_NOT_CONFIGURED"
		}
	} else if !t.privateReadOnly() {
		return false, "PRIVATE_SOURCE_ROUTE_DENIED"
	}
	lastGate := "PRIVATE_SOURCE_ROUTE_DENIED"
	for _, route := range p.Routes {
		if route.Provider != c.Provider || route.Model != c.Model {
			continue
		}
		if !route.DataHandlingReviewed {
			lastGate = "DATA_HANDLING_UNREVIEWED"
			continue
		}
		if c.Mode != "fixture" && route.Fingerprint != c.Fingerprint {
			lastGate = "BINARY_CHANGED"
			continue
		}
		if c.Provider == "opencode" {
			if c.Mode != "tool_free" || c.OpenCodeEndpoint != OpenCodeFreeEndpoint ||
				c.OpenCodeTransport != OpenCodeNativeReadAsk || route.Transport != c.OpenCodeTransport {
				lastGate = "PRIVATE_SOURCE_ROUTE_DENIED"
				continue
			}
		}
		if ok, gate := route.AccountMatches(c); !ok {
			lastGate = gate
			continue
		}
		return true, ""
	}
	return false, lastGate
}

func (p *PrivateSourcePolicy) Allows(c Capability, t Task, now time.Time) bool {
	ok, _ := p.RouteDecision(c, t, now)
	return ok
}

func (p *PrivateSourcePolicy) CheckContext(paths []string, files map[string]string) *Fault {
	if p == nil || len(paths) == 0 || len(paths) != len(files) {
		return GateError("POLICY_DENIED", "PATH_NOT_APPROVED", "private source context does not match the approved selection", "restrict_allowed_paths_to_approved_files")
	}
	size := 0
	for _, path := range paths {
		content, ok := files[path]
		if !ok {
			return GateError("POLICY_DENIED", "PATH_NOT_APPROVED", "private source path is missing from approved context", "restrict_allowed_paths_to_approved_files")
		}
		if ContainsSecret([]byte(content)) {
			return GateError("POLICY_DENIED", "SECRET_DETECTED", "private source file failed its secret check", "remove_secret_before_retry")
		}
		if ContainsPersonalData([]byte(content)) {
			return GateError("POLICY_DENIED", "PERSONAL_DATA_CLASSIFIED", "private source file contains classified personal data", "remove_personal_data_before_retry")
		}
		if p.Files[path] != Hash([]byte(content)) {
			return GateError("POLICY_DENIED", "DIGEST_MISMATCH", "private source file digest does not match the approved grant", "reapprove_file_digest_in_private_source_grant")
		}
		size += len(path) + len(content)
		if size > PrivateSourceMaxBytes {
			return GateError("POLICY_DENIED", "CONTEXT_LIMIT", "private source context exceeds size limit", "reduce_approved_file_scope")
		}
	}
	return nil
}
