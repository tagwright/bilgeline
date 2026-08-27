# Deploying bilgeline

bilgeline runs as two containers: bilgeline itself, and an OpenTelemetry
Collector you own. This guide covers the split, the turnkey compose stack, the
secret model, first-boot ordering, and how to check it worked.

## The two-container model

bilgeline does not ship or run a collector. It is a single Go binary that reads
`bilgeline.*` labels off your running containers, generates an
`otelcol-contrib` config, and drives the collector to reload it. The collector
does the actual log tailing and shipping.

The split matters for what each container mounts and why.

bilgeline mounts:

- the container socket (`/var/run/docker.sock`), read-write. It reads it to
  discover containers, and it writes to it to signal the collector: a SIGHUP to
  reload after a config change, and a container restart if a reload wedges.
  Read-only is not enough for the signalling.
- the shared config volume, read-write. This is where it writes the generated
  collector YAML.
- its own `bilgeline.yml`, read-only. Optional: bilgeline runs env-only with no
  file. This is where you name your destinations.

The collector mounts:

- the shared config volume, read-only. It only reads what bilgeline writes.
- `/var/lib/docker/containers`, read-only. This is the filelog source. Only the
  collector mounts it, never bilgeline. bilgeline never reads a log byte.
- a durable volume for filelog checkpoints
  (`/var/lib/otelcol/file_storage`). This is what makes a reload or restart
  resume tailing from the right offset instead of re-reading or skipping lines.

The point of the split: bilgeline holds no destination credentials and never
touches your log bytes. Those live entirely on the collector side.

## The compose stack

`docker-compose.yml` in the repository root is a complete, runnable stack. To
use it:

1. Copy `docker-compose.yml`, `bilgeline.example.yml`, and
   `collector.env.example` to your deploy directory.

2. Copy `bilgeline.example.yml` to `bilgeline.yml` and edit it: name your
   destinations, set a `default_destination`, list any fleet-wide labels. The
   schema is documented inline in the example and in `docs` on the schema
   itself. bilgeline mounts this at `/etc/bilgeline/bilgeline.yml`.

3. Provide the collector's destination secrets. Copy `collector.env.example` to
   `collector.env` and fill in real values, or generate `collector.env` from a
   SOPS-encrypted source at deploy time (see the next section). The compose file
   marks `collector.env` optional, so the stack boots without it: a destination
   whose `${env:VAR}` is unset simply fails to authenticate at the collector
   until you provide it.

4. Bring it up:

   ```
   docker compose up -d
   ```

The collector image is pinned to `0.159.0`, the version bilgeline's config
generator is validated against. When you upgrade one, review the other.

## The collector's user

The compose stack runs the collector as a non-root user by default, because that
is the more secure default and it works on a stock Docker host. There is a
one-line root fallback for hosts where the default does not fit. This section
covers both and when to reach for the fallback.

### The secure default: non-root with the root group

