// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"time"
)

// ProviderQuotaState captures real-time quota telemetry and circuit breaker state.
type ProviderQuotaState struct {
	ProviderID           string     `json:"provider_id"`
	TierName             string     `json:"tier_name"`
	WindowDurationHours  float64    `json:"window_duration_hours"`
	RequestsUsedInWindow int        `json:"requests_used_in_window"`
	MaxWindowRequests    int        `json:"max_window_requests"`
	HealthPct            float64    `json:"health_pct"`
	CircuitState         string     `json:"circuit_state"`
	RetryAfterUTC        *time.Time `json:"retry_after_utc,omitempty"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// JobRoutingAudit captures routing decision metadata for a job attempt.
type JobRoutingAudit struct {
	JobID                   string    `json:"job_id"`
	TaskCategory            string    `json:"task_category"`
	AttemptNumber           int       `json:"attempt_number"`
	AssignedModel           string    `json:"assigned_model"`
	AssignedEffort          string    `json:"assigned_effort"`
	RouteReason             string    `json:"route_reason"`
	QuotaPressureAtDispatch float64   `json:"quota_pressure_at_dispatch"`
	DispatchStatus          string    `json:"dispatch_status"`
	LatencyMs               int64     `json:"latency_ms"`
	CreatedAt               time.Time `json:"created_at"`
}

// SaveQuotaState inserts or updates provider quota telemetry.
func (s *Store) SaveQuotaState(state ProviderQuotaState) error {
	now := time.Now().UTC()
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = now
	}
	var retryAfter *string
	if state.RetryAfterUTC != nil {
		formatted := state.RetryAfterUTC.UTC().Format(time.RFC3339Nano)
		retryAfter = &formatted
	}
	updatedAtStr := state.UpdatedAt.UTC().Format(time.RFC3339Nano)

	query := `
	INSERT INTO provider_quota_state (
		provider_id, tier_name, window_duration_hours, requests_used_in_window,
		max_window_requests, health_pct, circuit_state, retry_after_utc, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(provider_id) DO UPDATE SET
		tier_name=excluded.tier_name,
		window_duration_hours=excluded.window_duration_hours,
		requests_used_in_window=excluded.requests_used_in_window,
		max_window_requests=excluded.max_window_requests,
		health_pct=excluded.health_pct,
		circuit_state=excluded.circuit_state,
		retry_after_utc=excluded.retry_after_utc,
		updated_at=excluded.updated_at;`

	_, err := s.db.Exec(query,
		state.ProviderID,
		state.TierName,
		state.WindowDurationHours,
		state.RequestsUsedInWindow,
		state.MaxWindowRequests,
		state.HealthPct,
		state.CircuitState,
		retryAfter,
		updatedAtStr,
	)
	return err
}

// GetQuotaStates returns all tracked provider quota states.
func (s *Store) GetQuotaStates() ([]ProviderQuotaState, error) {
	query := `
	SELECT provider_id, tier_name, window_duration_hours, requests_used_in_window,
	       max_window_requests, health_pct, circuit_state, retry_after_utc, updated_at
	FROM provider_quota_state
	ORDER BY provider_id ASC;`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []ProviderQuotaState
	for rows.Next() {
		var item ProviderQuotaState
		var retryAfter sql.NullString
		var updatedAtStr string

		if err := rows.Scan(
			&item.ProviderID,
			&item.TierName,
			&item.WindowDurationHours,
			&item.RequestsUsedInWindow,
			&item.MaxWindowRequests,
			&item.HealthPct,
			&item.CircuitState,
			&retryAfter,
			&updatedAtStr,
		); err != nil {
			return nil, err
		}

		if retryAfter.Valid && retryAfter.String != "" {
			if t, err := parseTime(retryAfter.String); err == nil {
				item.RetryAfterUTC = &t
			}
		}
		if t, err := parseTime(updatedAtStr); err == nil {
			item.UpdatedAt = t
		}
		states = append(states, item)
	}
	return states, rows.Err()
}

// RecordRoutingAudit inserts a routing audit record for a job attempt.
func (s *Store) RecordRoutingAudit(audit JobRoutingAudit) error {
	now := time.Now().UTC()
	if audit.CreatedAt.IsZero() {
		audit.CreatedAt = now
	}
	createdAtStr := audit.CreatedAt.UTC().Format(time.RFC3339Nano)

	query := `
	INSERT INTO job_routing_audit (
		job_id, task_category, attempt_number, assigned_model, assigned_effort,
		route_reason, quota_pressure_at_dispatch, dispatch_status, latency_ms, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(job_id, attempt_number) DO UPDATE SET
		task_category=excluded.task_category,
		assigned_model=excluded.assigned_model,
		assigned_effort=excluded.assigned_effort,
		route_reason=excluded.route_reason,
		quota_pressure_at_dispatch=excluded.quota_pressure_at_dispatch,
		dispatch_status=excluded.dispatch_status,
		latency_ms=excluded.latency_ms,
		created_at=excluded.created_at;`

	_, err := s.db.Exec(query,
		audit.JobID,
		audit.TaskCategory,
		audit.AttemptNumber,
		audit.AssignedModel,
		audit.AssignedEffort,
		audit.RouteReason,
		audit.QuotaPressureAtDispatch,
		audit.DispatchStatus,
		audit.LatencyMs,
		createdAtStr,
	)
	return err
}

// UpdateRoutingAuditStatus updates the dispatch status and latency of existing routing audit records for a job ID and attempt number.
func (s *Store) UpdateRoutingAuditStatus(jobID string, attemptNumber int, status string, latencyMs int64) error {
	if attemptNumber <= 0 {
		query := `
		UPDATE job_routing_audit
		SET dispatch_status = ?, latency_ms = ?
		WHERE job_id = ?;`
		_, err := s.db.Exec(query, status, latencyMs, jobID)
		return err
	}
	query := `
	UPDATE job_routing_audit
	SET dispatch_status = ?, latency_ms = ?
	WHERE job_id = ? AND attempt_number = ?;`
	_, err := s.db.Exec(query, status, latencyMs, jobID, attemptNumber)
	return err
}

// GetRoutingAudit retrieves routing history for a given job ID.
func (s *Store) GetRoutingAudit(jobID string) ([]JobRoutingAudit, error) {
	query := `
	SELECT job_id, task_category, attempt_number, assigned_model, assigned_effort,
	       route_reason, quota_pressure_at_dispatch, dispatch_status, COALESCE(latency_ms, 0), created_at
	FROM job_routing_audit
	WHERE job_id = ?
	ORDER BY attempt_number ASC;`

	rows, err := s.db.Query(query, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []JobRoutingAudit
	for rows.Next() {
		var item JobRoutingAudit
		var createdAtStr string

		if err := rows.Scan(
			&item.JobID,
			&item.TaskCategory,
			&item.AttemptNumber,
			&item.AssignedModel,
			&item.AssignedEffort,
			&item.RouteReason,
			&item.QuotaPressureAtDispatch,
			&item.DispatchStatus,
			&item.LatencyMs,
			&createdAtStr,
		); err != nil {
			return nil, err
		}

		if t, err := parseTime(createdAtStr); err == nil {
			item.CreatedAt = t
		}
		records = append(records, item)
	}
	return records, rows.Err()
}

func parseTime(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, sql.ErrNoRows
}
