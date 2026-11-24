// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

const routingWebFixtureHost = "127.0.0.1:49152"
const routingWebFixtureOrigin = "http://" + routingWebFixtureHost
const routingWebFixtureBootstrap = "fixture-bootstrap"

type routingWebFixture struct {
	cfg     model.Config
	handler *routingWebPanel
}

func newRoutingWebFixture(t *testing.T) routingWebFixture {
	t.Helper()
	cfg := model.Config{
		StateDir: filepath.Join(t.TempDir(), "preferences"),
		Repositories: map[string]model.Repo{
			"project": {
				Root:                  t.TempDir(),
				AllowedPaths:          []string{"."},
				DefaultRoutingProfile: model.RoutingProfilePricePerformance,
			},
		},
	}
	return routingWebFixture{
		cfg:     cfg,
		handler: newRoutingWebPanel(routingWebFixtureHost, cfg, "project", routingWebFixtureBootstrap, time.Minute),
	}
}

func (f routingWebFixture) request(method, path, host, origin, session, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, routingWebFixtureOrigin+path, strings.NewReader(body))
	req.Host = host
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	resp := httptest.NewRecorder()
	f.handler.ServeHTTP(resp, req)
	return resp
}

func (f routingWebFixture) bootstrap(t *testing.T) string {
	t.Helper()
	resp := f.request(http.MethodPost, "/session", routingWebFixtureHost, routingWebFixtureOrigin, "", "code="+routingWebFixtureBootstrap)
	var data struct{ Session string }
	if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &data) != nil || data.Session == "" || data.Session == routingWebFixtureBootstrap {
		t.Fatalf("operator bootstrap failed with HTTP %d", resp.Code)
	}
	return data.Session
}

