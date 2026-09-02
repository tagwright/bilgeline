// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package secret resolves the named-secret references bilgeline's own
// notification and telemetry channels carry (an ntfy token, a Telegram bot
// token, an SMTP password, a Gatus push token). It is bilgeline's SECRET
// DOMAIN 2 and only that: the alerting credentials the beacon notifier, which
// runs inside bilgeline's process, needs at send time.
//
// It is deliberately NOT the path for exporter/destination secrets (domain 1,
// e.g. a Loki bearer token). Those belong to the COLLECTOR's process, travel as
// literal ${env:VAR} references copied verbatim into the generated collector
// config, and are never resolved here. Keeping the two domains in separate code
// paths is intentional: a Loki token must never be readable from bilgeline's
// process, and an ntfy token must never leak into a collector config file.
//
// Nothing in this package holds a secret value longer than it has to, and no
// secret value is ever logged.
package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultSecretsDir is bilgeline's default alerting-secrets directory, used
// when neither the config's secrets_dir nor BILGELINE_SECRETS_DIR is set. It
// mirrors ballast's /run/ballast/secrets convention: a tmpfs mount the operator
// drops the alerting credential files into (e.g. via SOPS at deploy time).
const DefaultSecretsDir = "/run/bilgeline/secrets"

// Resolver resolves a named secret to its value. Its signature intentionally
// matches beacon.SecretResolver so BeaconResolver can hand it straight to the
// notifier: channels and sinks name secrets, they never contain them.
type Resolver func(name string) (string, error)

// FileEnvResolver returns a Resolver that looks up name first as a file under
// secretsDir, then as an environment variable.
//
// Resolution order:
//  1. File filepath.Join(secretsDir, name).
//  2. Env var BILGELINE_SECRET_<NAME>, where NAME is name uppercased with "-"
//     replaced by "_".
//  3. Neither found: an error naming the secret, so a channel's send fails
//     loudly (and is logged) rather than sending with an empty credential.
//
// Whichever source a value comes from, it is trimmed the same way: leading and
// trailing whitespace (spaces, tabs, CR, LF) is stripped before it is returned.
// This keeps a secret's resolved value identical regardless of source, so a
// token pasted into an env var with a stray trailing newline behaves exactly
// like one read from a file. Ballast learned this the hard way: an untrimmed
// trailing newline silently corrupts a bearer token and every push 401s.
//
// secretsDir defaults to DefaultSecretsDir when empty.
func FileEnvResolver(secretsDir string) Resolver {
	if secretsDir == "" {
		secretsDir = DefaultSecretsDir
	}

	return func(name string) (string, error) {
		if name == "" {
			return "", fmt.Errorf("secret: empty secret name")
		}

		path := filepath.Join(secretsDir, name)
		if data, err := os.ReadFile(path); err == nil {
			return trimSecret(string(data)), nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("secret: read %s: %w", path, err)
		}

		envName := envVarName(name)
		if v, ok := os.LookupEnv(envName); ok {
			return trimSecret(v), nil
		}

		return "", fmt.Errorf("secret: %q not found in %s or %s", name, path, envName)
	}
}

// trimSecret strips leading and trailing whitespace (spaces, tabs, CR, LF) from
// a resolved secret value. It is applied uniformly to every source a Resolver
// can pull from, so the resolved value never depends on which source supplied
// it.
func trimSecret(v string) string {
	return strings.Trim(v, "\r\n \t")
}

// envVarName maps a secret name to the BILGELINE_SECRET_<NAME> env var the
// resolver falls back to when no secrets-directory file exists for it.
func envVarName(name string) string {
	upper := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return "BILGELINE_SECRET_" + upper
}
