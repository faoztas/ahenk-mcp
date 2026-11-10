// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	openCodeCooldownFile = "opencode-cooldown-imports.json"
	openCodeImportLimit  = 128 << 10
)

// OpenCodeCooldownImport records a historical gate separately from job history.
type OpenCodeCooldownImport struct {
	UpstreamStatus         int       `json:"upstream_status,omitempty"`
	RetrySource            string    `json:"retry_source,omitempty"`
	Endpoint               string    `json:"endpoint,omitempty"`
	Recheck                bool      `json:"recheck,omitempty"`
	SourceDir              string    `json:"source_state_dir"`
	JobID                  string    `json:"source_job_id"`
	Attempt                int       `json:"source_attempt"`
	Model                  string    `json:"model"`
	OpenCodeAccount        string    `json:"opencode_account,omitempty"`
	OpenCodeTransport      string    `json:"opencode_transport,omitempty"`
	RecordedScope          string    `json:"recorded_scope,omitempty"`
	RetryAt                time.Time `json:"retry_at,omitzero"`
	Fingerprint            string    `json:"fingerprint"`
	FinishedAt             time.Time `json:"finished_at"`
	RetryAfterMS           int64     `json:"retry_after_ms"`
	ProviderRateLimitScope string    `json:"provider_rate_limit_scope,omitempty"`
	ImportedAt             time.Time `json:"imported_at"`
}

