// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

//go:build integration

// The live ntfy end-to-end proof lives behind the integration build tag, not in
// the always-run leg: it needs a real ntfy server (BILGELINE_ITEST_NTFY_URL),
// an absent resource in plain CI. A resource-absent skip belongs in the
// integration leg (where the live harness / operator supplies the resource),
// where it is counted against the skip budget, never in `go test ./...`, whose
// budget is 0 because that leg has no absent resource to justify a skip.
// (tagwright Testing Standard enforceability layer, task #552.)

package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tagwright/courier"

	"github.com/tagwright/bilgeline/internal/config"
)

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
	if err := n.Notify(context.Background(), courier.Notification{
		Title: "bilgeline: error (itest)", Body: body, Level: courier.LevelError,
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