func TestRoutingWebUsesOperatorStore(t *testing.T) {
	f := newRoutingWebFixture(t)
	resp := f.request(http.MethodGet, "/", routingWebFixtureHost, "", "", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("panel GET returned HTTP %d", resp.Code)
	}
	for _, field := range []string{"Project", "Profile", "Source", "price_performance", "quality", "price", "Reset"} {
		if !strings.Contains(resp.Body.String(), field) {
			t.Errorf("panel missing %q", field)
		}
	}
	if strings.Contains(resp.Body.String(), routingWebFixtureBootstrap) {
		t.Fatal("panel exposed the bootstrap code")
	}
	session := f.bootstrap(t)
	check := func(want model.RoutingProfile, source, saved string) {
		t.Helper()
		resp := f.request(http.MethodGet, "/preference", routingWebFixtureHost, "", session, "")
		var state struct{ Project, Profile, Source string }
		err := json.Unmarshal(resp.Body.Bytes(), &state)
		if resp.Code != http.StatusOK || err != nil || state.Project != "project" || state.Profile != string(want) || state.Source != source {
			t.Fatalf("preference GET returned HTTP %d, state %+v, decode error %v", resp.Code, state, err)
		}
		got, err := store.ReadRoutingPreference(f.cfg.StateDir, "project", f.cfg.Repositories["project"].Root)
		if err != nil || got != saved {
			t.Fatalf("shared store read returned %q, %v; want %q", got, err, saved)
		}
	}
	check(model.RoutingProfilePricePerformance, "repo_config", "")
	if _, err := os.Stat(f.cfg.StateDir); !os.IsNotExist(err) {
		t.Fatalf("status GET touched the state directory: %v", err)
	}
	for _, profile := range model.RoutingProfiles() {
		resp := f.request(http.MethodPost, "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "profile="+string(profile))
		if resp.Code != http.StatusNoContent {
			t.Fatalf("selecting %s returned HTTP %d", profile, resp.Code)
		}
		check(profile, "operator", string(profile))
	}
	other := string(model.RoutingProfileQuality)
	if err := store.SaveRoutingPreference(f.cfg.StateDir, "project", f.cfg.Repositories["project"].Root, &other); err != nil {
		t.Fatal(err)
	}
	check(model.RoutingProfileQuality, "operator", other)
	resp = f.request(http.MethodPost, "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "reset=1")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("reset returned HTTP %d", resp.Code)
	}
	check(model.RoutingProfilePricePerformance, "repo_config", "")
	legacy := f.cfg.Repositories["project"]
	legacy.DefaultRoutingProfile = ""
	cfg := f.cfg
	cfg.Repositories = map[string]model.Repo{"project": legacy}
	f.handler = newRoutingWebPanel(routingWebFixtureHost, cfg, "project", routingWebFixtureBootstrap, time.Minute)
	session = f.bootstrap(t)
	check("", "legacy", "")
}

func TestRoutingWebRejectsStateAliasedIntoAnotherRepository(t *testing.T) {
	for _, tc := range []struct {
		name, method, origin, body string
	}{
		{"set", http.MethodPost, routingWebFixtureOrigin, "profile=quality"},
		{"reset", http.MethodPost, routingWebFixtureOrigin, "reset=1"},
		{"read", http.MethodGet, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstRepo, secondRepo := t.TempDir(), t.TempDir()
			alias := filepath.Join(t.TempDir(), "state-parent")
			if err := os.Symlink(t.TempDir(), alias); err != nil {
				t.Fatal(err)
			}
			cfg := model.Config{
				StateDir: filepath.Join(alias, "preferences"),
				Repositories: map[string]model.Repo{
					"project": {Root: firstRepo},
					"other":   {Root: secondRepo},
				},
			}
			f := routingWebFixture{
				handler: newRoutingWebPanel(routingWebFixtureHost, cfg, "project", routingWebFixtureBootstrap, time.Minute),
			}
			session := f.bootstrap(t)
			resp := f.request(http.MethodGet, "/preference", routingWebFixtureHost, "", session, "")
			if resp.Code != http.StatusOK {
				t.Fatalf("initial read returned HTTP %d", resp.Code)
			}

			// Move the alias after sign-in so each preference operation must validate it.
			target := filepath.Join(secondRepo, "preferences")
			initial := string(model.RoutingProfilePrice)
			if err := store.SaveRoutingPreference(target, "project", firstRepo, &initial); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 1 {
				t.Fatalf("finding seeded preference: %d entries, %v", len(entries), err)
			}
			record := filepath.Join(target, entries[0].Name())
			before, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secondRepo, alias); err != nil {
				t.Fatal(err)
			}

			resp = f.request(tc.method, "/preference", routingWebFixtureHost, tc.origin, session, tc.body)
			if resp.Code != http.StatusInternalServerError {
				t.Errorf("got HTTP %d, want %d for unsafe state directory", resp.Code, http.StatusInternalServerError)
			}
			for _, path := range []string{firstRepo, secondRepo, alias} {
				if strings.Contains(resp.Body.String(), path) {
					t.Error("response exposed a filesystem path")
				}
			}
			after, err := os.ReadFile(record)
			if err != nil || !bytes.Equal(after, before) {
				t.Errorf("preference record changed: %v", err)
			}
			entries, err = os.ReadDir(target)
			if err != nil || len(entries) != 1 {
				t.Errorf("state directory contents changed: %d entries, %v", len(entries), err)
			}
		})
	}
}

