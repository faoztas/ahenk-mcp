// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRoutingPreferenceAtomicAndScoped(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	repo, otherRoot := t.TempDir(), t.TempDir()
	const id = "demo"
	assertRoutingPreference(t, state, id, repo, "")
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read initialized state: %v", err)
	}
	if err := SaveRoutingPreference(state, id, repo, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset initialized state: %v", err)
	}
	quality := "quality"
	if err := SaveRoutingPreference(state, id, repo, &quality); err != nil {
		t.Fatal(err)
	}
	assertRoutingPreference(t, state, id, repo, quality)
	assertRoutingPreference(t, state, "other", repo, "")
	assertRoutingPreference(t, state, id, otherRoot, "")
	alias := filepath.Join(base, "repo-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	assertRoutingPreference(t, state, id, alias, quality)
	price := "price"
	if err := SaveRoutingPreference(state, id, otherRoot, &price); err != nil {
		t.Fatal(err)
	}
	if err := SaveRoutingPreference(state, "other", repo, &price); err != nil {
		t.Fatal(err)
	}
	assertRoutingPreference(t, state, id, repo, quality)

	invalid := "Quality"
	if err := SaveRoutingPreference(state, id, repo, &invalid); !errors.Is(err, ErrInvalidRoutingPreference) {
		t.Fatalf("accepted an invalid profile: %v", err)
	}
	assertRoutingPreference(t, state, id, repo, quality)
	if _, err := ReadRoutingPreference(state, "../escape", repo); !errors.Is(err, ErrInvalidRoutingPreference) {
		t.Fatalf("accepted an invalid repository ID: %v", err)
	}
	if _, err := ReadRoutingPreference(state, id, "relative-root"); !errors.Is(err, ErrInvalidRoutingPreference) {
		t.Fatalf("accepted a relative repository root: %v", err)
	}

	db, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	database := filepath.Join(state, "state.sqlite")
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}

	start, writersDone := make(chan struct{}), make(chan struct{})
	results := make(chan error, 3)
	var ready, writers sync.WaitGroup
	ready.Add(3)
	writers.Add(2)
	write := func(profile string, reset bool) {
		defer writers.Done()
		ready.Done()
		<-start
		for i := range 64 {
			value := &profile
			if reset && i%2 == 0 {
				value = nil
			}
			if err := SaveRoutingPreference(state, id, repo, value); err != nil {
				results <- fmt.Errorf("write %s: %w", profile, err)
				return
			}
		}
		results <- nil
	}
	go write("price_performance", false)
	go write("price", true)
	go func() {
		ready.Done()
		<-start
		for {
			profile, err := ReadRoutingPreference(state, id, repo)
			if err != nil || (profile != "" && profile != "price_performance" && profile != "quality" && profile != "price") {
				results <- fmt.Errorf("reader observed %q, error %v", profile, err)
				return
			}
			select {
			case <-writersDone:
				results <- nil
				return
			default:
			}
		}
	}()
	ready.Wait()
	close(start)
	writers.Wait()
	close(writersDone)
	for range 3 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if err := SaveRoutingPreference(state, id, repo, nil); err != nil {
		t.Fatal(err)
	}
	assertRoutingPreference(t, state, id, repo, "")
	assertRoutingPreference(t, state, "other", repo, price)
	assertRoutingPreference(t, state, id, otherRoot, price)
	after, err := os.ReadFile(database)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("preference operations changed SQLite bytes: %v", err)
	}
}