The shipped compose sets `user: "10001:0"` on the collector: uid 10001 (this
image's conventional nonroot uid) with GID 0, the root group. That combination
still reads the logs without being root, for two reasons that hold on a stock
Docker install:

- Group read on the log files. Docker writes its json-file logs as `root:root`
  mode `0640`. The `0640` grants the owning GROUP read, and that group is `0`.
  So a process running with GID 0 can read every log byte without being uid 0.
- Explicit paths, not a directory glob. bilgeline generates an explicit
  per-container include path for each routed container
  (`/var/lib/docker/containers/<id>/<id>-json.log`), never a wildcard over the
  directory. That matters because Docker's container directories are mode `0710`
  (`root:root`): the root group gets the `+x` to traverse into a known path but
  not the `+r` to list the directory. A glob would need to list and would find
  nothing as GID 0. Explicit paths only need traverse plus the file read, both
  of which GID 0 has. This is why the non-root default works at all.

The one wrinkle a non-root collector introduces is its checkpoint volume. The
`file_storage` extension writes filelog read offsets to
`/var/lib/otelcol/file_storage` on a named volume, and a fresh named volume is
`root:root`, which a non-root process cannot write. The stack handles this the
same way it handles the shared config directory: the one-shot `config-seed`
service chowns the checkpoint volume to uid 10001 before the collector starts,
so the non-root collector owns its checkpoint directory and `file_storage`
writes succeed. You do not have to do anything for this. It is in the compose
file.

The generated config the collector reads is world-readable (bilgeline writes it
mode `0644` with an atomic temp-file-and-rename), so a non-root collector reads
it with no extra grant.

### The root fallback: `user: "0:0"`

If the secure default does not fit your host, run the collector as root instead.
Replace the collector's `user:` line in `docker-compose.yml` with:

```
user: "0:0"
```

Root reads any log byte regardless of ownership or directory mode, and owns the
checkpoint volume with no permission step (the `config-seed` chown becomes merely
harmless). It is the simplest thing that always works.

Reach for the root fallback when:

- Your host does not make the json-file logs readable by group 0. A different
  log driver, user-namespace remapping that changes log ownership, or tighter
  directory modes than the stock `0710` can all leave a GID 0 process unable to
  read. If the collector under the non-root default tails nothing (the pipeline
  ships nothing with no error), this is the first thing to suspect.
- You want the least-fuss setup and are comfortable running this one container as
  root. Reading root-owned host logs is the collector's whole job, and it holds
  your destination secrets either way, so some operators simply prefer root here.

The trade is straightforward: the non-root default is the more secure posture and
works on a stock Docker host, the root fallback is the always-works posture. The
stack ships secure and lets you fall back in one line.

## The secret model (S1)

Secrets never live in the compose file, in labels, or in any file bilgeline
writes. A `docker inspect` on a labeled container reveals nothing sensitive, and
the generated `otelcol.yaml` on the shared volume holds no plaintext credential.

The mechanism: a destination in `bilgeline.yml` carries `${env:VAR}` references
instead of literal secrets, for example an `Authorization` header of
`Bearer ${env:LOKI_BEARER}`. bilgeline copies that string verbatim into the
generated config and never expands it. The collector expands it at config load,
from an environment variable set on the collector container. bilgeline's own
process holds no exporter credentials.

So provisioning a secret means setting an environment variable on the collector,
which the turnkey stack does through the `collector.env` env-file.

### SOPS with age

A minimal, real recipe: generate an age key, encrypt a small secrets file with
it, and decrypt it into `collector.env` at deploy time. This mirrors the pattern
the rest of the suite uses, adapted to bilgeline's "secrets are collector env
vars" model.

1. Generate an age key pair. Keep the private key off the host it protects,
   somewhere durable:

   ```
   age-keygen -o bilgeline-age-key.txt
   ```

   This prints the matching public key (`age1...`) to stderr. Keep both the
   private key file and that public value.

2. Point SOPS at that public key with a `.sops.yaml` next to your secrets file,
   so `sops` knows who can decrypt it:

   ```yaml
   creation_rules:
     - path_regex: \.sops\.env$
       age: age1exampleexampleexampleexampleexampleexampleexampleexamplex
   ```

3. Write the secret values as `NAME=value` pairs, then encrypt in place. The
   names are exactly the `${env:VAR}` names your destinations reference:

   ```
   cat > collector.sops.env <<'EOF'
   LOKI_BEARER=<your loki bearer token>
   EOF

   sops -e -i collector.sops.env
   ```

   `collector.sops.env` now holds ciphertext and is safe to commit.

4. At deploy time, decrypt it to the plaintext `collector.env` the collector's
   env-file reads, then bring the stack up:

   ```sh
   sops -d collector.sops.env > collector.env
   docker compose up -d
   rm -f collector.env
   ```

   The age private key from step 1 needs to be available wherever this decrypt
   step runs (as `SOPS_AGE_KEY_FILE`, typically), and nowhere else. Removing the
   plaintext after the collector has started is optional: the collector reads
   the env-file once at container creation and keeps the values in its own
   process environment, so the file on disk is not needed once it is up.

To see exactly which env vars the collector will need, run
`bilgeline generate` (below) and read the `${env:...}` references in the output.

## First-boot ordering

The collector's `--config` points at `/config/otelcol.yaml`, a file bilgeline
writes. On a brand-new volume that file does not exist until bilgeline's startup
reconcile writes it, which would leave the collector crash-looping for the
second or two before the first write.

The stack removes that window with a one-shot `config-seed` service. It writes
an inert-but-valid collector config to the shared volume, but only if none
exists yet. Both the collector and bilgeline `depends_on` it with
`condition: service_completed_successfully`, so neither starts until the seed
has run. The seeded file is byte-for-byte the inert config bilgeline itself
writes when nothing is labeled, so nothing diverges: bilgeline overwrites it on
its startup reconcile and on every container change after. On later boots the
file already exists on the named volume and the seed is a no-op.

Expected first-boot sequence:

1. `config-seed` runs, writes the inert config, exits.
2. The collector starts against that inert config and comes up healthy,
   shipping nothing (nothing is routed yet).
3. bilgeline starts, discovers labeled containers, writes the real config, and
   SIGHUPs the collector to load it.

If you deploy the collector separately from this compose file, seed the shared
volume yourself the same way, or accept that `restart: unless-stopped` will
carry the collector through the brief crash-loop until bilgeline's first write.
Do not point the collector at a config path bilgeline is not writing to.

## Credential rotation means recreating the collector

The collector expands `${env:VAR}` once, at config load, into its process
environment. A running container's environment is fixed. So rotating a
credential is not a bilgeline operation and not a reload: update the value
(re-encrypt `collector.sops.env`, or edit `collector.env`) and recreate the
collector container:

```
docker compose up -d --force-recreate otel-collector
```

bilgeline does not need to regenerate anything for a rotation. The config still
references the same `${env:VAR}` name. Only the value behind it changed, and
only the collector reads it.

## Verify it worked

Dry-run the config bilgeline would generate, without writing or signalling:

```
docker compose exec bilgeline bilgeline generate
```

Read the output. Every destination you expect should be present, each secret
should appear as a `${env:...}` reference and never as a plaintext value, and
the `${env:...}` names should match what you put in `collector.env`.

Check what bilgeline discovered and the collector's health:

```
docker compose exec bilgeline bilgeline status
```

Confirm the collector loaded a real (not inert) config:

```
docker compose logs otel-collector
```

A healthy collector logs `Everything is ready` on startup. After bilgeline's
first write and SIGHUP you should see it reload without a crash. Its health
endpoint is reachable inside the stack at `http://otel-collector:13133`.

Finally, confirm logs land at the destination: check the destination itself
(your Loki, your OTLP endpoint, the file archive) for entries from a labeled
container. If nothing arrives, the usual causes are an unset or wrong
`${env:VAR}` on the collector, a destination endpoint the collector cannot
reach, or a container that is not actually labeled. `bilgeline validate` reports
label and config diagnostics with a nonzero exit on any error.
