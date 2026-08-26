// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"

	"github.com/tagwright/beacon"

	"github.com/tagwright/bilgeline/internal/discovery"
)

// buildNotifier constructs the beacon notifier the daemon reports through. In
// v1 it wires only beacon's always-on "log" floor channel, so a discovery or
// apply problem is never silently swallowed: it lands in the structured log
// stream (and, ultimately, a nonzero process exit is the last-resort floor).
//
// TODO fast-follow: full beacon channel + telemetry config. A notifications /
// telemetry section in bilgeline.yml (ntfy, Telegram, a Gatus push, and so on)
// is a deliberate follow-up. It slots in exactly here: read cfg's channel and
// telemetry entries, map them onto beacon.ChannelConfig / beacon.TelemetryConfig
// the way ballast's BuildNotifier does, append the "log" floor only when nothing
// else is configured, and pass a real beacon.SecretResolver instead of the nil
// below. Nothing outside this function needs to change: the daemon already
// alerts through the returned *beacon.Beacon.
//
// The "log" channel resolves no secrets, so a nil SecretResolver is correct
// here; beacon replaces nil with a resolver that errors on every lookup, which
// the log channel never triggers.
func buildNotifier() (*beacon.Beacon, error) {
	cfg := beacon.Config{
		Channels: []beacon.ChannelConfig{{Type: "log"}},
	}
	return beacon.New(cfg, nil)
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

// routeDiagnostics logs every discovery diagnostic and alerts on it at the level
// its severity maps to. Warnings are logged and alerted at LevelWarning; errors
// (a container skipped for a validation fault) at LevelError. Logging is the
// floor: even with no channel configured beyond "log", the finding is recorded.
func (r *reconciler) routeDiagnostics(diags []discovery.Diagnostic) {
	for _, d := range diags {
		level := diagLevel(d.Severity)
		if d.Severity == discovery.SeverityError {
			r.logger.Error("discovery diagnostic",
				"container", d.ContainerName, "container_id", d.ContainerID, "message", d.Message)
		} else {
			r.logger.Warn("discovery diagnostic",
				"container", d.ContainerName, "container_id", d.ContainerID, "message", d.Message)
		}
		notify(r.notifier, level, "bilgeline: "+string(d.Severity)+" ("+d.ContainerName+")", d.Message)
	}
}
