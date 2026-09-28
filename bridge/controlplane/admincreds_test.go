// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
)

// secretVolume lays out a directory the way kubelet's AtomicWriter
// maintains a Secret volume: the files live in a timestamped directory,
// ..data points at it, and each key is a symlink through ..data. An update
// writes a new directory, swaps ..data with one rename, then removes the
// old directory.
type secretVolume struct {
	t   *testing.T
	dir string
	gen int
	cur string
}

func newSecretVolume(t *testing.T, username, password string) *secretVolume {
	t.Helper()
	v := &secretVolume{t: t, dir: t.TempDir()}
	v.update(username, password)
	for _, key := range []string{adminUsernameKey, adminPasswordKey} {
		if err := os.Symlink(filepath.Join(kubeletDataDir, key), filepath.Join(v.dir, key)); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

// update swaps in a new version; call it from the test goroutine only.
func (v *secretVolume) update(username, password string) {
	v.t.Helper()
	if err := v.swap(username, password); err != nil {
		v.t.Fatal(err)
	}
}

// swap writes a new version and swaps ..data to it. It reports errors
// instead of failing the test, so any goroutine may call it.
func (v *secretVolume) swap(username, password string) error {
	v.gen++
	gen := fmt.Sprintf("..2026_09_27_12_00_00.%09d", v.gen)
	if err := os.Mkdir(filepath.Join(v.dir, gen), 0o755); err != nil {
		return err
	}
	for key, value := range map[string]string{adminUsernameKey: username, adminPasswordKey: password} {
		if err := os.WriteFile(filepath.Join(v.dir, gen, key), []byte(value), 0o600); err != nil {
			return err
		}
	}
	tmp := filepath.Join(v.dir, "..data_tmp")
	if err := os.Symlink(gen, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(v.dir, kubeletDataDir)); err != nil {
		return err
	}
	if v.cur != "" {
		if err := os.RemoveAll(filepath.Join(v.dir, v.cur)); err != nil {
			return err
		}
	}
	v.cur = gen
	return nil
}

// TestAdminCredsReader_PicksUpRotatedSecret: after the operator rotates
// the Harbor admin password or system-robot secret and kubelet updates the
// mounted Secret, the next Harbor call carries the new credentials, with
// no restart. The change is logged without the values.
func TestAdminCredsReader_PicksUpRotatedSecret(t *testing.T) {
	vol := newSecretVolume(t, "robot$harbor-bridge", "old-secret")

	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	var logMu sync.Mutex
	var logged []string
	log := funcr.New(func(prefix, args string) {
		logMu.Lock()
		defer logMu.Unlock()
		logged = append(logged, prefix+" "+args)
	}, funcr.Options{})
	reader := NewAdminCredsReader(&Config{HarborAdminDir: vol.dir}, log)
	user, pass, err := reader.Read()
	if err != nil {
		t.Fatalf("startup read: %v", err)
	}
	client, err := harbor.NewClient(u, user, pass, srv.Client().Transport, harbor.WithCredentialSource(reader.Read))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	vol.update("robot$harbor-bridge", "new-secret")
	if _, err := client.List(context.Background()); err != nil {
		t.Fatalf("List after rotation: %v", err)
	}

	basic := func(u, p string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p)) }
	mu.Lock()
	got := append([]string(nil), auth...)
	mu.Unlock()
	want := []string{basic("robot$harbor-bridge", "old-secret"), basic("robot$harbor-bridge", "new-secret")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Authorization headers = %q, want %q", got, want)
	}

	logMu.Lock()
	defer logMu.Unlock()
	var changes int
	for _, line := range logged {
		if strings.Contains(line, "old-secret") || strings.Contains(line, "new-secret") {
			t.Errorf("log line carries a credential: %s", line)
		}
		if strings.Contains(line, "changed on disk") {
			changes++
		}
	}
	if changes != 1 {
		t.Errorf("logged %d credential changes, want 1: %q", changes, logged)
	}
}

// TestAdminCredsReader_FailsWhenTheFilesAreGone: a failed read fails the
// call with the path; it never reuses the previous credentials silently.
func TestAdminCredsReader_FailsWhenTheFilesAreGone(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "username"), "admin")
	mustWrite(t, filepath.Join(dir, "password"), "s3cret")
	reader := NewAdminCredsReader(&Config{HarborAdminDir: dir}, logr.Discard())
	if _, _, err := reader.Read(); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "password")); err != nil {
		t.Fatal(err)
	}
	user, pass, err := reader.Read()
	if err == nil {
		t.Fatalf("Read after the password file went away = %q, %q, want an error", user, pass)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "password")) || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error = %v, want the path and no credential", err)
	}
}

// TestLoadAdminCreds_NeverMixesVolumeVersions: reading the two key files
// one after the other can straddle a kubelet swap and pair the username of
// one version with the password of the next. Every read must return one
// version's pair while the volume is swapped continuously.
func TestLoadAdminCreds_NeverMixesVolumeVersions(t *testing.T) {
	// Enough swaps to catch the per-key read reliably; counted rather
	// than timed, so a single CPU only makes the test slower.
	const wantSwaps, timeout = 300, 60 * time.Second
	vol := newSecretVolume(t, "user-0", "pass-0")
	cfg := &Config{HarborAdminDir: vol.dir}

	var swaps atomic.Int64
	var swapErr error
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for n := 1; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			if swapErr = vol.swap(fmt.Sprintf("user-%d", n), fmt.Sprintf("pass-%d", n)); swapErr != nil {
				return
			}
			swaps.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()
	stopSwapper := sync.OnceValue(func() error {
		close(stop)
		<-done
		return swapErr
	})
	// Registered after newSecretVolume's TempDir, so it runs first.
	t.Cleanup(func() { _ = stopSwapper() })

	var reads, failures int
	var lastErr error
	deadline := time.Now().Add(timeout)
	for swaps.Load() < wantSwaps {
		select {
		case <-done:
			t.Fatalf("swap: %v", swapErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d swaps in %s; the test proves nothing", swaps.Load(), timeout)
		}
		reads++
		creds, err := cfg.LoadAdminCreds()
		if err != nil {
			failures++
			lastErr = err
			continue
		}
		if strings.TrimPrefix(creds.Username, "user-") != strings.TrimPrefix(creds.Password, "pass-") {
			t.Fatalf("read a mixed pair: %q with %q", creds.Username, creds.Password)
		}
	}
	if err := stopSwapper(); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if failures > 0 {
		t.Errorf("%d of %d reads failed during %d swaps, the last with %v; a swap mid-read must be retried", failures, reads, swaps.Load(), lastErr)
	}
}

// TestLoadAdminCreds_UnresolvableDataDirIsAnError: when ..data exists but
// cannot be resolved (on some filesystems readlink fails while a swap is
// in progress), the read must fail rather than fall back to the key files,
// which lead through ..data one after the other and could pair versions.
func TestLoadAdminCreds_UnresolvableDataDirIsAnError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, adminUsernameKey), "admin")
	mustWrite(t, filepath.Join(dir, adminPasswordKey), "s3cret")
	// A directory, so readlink fails with EINVAL, as it does mid-swap.
	if err := os.Mkdir(filepath.Join(dir, kubeletDataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	creds, err := (&Config{HarborAdminDir: dir}).LoadAdminCreds()
	if err == nil {
		t.Fatalf("LoadAdminCreds = %q, %q; want an error while ..data cannot be resolved", creds.Username, creds.Password)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, kubeletDataDir)) || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error = %v, want the ..data path and no credential", err)
	}
}
