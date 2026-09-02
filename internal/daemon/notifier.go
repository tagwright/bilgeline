// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tagwright/beacon"

	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/bilgeline/internal/discovery"
	"github.com/tagwright/bilgeline/internal/secret"
)

// buildNotifier constructs the beacon notifier the daemon reports through,
// mapping cfg's notification channels and telemetry sinks onto beacon's own
// config types. beacon's always-on "log" floor channel is ALWAYS wired, so a
// discovery or apply problem is never silently swallowed: even with no channel
// configured it still lands in the structured log stream.
//
// The two secret domains stay separate here (see internal/config and
// internal/secret). The resolver this builds serves DOMAIN 2 only: the alerting
// credentials the configured channels and sinks name (an ntfy token, a Gatus
// push token, and so on), resolved from bilgeline's OWN secrets dir. It never
// touches exporter/destination secrets (domain 1), which travel as literal
// ${env:VAR} on the collector and are never resolved in this process.
//
// If cfg configures no notification channel, the result is exactly the v1
// behavior (the log floor only), so this is backward compatible: an existing
// config with no notifications section behaves as before.
func buildNotifier(cfg *config.Config) (*beacon.Beacon, error) {
	// The log floor is always on, ahead of any configured channel, so a
	// diagnostic is reported to the logs whether or not a real channel is set.
	channels := make([]beacon.ChannelConfig, 0, len(cfg.Notifications)+1)
	channels = append(channels, beacon.ChannelConfig{Type: "log"})
	for i, c := range cfg.Notifications {
		level, err := parseLevel(c.MinLevel)
		if err != nil {
			return nil, fmt.Errorf("notification channel %d (%s): %w", i, c.Type, err)
		}
		channels = append(channels, beacon.ChannelConfig{
			Type:     c.Type,
			MinLevel: level,
			Settings: c.Settings,
		})
	}

	telemetry := make([]beacon.TelemetryConfig, 0, len(cfg.Telemetry))
	for _, t := range cfg.Telemetry {
		telemetry = append(telemetry, beacon.TelemetryConfig{
			Type:     t.Type,
			Settings: t.Settings,
		})
	}

	// Domain-2 resolver, built only from bilgeline's own alerting-secrets dir.
	// Handed to beacon so channels and sinks resolve their named credentials at
	// send time (credential rotation takes effect without a restart).
	resolver := secret.FileEnvResolver(cfg.SecretsDir)

	beaconCfg := beacon.Config{Channels: channels, Telemetry: telemetry}
	return beacon.New(beaconCfg, beacon.SecretResolver(resolver))
}

// parseLevel maps a config.ChannelConfig.MinLevel string onto a beacon.Level.
// An empty value means "receive everything" (LevelInfo). Config.Validate
// already rejects unknown levels, so the default arm is a belt-and-braces guard.
func parseLevel(s string) (beacon.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return beacon.LevelInfo, nil
	case "warn", "warning":
		return beacon.LevelWarning, nil
	case "error":
		return beacon.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown notification level %q", s)
	}
}

// notify sends one alert through notifier, tolerating a nil notifier (a no-op)
// and swallowing the send error: alerting is best-effort and the structured log
// is the durable record. Mirrors ballast's daemon.notify.
func notify(notifier *beacon.Beacon, level beacon.Level, title, body string) {
	if notifier == nil {
		return
	}
	_ = notifier.Notify(context.Background(), beacon.Notification{
		Title: title,
		Body:  body,
		Level: level,
	})
}

// report pushes one reconcile-outcome health result through notifier's
// telemetry sinks (e.g. a Gatus external endpoint), tolerating a nil notifier
// and swallowing the send error: telemetry is best-effort, exactly like notify.
// With no telemetry sink configured this is a no-op (beacon fans out to zero
// sinks). The push is event-driven, fired once per reconcile pass, mirroring
// how ballast reports per backup run rather than on a background clock.
func report(notifier *beacon.Beacon, ok bool, message string, dur time.Duration) {
	if notifier == nil {
		return
	}
	_ = notifier.Report(context.Background(), beacon.Health{
		Name:     "bilgeline",
		OK:       ok,
		Message:  message,
		Duration: dur,
	})
}

// routeDiagnostics logs every discovery diagnostic and alerts on it at the level
// its severity maps to. Warnings are logged and alerted at LevelWarning; errors
// (a container skipped for a validation fault) at LevelError. Logging is the
// floor: even with no channel configured beyond "log", the finding is recorded.
//
// It returns the number of error-severity diagnostics seen, so the reconcile
// pass can fold them into the health it reports to telemetry: a pass that
// skipped a container for a validation fault is degraded even when the backend
// apply itself succeeded.
func (r *reconciler) routeDiagnostics(diags []discovery.Diagnostic) int {
	errCount := 0
	for _, d := range diags {
		level := diagLevel(d.Severity)
		if d.Severity == discovery.SeverityError {
			errCount++
			r.logger.Error("discovery diagnostic",
				"container", d.ContainerName, "container_id", d.ContainerID, "message", d.Message)
		} else {
			r.logger.Warn("discovery diagnostic",
				"container", d.ContainerName, "container_id", d.ContainerID, "message", d.Message)
		}
		notify(r.notifier, level, "bilgeline: "+string(d.Severity)+" ("+d.ContainerName+")", d.Message)
	}
	return errCount
}
