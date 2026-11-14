// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	_ "modernc.org/sqlite"
)

const readOnlyStateQuery = "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(3000)"

var ErrBusy = errors.New("state already has a scheduler")
var ErrConflict = errors.New("request ID already has different arguments")

type Store struct {
	db   *sql.DB
	lock *os.File
	Dir  string
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	actual, err := stateDirectory(dir)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(actual, "owner.lock"), syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "owner.lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	s := &Store{Dir: actual, lock: lock}
	if err := s.init(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func stateDirectory(dir string) (string, error) {
	actual, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(actual)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return "", fmt.Errorf("state directory must be private and owned by the current user")
	}
	return actual, nil
}

type JobEvidence struct {
	model.Job
	RunningAudited   bool
	CompletedAudited bool
}

// ReadEvidence reads a consistent snapshot of the existing jobs and audit log
// without acquiring scheduler ownership or initializing the state.
func ReadEvidence(ctx context.Context, dir string) (string, []JobEvidence, error) {
	actual, err := stateDirectory(dir)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(actual, "state.sqlite")
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return "", nil, errors.New("unsafe state database")
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: readOnlyStateQuery}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return "", nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
		SELECT jobs.body, COALESCE(jobs.request, ''), COUNT(CASE WHEN audit.status=? THEN 1 END)>0, COUNT(CASE WHEN audit.status=? THEN 1 END)>0
		FROM jobs LEFT JOIN audit ON audit.job=jobs.id GROUP BY jobs.id ORDER BY jobs.rowid DESC`, model.Running, model.Completed)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var jobs []JobEvidence
	for rows.Next() {
		var j JobEvidence
		var body []byte
		if err := rows.Scan(&body, &j.RequestID, &j.RunningAudited, &j.CompletedAudited); err != nil {
			return "", nil, err
		}
		if err := json.Unmarshal(body, &j.Job); err != nil {
			return "", nil, err
		}
		jobs = append(jobs, j)
	}
	return actual, jobs, rows.Err()
}

func (s *Store) init() error {
	dbPath := filepath.Join(s.Dir, "state.sqlite")
	f, err := syscall.Open(dbPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	syscall.Close(f)
	s.db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	s.db.SetMaxOpenConns(1)
	if _, err = s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 2 {
		return nil
	}
	if version > 2 {
		return fmt.Errorf("unsupported state schema %d", version)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if version == 0 {
		_, err = tx.Exec(`
		CREATE TABLE jobs (id TEXT PRIMARY KEY, repo TEXT NOT NULL, request TEXT, digest TEXT, body BLOB NOT NULL, UNIQUE(repo, request));
		CREATE TABLE audit (seq INTEGER PRIMARY KEY, job TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE, at TEXT NOT NULL, status TEXT NOT NULL, reason TEXT NOT NULL);`)
		if err != nil {
			return err
		}
	}

	if version < 2 {
		_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS provider_quota_state (
			provider_id TEXT PRIMARY KEY,
			tier_name TEXT NOT NULL,
			window_duration_hours REAL NOT NULL,
			requests_used_in_window INTEGER NOT NULL DEFAULT 0,
			max_window_requests INTEGER NOT NULL,
			health_pct REAL NOT NULL DEFAULT 1.0,
			circuit_state TEXT NOT NULL DEFAULT 'CLOSED',
			retry_after_utc TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS job_routing_audit (
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			task_category TEXT NOT NULL,
			attempt_number INTEGER NOT NULL DEFAULT 1,
			assigned_model TEXT NOT NULL,
			assigned_effort TEXT NOT NULL,
			route_reason TEXT NOT NULL,
			quota_pressure_at_dispatch REAL NOT NULL,
			dispatch_status TEXT NOT NULL,
			latency_ms INTEGER,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (job_id, attempt_number)
		);
		CREATE INDEX IF NOT EXISTS idx_routing_audit_created ON job_routing_audit(created_at);
		PRAGMA user_version=2;`)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Close() {
	if s.db != nil {
		s.db.Close()
	}
	s.lock.Close()
}

func (s *Store) Insert(jobs []model.Job, digest string) (string, error) {
	return s.InsertWithAdmission(jobs, digest, nil)
}

// InsertWithAdmission returns an existing matching request even when new
// submissions would fail the current admission check.
func (s *Store) InsertWithAdmission(jobs []model.Job, digest string, admissionErr error) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	first := jobs[0]
	var existing, previous string
	err = tx.QueryRow("SELECT id,digest FROM jobs WHERE repo=? AND request=?", first.RepoID, first.RequestID).Scan(&existing, &previous)
	if err == nil {
		if previous != digest {
			return "", ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if admissionErr != nil {
		return "", admissionErr
	}
	if err := insertJobs(tx, jobs, digest); err != nil {
		return "", err
	}
	return first.ID, tx.Commit()
}

func insertJobs(tx *sql.Tx, jobs []model.Job, digest string) error {
	for _, j := range jobs {
		b, err := json.Marshal(j)
		if err != nil {
			return err
		}
		var key any
		if j.RequestID != "" {
			key = j.RequestID
		}
		if _, err := tx.Exec("INSERT INTO jobs(id,repo,request,digest,body) VALUES(?,?,?,?,?)", j.ID, j.RepoID, key, digest, b); err != nil {
			return err
		}
		if err := audit(tx, j); err != nil {
			return err
		}
	}
	return nil
}

// Expand replaces one claimed planning job and inserts its DAG in one
// transaction. A crash cannot leave a partially visible batch.
func (s *Store) Expand(parent model.Job, children []model.Job) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := json.Marshal(parent)
	if err != nil {
		return err
	}
	r, err := tx.Exec(`UPDATE jobs SET body=? WHERE id=? AND repo=? AND json_extract(body,'$.status') IN (?,?) AND
		COALESCE(json_array_length(json_extract(body,'$.children')),0)=0`, b, parent.ID, parent.RepoID, model.Running, model.Verifying)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	if err := audit(tx, parent); err != nil {
		return err
	}
	if err := insertJobs(tx, children, ""); err != nil {
		return err
	}
	return tx.Commit()
}

func audit(tx *sql.Tx, j model.Job) error {
	_, err := tx.Exec("INSERT INTO audit(job,at,status,reason) VALUES(?,?,?,?)", j.ID, time.Now().UTC().Format(time.RFC3339Nano), j.Status, j.Reason)
	return err
}

func (s *Store) Save(jobs ...model.Job) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, j := range jobs {
		b, err := json.Marshal(j)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE jobs SET body=? WHERE id=?", b, j.ID); err != nil {
			return err
		}
		if err = audit(tx, j); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) All() ([]model.Job, error) {
	rows, err := s.db.Query("SELECT body FROM jobs ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		var b []byte
		var j model.Job
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM jobs WHERE id=?", id)
	return err
}
