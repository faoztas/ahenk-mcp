// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationToUserVersion2(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("query user_version failed: %v", err)
	}
	if version != 2 {
		t.Fatalf("expected user_version 2, got %d", version)
	}

	// Insert and retrieve quota state
	state := ProviderQuotaState{
		ProviderID:           "codex",
		TierName:             "codex_pro",
		WindowDurationHours:  5.0,
		RequestsUsedInWindow: 45,
		MaxWindowRequests:    180,
		HealthPct:            0.75,
		CircuitState:         "CLOSED",
	}
	if err := s.SaveQuotaState(state); err != nil {
		t.Fatalf("SaveQuotaState failed: %v", err)
	}
	states, err := s.GetQuotaStates()
	if err != nil || len(states) == 0 {
		t.Fatalf("GetQuotaStates failed: %v", err)
	}
	if states[0].ProviderID != "codex" || states[0].HealthPct != 0.75 {
		t.Errorf("quota state mismatch: %+v", states[0])
	}

	// Insert parent job for foreign key
	_, err = s.db.Exec("INSERT INTO jobs(id, repo, body) VALUES ('job-test-1', 'repo-1', '{}')")
	if err != nil {
		t.Fatalf("insert parent job failed: %v", err)
	}

	// Record routing audit
	audit := JobRoutingAudit{
		JobID:                   "job-test-1",
		TaskCategory:            "coding",
		AttemptNumber:           1,
		AssignedModel:           "codex/gpt-6.1-sol",
		AssignedEffort:          "high",
		RouteReason:             "NOMINAL_HIGH_EFFORT",
		QuotaPressureAtDispatch: 0.75,
		DispatchStatus:          "SUCCESS",
		LatencyMs:               780,
	}
	if err := s.RecordRoutingAudit(audit); err != nil {
		t.Fatalf("RecordRoutingAudit failed: %v", err)
	}
	history, err := s.GetRoutingAudit("job-test-1")
	if err != nil || len(history) != 1 {
		t.Fatalf("GetRoutingAudit failed: %v", err)
	}
	if history[0].AssignedModel != "codex/gpt-6.1-sol" {
		t.Errorf("audit history mismatch: %+v", history[0])
	}
}

func TestMigrationFromVersion1ToVersion2(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	// Create a v1 SQLite DB directly
	dbPath := filepath.Join(dir, "state.sqlite")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawDB.Exec(`
		PRAGMA journal_mode=WAL;
		CREATE TABLE jobs (id TEXT PRIMARY KEY, repo TEXT NOT NULL, request TEXT, digest TEXT, body BLOB NOT NULL, UNIQUE(repo, request));
		CREATE TABLE audit (seq INTEGER PRIMARY KEY, job TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE, at TEXT NOT NULL, status TEXT NOT NULL, reason TEXT NOT NULL);
		PRAGMA user_version=1;
	`)
	if err != nil {
		rawDB.Close()
		t.Fatal(err)
	}
	rawDB.Close()

	// Now open using store.Open which should migrate v1 -> v2
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open v1 db failed: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("query user_version failed: %v", err)
	}
	if version != 2 {
		t.Fatalf("expected user_version 2 after migration, got %d", version)
	}
}
