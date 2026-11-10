// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestOpenCodePriceGate(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		free bool
	}{
		{`[{"input":0,"output":0,"cache":{"read":0,"write":0}}]`, true},
		{`[]`, false},
		{`[{"input":0,"output":0,"cache":{"read":0}}]`, false},
		{`[{"input":0,"output":0.01,"cache":{"read":0,"write":0}}]`, false},
		{`[{"input":0,"output":0,"cache":{"read":0,"write":0}},{"input":1,"output":0,"cache":{"read":0,"write":0}}]`, false},
	} {
		var ps []price
		if err := json.Unmarshal([]byte(tc.raw), &ps); err != nil {
			t.Fatal(err)
		}
		if got := freePrices(ps); got != tc.free {
			t.Fatalf("free=%v for %s", got, tc.raw)
		}
	}
}

func TestOpenCodeCatalogRequiresUniqueAnonymousZeroPriceChatModel(t *testing.T) {
	const zero = `[{"input":0,"output":0,"cache":{"read":0,"write":0}}]`
	var prices []price
	if err := json.Unmarshal([]byte(zero), &prices); err != nil {
		t.Fatal(err)
	}
	models := []openCodeCatalogModel{
		{Provider: "opencode", ID: "big-pickle", Cost: prices},
		{Provider: "opencode", ID: "alternate-free", Type: "chat", Cost: prices},
		{Provider: "opencode", ID: "duplicate", Cost: prices},
		{Provider: "opencode", ID: "duplicate", Cost: prices},
		{Provider: "opencode", ID: "no-price"},
		{Provider: "opencode", ID: "not-chat", Type: "image", Cost: prices},
		{Provider: "opencode-go", ID: "other-provider", Cost: prices},
	}
	got := openCodeFreeModels(models)
	if len(got) != 4 || !got["big-pickle"] || !got["alternate-free"] || got["duplicate"] || got["no-price"] {
		t.Fatalf("wrong catalog admission: %+v", got)
	}
	if !OpenCodeIdentityMatches("opencode/alternate-free", openCodeFingerprint) || OpenCodeIdentityMatches("opencode-go/other-provider", openCodeFingerprint) {
		t.Fatal("anonymous provider or pinned binary admission changed")
	}
}

func TestOpenCodeReadyURL(t *testing.T) {
	if got, err := loopbackURL("http://127.0.0.1:4321/"); err != nil || got != "http://127.0.0.1:4321" {
		t.Fatal(got, err)
	}
	for _, raw := range []string{
		"http://localhost:4321", "http://0.0.0.0:4321", "http://127.0.0.1:0",
		"http://127.0.0.1:4321/x", "http://127.0.0.1:4321/?y=z", "http://a@127.0.0.1:4321",
	} {
		if _, err := loopbackURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestOpenCodeUpstreamRateLimit(t *testing.T) {
	err := openCodeError(502, []byte(`{"_tag":"ProviderRequestError","status":429,"reason":"rate_limit","retryAfterMs":90000,"message":"provider payload"}`))
	fault, ok := err.(*model.Fault)
	if !ok || fault.Code != "RATE_LIMIT" || fault.RetryAfterMS != 90000 || fault.RetrySource != "provider_body" || fault.HTTPStatus != 502 || fault.UpstreamStatus != 429 || fault.ProviderReason != "rate_limit" || fault.Message == "provider payload" {
		t.Fatalf("unexpected fault: %+v", fault)
	}
}

func TestOpenCodeRateLimitHeaderAndTyped503(t *testing.T) {
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	if retryAfterMillis("21730", now) != 21_730_000 ||
		retryAfterMillis("Tue, 10 Nov 2026 00:00:00 GMT", now) != 21_600_000 ||
		retryAfterMillis("Mon, 09 Nov 2026 17:00:00 GMT", now) != 0 {
		t.Fatal("Retry-After seconds, HTTP-date or past date was interpreted incorrectly")
	}
	fault, ok := openCodeError(502, []byte(`{"_tag":"ProviderRequestError","status":503,"retryAfterMs":45000}`)).(*model.Fault)
	if !ok || fault.Code != "TEMPORARY_PROVIDER_ERROR" || fault.RetryAfterMS != 45_000 || fault.RetrySource != "provider_body" {
		t.Fatalf("expected fallback-eligible classification: %v", fault)
	}
}

func TestOpenCodeAccessAndBillingClasses(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, code string
	}{
		{401, "", "AUTH_REQUIRED"}, {403, "", "ACCESS_DENIED"}, {402, "", "ZERO_COST_UNAVAILABLE"},
		{502, `{"_tag":"ProviderRequestError","status":403,"reason":"authentication"}`, "ACCESS_DENIED"},
		{404, `{"_tag":"ModelNotFoundError"}`, "MODEL_UNAVAILABLE"},
	} {
		fault, ok := openCodeError(tc.status, []byte(tc.body)).(*model.Fault)
		if !ok || fault.Code != tc.code || fault.HTTPStatus != tc.status {
			t.Fatalf("status %d: %+v", tc.status, fault)
		}
	}
}

func TestOpenCodePublicQuotaKeepsDistinctExactIDs(t *testing.T) {
	body := []byte(`{"opencode":{"models":{
		"longcat-2.5-free":{"id":"longcat-2.5-free","limit":{"context":100000}},
		"deepseek-v4.1-flash-free":{"id":"deepseek-v4.1-flash-free"},
		"deepseek-v4.1-flash":{"id":"deepseek-v4.1-flash"},
		"retired":{"id":"retired","status":"deprecated"},
		"ambiguous":{"id":"another-id"}
	}},"other-provider":{"models":{"longcat-2.5-free":{"id":"longcat-2.5-free"}}}}`)
	rows, err := openCodePublicQuota(body)
	if err != nil || len(rows) != 3 {
		t.Fatalf("unexpected public catalog: %+v, %v", rows, err)
	}
	for i, want := range []string{"opencode/deepseek-v4.1-flash", "opencode/deepseek-v4.1-flash-free", "opencode/longcat-2.5-free"} {
		got := rows[i]
		if got.Model != want || got.State != "UNKNOWN" || got.Remaining != nil || got.ResetAt != nil || got.Source != "public_catalog" {
			t.Fatalf("catalog model was collapsed or credited with a measured quota: %+v", got)
		}
	}
}

func TestOpenCodeUnverifiedConfigurationKeepsEveryExactModel(t *testing.T) {
	r := New(model.Config{Providers: []model.ProviderConfig{
		{ID: "opencode", Model: "opencode/free-a"},
		{ID: "opencode", Model: "opencode/free-b"},
		{ID: "opencode", Model: "opencode/free-a"},
		{ID: "opencode", Model: "another-provider/free-b"},
	}})
	var got []string
	for _, c := range r.Candidates {
		if c.Provider == "opencode" {
			got = append(got, c.Model)
			if c.Status == "available" {
				t.Fatalf("config alone authorized an unverified candidate: %+v", c)
			}
		}
	}
	if !slices.Equal(got, []string{"opencode/free-a", "opencode/free-b", "another-provider/free-b"}) {
		t.Fatalf("configured models were lost or duplicated: %v", got)
	}
}

func TestOpenCodeErrorDoesNotInferSharedScopeFromAuthenticationOrMessage(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   string
		delay  int64
	}{
		{502, `{"_tag":"Generate.ProviderRequestError","status":403,"reason":"authentication","message":"account-wide wording","retryAfterMs":120000}`, "ACCESS_DENIED", 120000},
		{502, `{"_tag":"ProviderRequestError","status":401,"retryAfterMs":130000}`, "AUTH_REQUIRED", 130000},
		{502, `{"_tag":"ProviderRequestError","status":402,"retryAfterMs":140000}`, "ZERO_COST_UNAVAILABLE", 140000},
		{502, `{"_tag":"ProviderRequestError","status":500,"retryAfterMs":150000}`, "TEMPORARY_PROVIDER_ERROR", 150000},
		{502, `{"_tag":"ProviderRequestError","status":404}`, "MODEL_UNAVAILABLE", 0},
		{504, `{}`, "TEMPORARY_PROVIDER_ERROR", 0},
	} {
		fault, ok := openCodeError(tc.status, []byte(tc.body)).(*model.Fault)
		if !ok || fault.Code != tc.code || fault.RetryAfterMS != tc.delay || fault.ProviderRateLimitScope != "" {
			t.Fatalf("OpenCode error projected an unsupported scope or lost retry metadata: %+v", fault)
		}
	}
}

