# Assumptions

| Area | Assumption | If wrong |
|---|---|---|
| Program | Long-running, multi-file, may restart briefly. | Short-lived CLI: check at launch, apply on exit. |
| Start | Always via the launcher in the install root. | No launcher means no updates (`ErrNotManaged`). |
| Install root | Writable by the user running the program (per-user install). | `Program Files`, `/usr/bin`, `/Applications` need a privileged helper (not built). |
| Restart | Something restarts a crashed program; the launcher only restarts for a requested restart or immediate rollback. | Rollback then happens at the next user launch. |
| Packages | Regular files and directories only; entry point fixed at bootstrap. | Symlinks (macOS `.app`) need a link-aware extractor; new entry point needs re-bootstrap. |
| Data | User data lives outside the root; new code migrates old data and old code can restore a backup. | Real databases need expand/contract migrations kept readable until `Confirm`. |
| Attacker | Controls network, server and CDN; cannot steal the offline signing key or alter the shipped client. | Stolen key: no revocation. |
| Freshness | Manifests expire (30 d default) and carry issue times; clients keep a replay floor. A badly fast client clock stops updates. | Shorten `-valid-for`. |
| Local attackers | Anyone who can write the root can replace the program; Unix checks reject unsafe roots. Windows ACLs are not inspected. | Admin-owned root plus privileged updater. |
| Smoke test | Proves only that the entry runs and prints the promised version; real health is the launcher's start counting plus `Confirm`. | Add a `--selftest`. |
| Rollback | 3 unconfirmed failed starts; clean exits are given back; exit 78 is never counted. | Change `MaxStartAttempts`. |
| Rollouts | Wanted; enforced by `updateserver` only; random per-install client ID sent with manifest requests. | `Config.NoClientID` (then only 100% releases are received). |
| Several copies | Safe via cross-process locks; each start of a trial release counts. | App-level single-instance check. |
| Disk | Checked before downloading (remaining bytes + 3x package + 1 MiB). | Tune `unpackedFactor`. |

## What was run

| Platform | Unit tests | End-to-end (real launcher and binaries) |
|---|---|---|
| Linux amd64 | Pass, incl. `-race` | All pass |
| macOS | Compiles and vets | Not run |
| Windows | Compiles and vets (amd64, arm64, 386) | Not run |

Windows-only code and its tests have never executed: launcher job object, rename/read retries, antivirus-busy handling, locks, process-tree cleanup. CI (`windows-latest`, `macos-latest`) is configured but has never run, and its action SHAs are unverified.

## Not built

Self-updating launcher, symlinks in packages, delta updates, OS-signature checks, key revocation, privileged installer, Windows Service support, GUI prompt, bandwidth limits, rollout health reporting.
