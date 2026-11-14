// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
)

func TestUpdateRoutingAuditStatus_AttemptScoped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// Insert parent job for foreign key
	_, err = s.db.Exec("INSERT INTO jobs(id, repo, body) VALUES ('job-multi-attempt', 'repo-1', '{}')")
	if err != nil {
		t.Fatalf("insert parent job failed: %v", err)
	}

	// Record routing audit for attempt 1
	audit1 := JobRoutingAudit{
		JobID:                   "job-multi-attempt",
		TaskCategory:            "coding",
		AttemptNumber:           1,
		AssignedModel:           "codex/gpt-5-sol",
		AssignedEffort:          "medium",
		RouteReason:             "INITIAL_ATTEMPT",
		QuotaPressureAtDispatch: 0.5,
		DispatchStatus:          "PENDING",
		LatencyMs:               0,
	}
	if err := s.RecordRoutingAudit(audit1); err != nil {
		t.Fatalf("RecordRoutingAudit attempt 1 failed: %v", err)
	}

	// Record routing audit for attempt 2
	audit2 := JobRoutingAudit{
		JobID:                   "job-multi-attempt",
		TaskCategory:            "coding",
		AttemptNumber:           2,
		AssignedModel:           "claude/claude-3-7-sonnet",
		AssignedEffort:          "high",
		RouteReason:             "FAILOVER_ATTEMPT",
		QuotaPressureAtDispatch: 0.8,
		DispatchStatus:          "PENDING",
		LatencyMs:               0,
	}
	if err := s.RecordRoutingAudit(audit2); err != nil {
		t.Fatalf("RecordRoutingAudit attempt 2 failed: %v", err)
	}

	// Update status of attempt 1 only
	if err := s.UpdateRoutingAuditStatus("job-multi-attempt", 1, "FAILED", 1500); err != nil {
		t.Fatalf("UpdateRoutingAuditStatus failed: %v", err)
	}

	history, err := s.GetRoutingAudit("job-multi-attempt")
	if err != nil || len(history) != 2 {
		t.Fatalf("GetRoutingAudit failed: %v, len=%d", err, len(history))
	}

	// Attempt 1 should be FAILED with 1500ms
	if history[0].AttemptNumber != 1 || history[0].DispatchStatus != "FAILED" || history[0].LatencyMs != 1500 {
		t.Errorf("attempt 1 was not properly updated: %+v", history[0])
	}

	// Attempt 2 should remain PENDING with 0ms (not overwritten by attempt 1 update)
	if history[1].AttemptNumber != 2 || history[1].DispatchStatus != "PENDING" || history[1].LatencyMs != 0 {
		t.Errorf("attempt 2 was incorrectly modified by attempt 1 update: %+v", history[1])
	}

	// Update status of attempt 2
	if err := s.UpdateRoutingAuditStatus("job-multi-attempt", 2, "SUCCESS", 2300); err != nil {
		t.Fatalf("UpdateRoutingAuditStatus attempt 2 failed: %v", err)
	}

	history2, err := s.GetRoutingAudit("job-multi-attempt")
	if err != nil || len(history2) != 2 {
		t.Fatalf("GetRoutingAudit failed: %v", err)
	}
	if history2[0].DispatchStatus != "FAILED" || history2[0].LatencyMs != 1500 {
		t.Errorf("attempt 1 corrupted after attempt 2 update: %+v", history2[0])
	}
	if history2[1].DispatchStatus != "SUCCESS" || history2[1].LatencyMs != 2300 {
		t.Errorf("attempt 2 was not properly updated: %+v", history2[1])
	}
}