// ReadOpenCodeRateLimits projects only cooldown fields from an idle source DB.
// The source scheduler lock is opened read-only, and immutable mode avoids WAL
// housekeeping writes by SQLite.
func ReadOpenCodeRateLimits(ctx context.Context, dir string) ([]OpenCodeCooldownImport, error) {
	actual, err := stateDirectory(dir)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(actual, "owner.lock"), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return nil, err
	}
	path := filepath.Join(actual, "state.sqlite")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return nil, errors.New("unsafe source state database")
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
		SELECT jobs.id, json_extract(a.value,'$.number'), json_extract(a.value,'$.model'),
		       json_extract(a.value,'$.fingerprint'), json_extract(a.value,'$.finished_at'),
		       COALESCE(json_extract(a.value,'$.retry_after_ms'), 0),
		       COALESCE(json_extract(a.value,'$.provider_rate_limit_scope'), ''),
		       COALESCE(json_extract(a.value,'$.opencode_account'), ''),
		       COALESCE(json_extract(a.value,'$.opencode_transport'), ''),
		       COALESCE(json_extract(a.value,'$.health_scope'), ''),
		       COALESCE(json_extract(a.value,'$.retry_at'), ''),
		       COALESCE(json_extract(a.value,'$.upstream_status'),0),
		       COALESCE(json_extract(a.value,'$.retry_source'),''),
		       COALESCE(json_extract(a.value,'$.opencode_endpoint'),''),
		       COALESCE(json_extract(a.value,'$.opencode_recheck'),0)
		FROM jobs, json_each(jobs.body,'$.attempts') AS a
		WHERE json_extract(a.value,'$.provider')='opencode'
		  AND json_extract(a.value,'$.code')='RATE_LIMIT'
		  AND json_extract(a.value,'$.finished_at') IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []OpenCodeCooldownImport
	for rows.Next() {
		record := OpenCodeCooldownImport{SourceDir: actual}
		var finished, retryAt string
		if err := rows.Scan(&record.JobID, &record.Attempt, &record.Model, &record.Fingerprint, &finished, &record.RetryAfterMS,
			&record.ProviderRateLimitScope, &record.OpenCodeAccount, &record.OpenCodeTransport, &record.RecordedScope, &retryAt, &record.UpstreamStatus, &record.RetrySource, &record.Endpoint, &record.Recheck); err != nil {
			return nil, err
		}
		record.FinishedAt, err = time.Parse(time.RFC3339Nano, finished)
		if err != nil {
			return nil, errors.New("invalid historical rate-limit time")
		}
		record.FinishedAt = record.FinishedAt.UTC()
		if retryAt != "" {
			record.RetryAt, err = time.Parse(time.RFC3339Nano, retryAt)
			if err != nil {
				return nil, errors.New("invalid historical retry time")
			}
			record.RetryAt = record.RetryAt.UTC()
		}
		if !validOpenCodeImport(record) {
			return nil, errors.New("invalid historical rate-limit record")
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func validOpenCodeImport(record OpenCodeCooldownImport) bool {
	return model.ValidID(record.JobID) && record.Attempt >= 1 && !record.FinishedAt.IsZero() && record.RetryAfterMS >= 0 && (record.Endpoint == "" || record.Endpoint == model.OpenCodeFreeEndpoint) &&
		(record.RetryAt.IsZero() || !record.RetryAt.Before(record.FinishedAt)) &&
		(model.OpenCodeTransportID(record.OpenCodeTransport) == model.OpenCodeStateless || record.OpenCodeTransport == model.OpenCodeNativeReadAsk) &&
		(record.OpenCodeAccount == "" || model.ValidID(record.OpenCodeAccount) && !model.ContainsSecret([]byte(record.OpenCodeAccount)))
}

func ReadOpenCodeCooldownImports(dir string) ([]OpenCodeCooldownImport, error) {
	path := filepath.Join(dir, openCodeCooldownFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > openCodeImportLimit || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return nil, errors.New("unsafe OpenCode cooldown import file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, openCodeImportLimit+1))
	d.DisallowUnknownFields()
	var records []OpenCodeCooldownImport
	if err := d.Decode(&records); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("unexpected OpenCode cooldown import data")
	}
	for _, record := range records {
		if !filepath.IsAbs(record.SourceDir) || !validOpenCodeImport(record) ||
			record.ImportedAt.IsZero() || record.FinishedAt.After(record.ImportedAt) {
			return nil, errors.New("invalid OpenCode cooldown import")
		}
	}
	return records, nil
}

// SaveOpenCodeCooldownImports is called while the destination scheduler lock
// is held. Re-importing a source attempt preserves its original provenance.
func (s *Store) SaveOpenCodeCooldownImports(records []OpenCodeCooldownImport, now time.Time) ([]OpenCodeCooldownImport, error) {
	existing, err := ReadOpenCodeCooldownImports(s.Dir)
	if err != nil {
		return nil, err
	}
	changed := false
	for _, record := range records {
		if record.SourceDir == s.Dir {
			return nil, errors.New("cannot import from the destination state")
		}
		index := slices.IndexFunc(existing, func(old OpenCodeCooldownImport) bool {
			return old.SourceDir == record.SourceDir && old.JobID == record.JobID && old.Attempt == record.Attempt
		})
		if index >= 0 {
			old := existing[index]
			merged := old
			record.ImportedAt = old.ImportedAt
			// A verified re-import can fill previously omitted source fields.
			// Values already recorded and the original import time are preserved.
			if merged.UpstreamStatus == 0 {
				merged.UpstreamStatus = record.UpstreamStatus
			}
			if merged.RetrySource == "" {
				merged.RetrySource = record.RetrySource
			}
			if merged.Endpoint == "" {
				merged.Endpoint = record.Endpoint
			}
			if !merged.Recheck {
				merged.Recheck = record.Recheck
			}
			if merged.RetryAt.IsZero() {
				merged.RetryAt = record.RetryAt
			}
			if merged.RecordedScope == "" {
				merged.RecordedScope = record.RecordedScope
			}
			if merged.OpenCodeAccount == "" && model.OpenCodeAccountID(record.OpenCodeAccount) != model.OpenCodeAnonymous {
				merged.OpenCodeAccount = record.OpenCodeAccount
			}
			if model.OpenCodeAccountID(record.OpenCodeAccount) == model.OpenCodeAccountID(merged.OpenCodeAccount) {
				record.OpenCodeAccount = merged.OpenCodeAccount
			}
			if model.OpenCodeTransportID(record.OpenCodeTransport) == model.OpenCodeTransportID(old.OpenCodeTransport) {
				record.OpenCodeTransport = old.OpenCodeTransport
			}
			if record != merged {
				return nil, errors.New("historical rate-limit record conflicts with the existing import")
			}
			if merged != old {
				existing[index], changed = merged, true
			}
			continue
		}
		record.ImportedAt = now
		existing = append(existing, record)
		changed = true
	}
	if changed {
		data, err := json.Marshal(existing)
		if err != nil || len(data) > openCodeImportLimit {
			return nil, errors.New("OpenCode cooldown imports exceeded storage limit")
		}
		if err := execution.Save(s.Dir, openCodeCooldownFile, data); err != nil {
			return nil, err
		}
	}
	return existing, nil
}