func TestRoutingPreferenceRejectsUnsafeState(t *testing.T) {
	const id = "demo"
	repo := t.TempDir()
	quality := "quality"
	pathFor := func(t *testing.T, state string) string {
		t.Helper()
		if err := SaveRoutingPreference(state, id, repo, &quality); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(state)
		if err != nil || len(entries) != 1 {
			t.Fatalf("expected one preference file, got %d: %v", len(entries), err)
		}
		path := filepath.Join(state, entries[0].Name())
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("preference file has unsafe mode %v", info.Mode())
		}
		return path
	}
	assertInvalid := func(t *testing.T, state string) {
		t.Helper()
		if got, err := ReadRoutingPreference(state, id, repo); got != "" || !errors.Is(err, ErrInvalidRoutingPreference) {
			t.Fatalf("read unsafe preference: %q, %v", got, err)
		}
	}
	fixtures := map[string]func(*testing.T, string){
		"permissions": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
		},
		"owner": func(t *testing.T, path string) {
			if os.Geteuid() != 0 {
				t.Skip("changing the fixture owner requires root")
			}
			if err := os.Chown(path, 1, -1); err != nil {
				t.Fatal(err)
			}
		},
		"hard link": func(t *testing.T, path string) {
			if err := os.Link(path, filepath.Join(t.TempDir(), "target")); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"public directory": func(t *testing.T, path string) {
			if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range fixtures {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			change(t, pathFor(t, state))
			assertInvalid(t, state)
			for _, value := range []*string{&quality, nil} {
				if err := SaveRoutingPreference(state, id, repo, value); !errors.Is(err, ErrInvalidRoutingPreference) {
					t.Fatalf("modified unsafe preference: %v", err)
				}
			}
		})
	}
	t.Run("state directory symlink", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state")
		pathFor(t, state)
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(state, alias); err != nil {
			t.Fatal(err)
		}
		assertInvalid(t, alias)
	})
	t.Run("malformed contents", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state")
		path := pathFor(t, state)
		for _, contents := range []string{"", "price", "quality\nprice\n", "unsupported\n", "price_performance\nextra content"} {
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			assertInvalid(t, state)
		}
		if err := SaveRoutingPreference(state, id, repo, &quality); err != nil {
			t.Fatalf("could not replace malformed preference: %v", err)
		}
		assertRoutingPreference(t, state, id, repo, quality)
	})
}

func TestRoutingPreferenceRejectsParentTraversal(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	repo := t.TempDir()
	quality := "quality"
	if err := SaveRoutingPreference(state, "demo", repo, &quality); err != nil {
		t.Fatal(err)
	}
	// Construct the raw argument without letting filepath.Join clean it first.
	separator := string(os.PathSeparator)
	malformed := filepath.Join(base, "missing") + separator + ".." + separator + "state"
	if got, err := ReadRoutingPreference(malformed, "demo", repo); got != "" || !errors.Is(err, ErrInvalidRoutingPreference) {
		t.Errorf("read accepted a path traversing a missing directory: %q, %v", got, err)
	}
	if err := SaveRoutingPreference(malformed, "demo", repo, nil); !errors.Is(err, ErrInvalidRoutingPreference) {
		t.Errorf("reset accepted a path traversing a missing directory: %v", err)
	}
	assertRoutingPreference(t, state, "demo", repo, quality)
}

func TestRoutingPreferenceDoesNotCreateStateThroughRepositoryAlias(t *testing.T) {
	for _, spelling := range []string{"repo", "REPO"} {
		t.Run(spelling, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(base, spelling)
			if _, err := os.Stat(target); spelling != "repo" && errors.Is(err, os.ErrNotExist) {
				t.Skip("filesystem treats alternate case as a different path")
			}
			alias := filepath.Join(base, "alias")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			quality := "quality"
			state := filepath.Join(alias, "missing", "state")
			if _, err := ReadRoutingPreference(state, "demo", repo); !errors.Is(err, ErrInvalidRoutingPreference) {
				t.Errorf("read accepted a state directory inside the repository: %v", err)
			}
			if err := SaveRoutingPreference(state, "demo", repo, nil); !errors.Is(err, ErrInvalidRoutingPreference) {
				t.Errorf("reset accepted a state directory inside the repository: %v", err)
			}
			if err := SaveRoutingPreference(state, "demo", repo, &quality); !errors.Is(err, ErrInvalidRoutingPreference) {
				t.Errorf("set accepted a state directory inside the repository: %v", err)
			}
			entries, err := os.ReadDir(repo)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected preference left entries in the repository: %v, %v", entries, err)
			}
		})
	}
}

func assertRoutingPreference(t *testing.T, state, id, repo, want string) {
	t.Helper()
	if got, err := ReadRoutingPreference(state, id, repo); got != want || err != nil {
		t.Fatalf("read preference for %q: got %q, want %q, error %v", id, got, want, err)
	}
}
