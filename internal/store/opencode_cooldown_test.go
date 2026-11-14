// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestReadOpenCodeRateLimitsIncludesCommittedWAL(t *testing.T) {
	const fixture = "AHENK_COOLDOWN_CRASH_FIXTURE"
	if dir := os.Getenv(fixture); dir != "" {
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec("PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		job := model.Job{ID: "wal-fixture", Status: model.Blocked,
			Attempts: []model.Attempt{{Number: 1, Provider: "opencode", Model: "opencode/fixture-free",
				Code: "RATE_LIMIT", Fingerprint: strings.Repeat("a", 64), FinishedAt: &now, RetryAfterMS: 30000}}}
		if _, err := db.Insert([]model.Job{job}, ""); err != nil {
			t.Fatal(err)
		}
		// Exit before SQLite checkpoints or closes its WAL.
		os.Exit(0)
	}
	dir := filepath.Join(t.TempDir(), "state")
	cmd := exec.Command(os.Args[0], "-test.run=^TestReadOpenCodeRateLimitsIncludesCommittedWAL$")
	cmd.Env = []string{fixture + "=" + dir}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare WAL-only cooldown: %v %s", err, output)
	}
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	base, wal := read("state.sqlite"), read("state.sqlite-wal")
	if len(wal) == 0 {
		t.Fatal("missing committed WAL fixture")
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(dir, "state.sqlite"), RawQuery: "mode=ro&immutable=1"}
	immutable, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	queryErr := immutable.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&count)
	if err := immutable.Close(); err != nil || queryErr != nil || count != 0 {
		t.Fatalf("fixture unexpectedly available without WAL: %d, %v, %v", count, queryErr, err)
	}
	records, err := ReadOpenCodeRateLimits(t.Context(), dir)
	if err != nil || len(records) != 1 || records[0].JobID != "wal-fixture" || records[0].RetryAfterMS != 30000 {
		t.Fatalf("WAL-only cooldown was not read: %+v %v", records, err)
	}
	if !bytes.Equal(base, read("state.sqlite")) || !bytes.Equal(wal, read("state.sqlite-wal")) {
		t.Fatal("cooldown import modified the source database or WAL")
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := ReadOpenCodeRateLimits(t.Context(), dir); err != syscall.EWOULDBLOCK {
		t.Fatalf("import did not respect scheduler ownership: %v", err)
	}
}

func TestReadOpenCodeRateLimitsCarriesImportedHistory(t *testing.T) {
	parent := t.TempDir()
	source, err := Open(filepath.Join(parent, "source"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []OpenCodeCooldownImport{
		{SourceDir: filepath.Join(parent, "original"), JobID: "native", Attempt: 1, Model: "opencode/glm-5.3-flash-free",
			OpenCodeTransport: model.OpenCodeNativeReadAsk, FinishedAt: now, RetryAfterMS: 90000},
		{SourceDir: filepath.Join(parent, "original"), JobID: "stateless", Attempt: 1, Model: "opencode/beam-free",
			FinishedAt: now, RetryAfterMS: 60000},
	}
	imported, err := source.SaveOpenCodeCooldownImports(rows, now)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	history, err := ReadOpenCodeRateLimits(t.Context(), source.Dir)
	if err != nil || !slices.Equal(history, imported) {
		t.Fatalf("carried history mismatch: %v", err)
	}
	dest, err := Open(filepath.Join(parent, "dest"))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	reimported, err := dest.SaveOpenCodeCooldownImports(history, now.Add(time.Minute))
	if err != nil || !slices.Equal(reimported, imported) {
		t.Fatalf("source provenance changed on second import: %v", err)
	}
}