func TestRoutingWebMutationRequiresLocalOperator(t *testing.T) {
	f := newRoutingWebFixture(t)
	initial := string(model.RoutingProfilePrice)
	if err := store.SaveRoutingPreference(f.cfg.StateDir, "project", f.cfg.Repositories["project"].Root, &initial); err != nil {
		t.Fatal(err)
	}
	records, err := filepath.Glob(filepath.Join(f.cfg.StateDir, "routing-preference-*"))
	if err != nil || len(records) != 1 {
		t.Fatalf("finding preference record: %d records, %v", len(records), err)
	}
	before, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	session := f.bootstrap(t)
	for _, tc := range []struct {
		name, method, path, host, origin, session, body string
		status                                          int
	}{
		{"wrong host", "POST", "/preference", "localhost:49152", routingWebFixtureOrigin, session, "profile=quality", 403},
		{"wrong origin", "POST", "/preference", routingWebFixtureHost, "http://127.0.0.1:49153", session, "profile=quality", 403},
		{"missing origin", "POST", "/preference", routingWebFixtureHost, "", session, "profile=quality", 403},
		{"null origin", "POST", "/preference", routingWebFixtureHost, "null", session, "profile=quality", 403},
		{"missing session", "POST", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, "", "profile=quality", 401},
		{"wrong session", "POST", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, "wrong", "profile=quality", 401},
		{"bootstrap is not a session", "POST", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, routingWebFixtureBootstrap, "profile=quality", 401},
		{"unauthenticated status", "GET", "/preference", routingWebFixtureHost, "", "", "", 401},
		{"foreign status origin", "GET", "/preference", routingWebFixtureHost, "https://example.test", session, "", 403},
		{"GET carrying state change", "GET", "/preference?profile=quality", routingWebFixtureHost, "", session, "", 400},
		{"disallowed method", "PUT", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "profile=quality", 405},
		{"unknown mode", "POST", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "profile=unknown", 400},
		{"duplicate mode", "POST", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "profile=quality&profile=price", 400},
		{"mixed operation", "POST", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "profile=quality&reset=1", 400},
		{"oversized body", "POST", "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "profile=" + strings.Repeat("x", 2048), 400},
		{"replayed bootstrap", "POST", "/session", routingWebFixtureHost, routingWebFixtureOrigin, "", "code=" + routingWebFixtureBootstrap, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.request(tc.method, tc.path, tc.host, tc.origin, tc.session, tc.body)
			if resp.Code != tc.status {
				t.Errorf("got HTTP %d, want %d", resp.Code, tc.status)
			}
			if strings.Contains(resp.Body.String(), routingWebFixtureBootstrap) || strings.Contains(resp.Body.String(), session) {
				t.Error("response exposed operator credentials")
			}
			got, err := os.ReadFile(records[0])
			if err != nil || !bytes.Equal(got, before) {
				t.Fatalf("rejected request changed preference bytes: %v", err)
			}
		})
	}
	for _, flag := range []string{"--bind=0.0.0.0", "--port=48111"} {
		err := runRoutingWeb([]string{"--config", "unused.json", "--repo", "project", flag})
		if err == nil || !strings.HasPrefix(err.Error(), "usage: ") {
			t.Errorf("listener option %s did not fail flag validation", flag)
		}
	}
	resp := f.request(http.MethodPost, "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "profile=quality")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("authorized update returned HTTP %d", resp.Code)
	}
	got, err := os.ReadFile(records[0])
	if err != nil || string(got) != "quality\n" {
		t.Fatalf("authorized request failed to write preference: %q, %v", got, err)
	}

	f.handler.mu.Lock()
	f.handler.lastActive = time.Now().Add(-2 * f.handler.idle)
	f.handler.mu.Unlock()
	resp = f.request(http.MethodPost, "/preference", routingWebFixtureHost, routingWebFixtureOrigin, session, "reset=1")
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("expired session returned HTTP %d", resp.Code)
	}
	afterExpiry, err := os.ReadFile(records[0])
	if err != nil || !bytes.Equal(afterExpiry, got) {
		t.Fatalf("expired session changed preference bytes: %v", err)
	}
}

func TestRoutingWebBootstrapIsSingleUse(t *testing.T) {
	f := newRoutingWebFixture(t)
	for _, origin := range []string{"", "https://example.test"} {
		resp := f.request("POST", "/session", routingWebFixtureHost, origin, "", "code="+routingWebFixtureBootstrap)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("bootstrap with invalid origin returned HTTP %d", resp.Code)
		}
	}
	if resp := f.request("POST", "/session", routingWebFixtureHost, routingWebFixtureOrigin, "", "code=wrong"); resp.Code != http.StatusUnauthorized {
		t.Fatalf("incorrect bootstrap code returned HTTP %d", resp.Code)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			codes <- f.request("POST", "/session", routingWebFixtureHost, routingWebFixtureOrigin, "", "code="+routingWebFixtureBootstrap).Code
		})
	}
	wg.Wait()
	a, b := <-codes, <-codes
	if a+b != http.StatusOK+http.StatusUnauthorized || a == b {
		t.Fatalf("concurrent bootstrap returned HTTP %d and %d", a, b)
	}
}

