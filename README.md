# selfupdate

Multi-file Go program (executable, libraries, data) that replaces itself safely on Windows, macOS and Linux. Standard library only.

**Versions are directories.** An update unpacks the new version beside the old one, then atomically rewrites a one-line pointer file. Running files are never modified; rollback is flipping the pointer back.

```
<root>/launcher       stub that users/service managers run; never self-updated
<root>/current        pointer file: "1.2.0"
<root>/root.json      entry point inside a version (fixed at bootstrap)
<root>/state.json     trial, rollback, skip-list, replay floor, client ID
<root>/versions/X.Y.Z whole versions: current, rollback target, last good
<root>/staging/       downloads and unpacking in progress
```

Code: `cmd/launcher`, `cmd/agent` (example), `cmd/updateserver`, `cmd/updatectl`; `internal/{launcher,update,pack,release,fsutil,appstate,winjob}`.

## Update flow

1. **Publish** (`updatectl publish`): deterministic `.tar.gz`, checked with the client's archive rules, manifest signed with Ed25519, manifest written last.
2. **Check**: poll `<base>/<channel>/<os>_<arch>/manifest.json`; require valid signature, right target, unexpired, not older than one already seen, `version > installed`.
3. **Download** to `staging/` (resumable, size and SHA-256 verified).
4. **Unpack**: regular files and directories only; no links, `..`, absolute or reserved names; size/count limits; exclusive create; hash re-checked while reading.
5. **Smoke test**: `<entry> --version` in a minimal environment, own process group (Unix) or job object (Windows); must print the manifest version. Tree is then pruned and re-verified.
6. **Install**: rename into `versions/`, record trial, write pointer (the switch). A crash before the pointer write leaves the old version current. Versions not newer than the installed one are refused.
7. **Restart**: the program exits 75; the launcher starts the new version. `RestartWhenIdle` saves state first.
8. **Rollback**: the launcher counts unconfirmed starts of a trial version; after 3 it points back at the previous version and blacklists the bad one. Exit 78 is never counted.

## Make a program updatable

* Use `update.New(...)` (needs `SELFUPDATE_ROOT`, set by the launcher; else `ErrNotManaged`: run without updates).
* `--version` prints the bare version and exits 0.
* Call `Confirm()` once healthy; exit 0 on deliberate quit; `RestartWhenIdle`/`Restart()` to apply.
* Release-fault failures that aren't crashes: `ReportBadRelease` then `Restart()`. Machine-fault failures: exit `EnvironmentExitCode` (78).
* Locate files from your executable's directory; write user data outside the root; no symlinks in packages.
* Persist state with `appstate` (versioned, migrated, backed up; restore on rollback). Real databases need expand/contract migrations.

`cmd/agent` does all of this.

## Build and publish

```
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=1.2.3 -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) -X main.publicKey=<b64>[,<b64>] -X main.defaultUpdateURL=https://updates.example.com/releases" -o prog/agent ./cmd/agent
updatectl keygen  -out keys                      # once; keep private.key off the server
updatectl publish -key keys/private.key -root releases -version 1.2.3 -os linux -arch amd64 -dir prog -entry agent [-rollout 10]
updatectl refresh -key keys/private.key -root releases -os linux -arch amd64   # before -valid-for (30d) lapses
```

Build before publishing (a manifest issued before the build is refused). Production builds ignore `-update-url` and `-allow-insecure-updates` unless built with `-X main.devOverrides=1`.

## First install

```
go build -o launcher ./cmd/launcher
updatectl bootstrap -root /opt/myapp -launcher launcher -package v1.tar.gz -version 1.0.0 -entry agent
/opt/myapp/launcher [program args]
```

Point the shortcut/service at `<root>/launcher`. SIGTERM is forwarded; SIGINT is escalated to SIGTERM after 3 s; anything still running 30 s after a stop signal is killed. On Windows the launcher runs in a kill-on-close job object, so the program dies with it.

## Server, rollouts, keys

* `updateserver` serves only the release layout (GET/HEAD, Range, TLS optional); never put `private.key` in the served directory. Static hosting also works, without staged rollouts.
* Staged rollout: `publish -rollout N`, then `updatectl rollout -root R -os O -arch A -percent N | -halt | -resume`. Only `updateserver` enforces it; `rollout.json` is unsigned and can only choose between signed manifests.
* Manifests expire; run `refresh` periodically. Key rotation: ship a build trusting `new,old`, then sign with new. Compromised keys cannot be revoked.

## Security model

* Authenticity is the Ed25519 signature, not the transport. A compromised server/CDN can withhold updates but not forge them.
* Root must not be world-writable; as root, it and its ancestors must be root-owned (Unix).
* OS code signing (Authenticode, Developer ID) is not checked; sign binaries in your pipeline.

## Limits

* Launcher is not self-updating; replace the file to change it.
* **Windows is unverified**: the Windows code has been compiled and vetted, never run. Install under the user's profile and don't run elevated from a user-writable directory (no ACL check). Services are unsupported.
* No symlinks in packages, delta updates, key revocation or privileged install helper.

Assumptions and test status: [ASSUMPTIONS.md](ASSUMPTIONS.md). 