func TestOpenCodeHonorsOnlyExplicitRateLimitScope(t *testing.T) {
	err := openCodeNativeFault{Status: 429, RetryAfter: 120000, RateScope: "account"}.error()
	fault, ok := err.(*model.Fault)
	if !ok || fault.Code != "RATE_LIMIT" || fault.ProviderRateLimitScope != "account" || fault.RetryAfterMS != 120000 {
		t.Fatalf("explicit upstream rate limit lost: %+v", err)
	}
	for _, raw := range []string{
		`{"_tag":"ProviderRequestError","status":429,"rateLimitScope":"organization"}`,
		`{"_tag":"ProviderRequestError","status":403,"rateLimitScope":"account"}`,
	} {
		if f := openCodeError(502, []byte(raw)).(*model.Fault); f.ProviderRateLimitScope != "" {
			t.Fatalf("untrusted rate limit scope was projected: %+v", f)
		}
	}
}

func TestOpenCodeRetryUnitsAndInvalidHintsOC06OC07(t *testing.T) {
	now := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	f, ok := openCodeErrorAt(502, []byte(`{"_tag":"ProviderRequestError","status":429,"retryAfterMs":1500,"resetAt":"2026-11-10T16:00:02Z"}`), now).(*model.Fault)
	if !ok || f.Code != "RATE_LIMIT" || f.UpstreamStatus != 429 || f.RetryAfterMS != 2000 || len(f.RetryHints) != 2 {
		t.Fatalf("%+v", f)
	}
	addOpenCodeRetry(f, "http_retry_after_ms", "3000", "milliseconds", now, "")
	addOpenCodeRetry(f, "http_retry_after", "4", "seconds", now, "")
	addOpenCodeRetry(f, "http_retry_after", "Tue, 10 Nov 2026 16:02:06 GMT", "seconds", now, "Tue, 10 Nov 2026 16:02:00 GMT")
	addOpenCodeRetry(f, "http_retry_after_ms", "-1", "milliseconds", now, "")
	addOpenCodeRetry(f, "http_retry_after_ms", "999999999999999999999999", "milliseconds", now, "")
	if f.RetryAfterMS != 6000 || f.RetryHints[4].ResetAt == nil || !f.RetryHints[4].ResetAt.Equal(now.Add(6*time.Second)) ||
		f.RetryHints[5].Valid || f.RetryHints[6].Valid {
		t.Fatalf("bad normalization: %+v", f)
	}
}