func TestRoutingWebClosesLoopbackPort(t *testing.T) {
	for _, cancelAfterLogin := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelAfterLogin), func(t *testing.T) {
			f := newRoutingWebFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type launch struct{ origin, code string }
			start := make(chan launch, 1)
			done := make(chan error, 1)
			idle := 150 * time.Millisecond
			if cancelAfterLogin {
				idle = time.Minute
			}
			go func() {
				done <- serveRoutingWeb(ctx, f.cfg, "project", idle, func(origin, code string) error {
					start <- launch{origin, code}
					return nil
				})
			}()
			var panel launch
			select {
			case panel = <-start:
			case err := <-done:
				t.Fatalf("panel exited before startup: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("panel startup timed out")
			}
			u, err := url.Parse(panel.origin)
			if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Port() == "0" {
				t.Fatal("panel did not announce a loopback ephemeral port")
			}
			if cancelAfterLogin {
				tr := &http.Transport{DisableKeepAlives: true}
				defer tr.CloseIdleConnections()
				client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, panel.origin+"/session", strings.NewReader(url.Values{"code": {panel.code}}.Encode()))
				if err != nil {
					t.Fatal("could not construct bootstrap request")
				}
				req.Header.Set("Origin", panel.origin)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("could not reach loopback panel: %v", err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("loopback bootstrap returned HTTP %d", resp.StatusCode)
				}
				cancel()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("panel did not shut down")
			}
			conn, err := net.DialTimeout("tcp4", u.Host, 250*time.Millisecond)
			if err == nil {
				conn.Close()
				t.Fatal("panel still accepts connections after shutdown")
			}
		})
	}
}

func TestRoutingWebInvalidPreferenceShowsFallback(t *testing.T) {
	f := newRoutingWebFixture(t)
	value := "quality"
	if err := store.SaveRoutingPreference(f.cfg.StateDir, "project", f.cfg.Repositories["project"].Root, &value); err != nil {
		t.Fatal(err)
	}
	records, err := filepath.Glob(filepath.Join(f.cfg.StateDir, "routing-preference-*"))
	if err != nil || len(records) != 1 {
		t.Fatalf("expected one preference record, got %d: %v", len(records), err)
	}
	const malformed = "invalid-private-marker"
	if err := os.WriteFile(records[0], []byte(malformed), 0600); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodGet, "/preference", routingWebFixtureHost, "", f.bootstrap(t), "")
	var state struct{ Profile, Source, Warning string }
	if err := json.Unmarshal(resp.Body.Bytes(), &state); resp.Code != http.StatusOK || err != nil ||
		state.Profile != "price_performance" || state.Source != "repo_config" ||
		state.Warning != "INVALID_ROUTING_PREFERENCE" || strings.Contains(resp.Body.String(), malformed) {
		t.Fatalf("invalid preference response: HTTP %d, state %+v, decode %v", resp.Code, state, err)
	}
	got, err := os.ReadFile(records[0])
	if err != nil || string(got) != malformed {
		t.Fatal("GET modified the invalid record")
	}
}

func TestRoutingWebCLIDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-config-marker")
	args := []string{"--config", path, "--repo", "example"}
	for _, invoke := range []struct {
		name string
		run  func() error
	}{
		{"startup", func() error { return runRoutingWeb(args) }},
		{"CLI", func() error {
			_, err := runRoutingTestCLI(t, append([]string{"web"}, args...)...)
			return err
		}},
	} {
		t.Run(invoke.name, func(t *testing.T) {
			if err := invoke.run(); err == nil || err.Error() != "cannot load routing configuration" {
				t.Fatal("expected a bounded config-load diagnostic from the web command")
			}
		})
	}
}
