// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tagwright/beacon"

	"github.com/tagwright/bilgeline/internal/config"
)

// TestBuildNotifierLogFloorOnly pins the backward-compatible default: a config
// with no notifications and no telemetry still builds a working notifier (the
// always-on log floor), never nil, never an error.
func TestBuildNotifierLogFloorOnly(t *testing.T) {
	n, err := buildNotifier(&config.Config{})
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if n == nil {
		t.Fatal("buildNotifier returned nil notifier")
	}
	// The log floor accepts and swallows a notify with no error.
	if err := n.Notify(context.Background(), beacon.Notification{
		Title: "t", Body: "b", Level: beacon.LevelError,
	}); err != nil {
		t.Errorf("Notify through log floor: %v", err)
	}
}

// TestBuildNotifierUnknownType proves the configured channels are actually
// handed to beacon: an unknown backend type reaches beacon.New and fails the
// build (config.Validate is the first line of defence, but buildNotifier must
// not silently drop channels).
func TestBuildNotifierUnknownType(t *testing.T) {
	_, err := buildNotifier(&config.Config{
		Notifications: []config.ChannelConfig{{Type: "carrierpigeon"}},
	})
	if err == nil {
		t.Fatal("buildNotifier: want error for unknown channel type, got nil")
	}
}

// TestBuildNotifierBadLevel pins that an unparseable min_level fails the build
// through parseLevel.
func TestBuildNotifierBadLevel(t *testing.T) {
	_, err := buildNotifier(&config.Config{
		Notifications: []config.ChannelConfig{{Type: "ntfy", MinLevel: "screaming", Settings: map[string]string{"topic": "x"}}},
	})
	if err == nil {
		t.Fatal("buildNotifier: want error for bad min_level, got nil")
	}
}

// TestBuildNotifierDeliversToNtfy is the end-to-end proof of the wiring the
// feature adds: config -> secret resolver -> beacon -> ntfy backend -> the wire.
// It stands up a fake ntfy (an httptest server), configures an ntfy channel
// pointing at it with a named token secret resolved from a secrets dir, builds
// the notifier through buildNotifier exactly as the daemon does, fires a
// notification, and asserts the request arrived with the resolved bearer token.
// This exercises the same code path a live ntfy container would, without one.
func TestBuildNotifierDeliversToNtfy(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	got := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody = string(buf)
		got = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The token is resolved from bilgeline's OWN secrets dir (domain 2), by
	// name, at send time, never a literal in the config.
	dir := t.TempDir()
	if err := writeFile(dir, "ntfy-itest-token", "tok-abc123"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	cfg := &config.Config{
		SecretsDir: dir,
		Notifications: []config.ChannelConfig{{
			Type:     "ntfy",
			MinLevel: "warn",
			Settings: map[string]string{
				"server":       srv.URL,
				"topic":        "bilgeline-itest",
				"token_secret": "ntfy-itest-token",
			},
		}},
	}

	n, err := buildNotifier(cfg)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}

	if err := n.Notify(context.Background(), beacon.Notification{
		Title: "bilgeline: error (svc)", Body: "unknown destination", Level: beacon.LevelError,
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if !got {
		t.Fatal("fake ntfy received no request")
	}
	if gotPath != "/bilgeline-itest" {
		t.Errorf("ntfy path = %q, want /bilgeline-itest", gotPath)
	}
	if gotAuth != "Bearer tok-abc123" {
		t.Errorf("Authorization = %q, want the resolved bearer token", gotAuth)
	}
	if gotBody != "unknown destination" {
		t.Errorf("ntfy body = %q, want the notification body", gotBody)
	}
}

// TestBuildNotifierMinLevelFilters pins that a channel's min_level filters: a
// warn-floor channel does not receive an info notification. Proven through the
// same fake-ntfy path.
func TestBuildNotifierMinLevelFilters(t *testing.T) {
	got := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Notifications: []config.ChannelConfig{{
			Type:     "ntfy",
			MinLevel: "warn",
			Settings: map[string]string{"server": srv.URL, "topic": "x"},
		}},
	}
	n, err := buildNotifier(cfg)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if err := n.Notify(context.Background(), beacon.Notification{Title: "i", Level: beacon.LevelInfo}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got {
		t.Error("info notification reached a warn-floor channel; min_level not applied")
	}
}

// TestBuildNotifierGatusTelemetry pins that a configured gatus telemetry sink
// builds and receives a reconcile-outcome health push, with the resolved token.
func TestBuildNotifierGatusTelemetry(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	got := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		got = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := writeFile(dir, "gatus-push", "push-xyz"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	cfg := &config.Config{
		SecretsDir: dir,
		Telemetry: []config.TelemetryConfig{{
			Type: "gatus",
			Settings: map[string]string{
				"url":          srv.URL,
				"endpoint_key": "infra_bilgeline",
				"token_secret": "gatus-push",
			},
		}},
	}
	n, err := buildNotifier(cfg)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	// Mirror the daemon's degraded-reconcile push.
	report(n, false, "apply failed: collector wedged", 0)

	if !got {
		t.Fatal("fake gatus received no push")
	}
	if gotPath != "/api/v1/endpoints/infra_bilgeline/external" {
		t.Errorf("gatus path = %q, want the external-endpoint push path", gotPath)
	}
	if gotAuth != "Bearer push-xyz" {
		t.Errorf("Authorization = %q, want the resolved push token", gotAuth)
	}
	// A degraded push carries success=false and the error message.
	if want := "success=false"; !strings.Contains(gotQuery, want) {
		t.Errorf("gatus query %q missing %q", gotQuery, want)
	}
}

// TestBuildNotifierLiveNtfy is the live end-to-end proof against a REAL ntfy
// server, mirroring how ballast proved ntfy delivery live. It is skipped unless
// BILGELINE_ITEST_NTFY_URL points at a reachable ntfy instance (the live
// integration harness sets it to a throwaway bilgeline-itest-ntfy container).
// It builds the notifier through buildNotifier exactly as the daemon does,
// fires an error-level notification at a unique topic, then polls ntfy's JSON
// API and asserts the message landed. This closes the config -> resolver ->
// beacon -> ntfy path against a real server, not a stub.
func TestBuildNotifierLiveNtfy(t *testing.T) {
	base := os.Getenv("BILGELINE_ITEST_NTFY_URL")
	if base == "" {
		t.Skip("BILGELINE_ITEST_NTFY_URL not set; skipping live ntfy integration")
	}
	base = strings.TrimRight(base, "/")
	topic := "bilgeline-itest-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	body := "live delivery proof " + topic

	cfg := &config.Config{
		Notifications: []config.ChannelConfig{{
			Type:     "ntfy",
			MinLevel: "warn",
			Settings: map[string]string{"server": base, "topic": topic},
		}},
	}
	n, err := buildNotifier(cfg)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if err := n.Notify(context.Background(), beacon.Notification{
		Title: "bilgeline: error (itest)", Body: body, Level: beacon.LevelError,
	}); err != nil {
		t.Fatalf("Notify to live ntfy: %v", err)
	}

	// Poll the topic's cached messages and assert ours arrived.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/" + topic + "/json?poll=1")
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(data), body) {
				return // delivered
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("message %q did not arrive at live ntfy topic %q within the deadline", body, topic)
}

// writeFile is a tiny test helper: write value into dir/name as a secret file.
func writeFile(dir, name, value string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600)
}
