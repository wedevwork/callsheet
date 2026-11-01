# Deploy Callsheet

This chapter covers sustained operation: the network and trust boundary, the certificates and who may hold them, the plane and sidecar as services under dedicated accounts, changing addresses, and recovering lost trust state. The [quickstart](quickstart.md) owns installation, the one-machine and second-machine tutorials, manuals, role registration and your first workspace dispatch. If you have never completed a dispatch, do the [one-machine path](quickstart.md#one-machine) first, then come back and enable services here.

Three components are involved. The **plane** holds the shared roster, tasks and workspaces and is the only component with a listener. A **sidecar** runs on each worker machine, connects out to the plane and supervises worker CLI processes. The **coordinator** is your own CLI session talking to a local `callsheet mcp` process.

## Before you deploy

You need:

- exactly one plane, on Linux or macOS, and the **same Callsheet version** on every participating machine (see [Install Callsheet](quickstart.md#install-callsheet), which covers a selected published release and the source-build fallback);
- an installed and authenticated eligible vendor CLI on each worker;
- a private LAN of trusted peers or, preferably, a VPN restricted to those peers.

Callsheet bundles no VPN and has no SaaS account of its own. Your vendor accounts may still be required.

The plane's bind must be an address assigned to the plane host: not a hostname, a wildcard or a public address. The default listen endpoint, `127.0.0.1:8443`, is unsuitable for other machines. Clients use a reachable URL whose host is one of the server certificate's SANs. Only trusted peers may reach the plane's port; apply that with whatever controls your network already has, and do not open the port globally. Sidecars connect out and have no inbound listener.

## Security posture

TLS protects confidentiality but provides no client identity or authorization. Any reachable peer holding the CA can dispatch work and read or write every workspace. Containment is whatever the vendor sandbox provides, which often does not block host reads. Run sidecars as a dedicated OS user. Callsheet adds no sandbox in v1; a Callsheet-side sandbox is planned as E8.

A CA certificate is public trust material, not an access token. A dedicated user reduces accessible files through OS permissions but does not make world-readable host files private. Model approvals are vendor behavior and are not a confinement boundary. See [Enabling a worker](real-adapters.md#enabling-a-worker) and the [support catalog](support-catalog.md) for what is and is not established for each vendor.

## State and certificates

`STATE` below is the selected absolute plane state directory.

| Path under `STATE` | Purpose | Distribution |
|---|---|---|
| `config.json` | schema version and bind | stays on the plane |
| `pki/ca.crt` | the internal CA certificate | the only file copied to clients |
| `pki/ca.key` | the CA private key | never leaves the plane machine |
| `pki/server.crt` | the plane's server certificate; it is not the CA | stays on the plane |
| `pki/server.key` | the server private key | never leaves the plane machine |

Both private keys stay on the plane machine and out of git, logs and example files.

Default state directories:

| Platform | Plane state default |
|---|---|
| Linux | `$XDG_STATE_HOME/callsheet/plane` when `XDG_STATE_HOME` is set and absolute; otherwise `$HOME/.local/state/callsheet/plane`. A relative `XDG_STATE_HOME` is an error. |
| macOS | `$HOME/Library/Application Support/callsheet/plane`, ignoring `XDG_STATE_HOME`. |

`--state-dir` overrides these; quote shell paths. Sidecar defaults are the corresponding paths ending in `sidecar`. The service samples deliberately use explicit paths under the service users' homes, not ambient defaults.

State and PKI directories are owner-only 0700. Keys must be 0600 or 0400; generated files are 0600. Configuration and certificate files must not be group- or other-writable. Use 0600 for distributed CA copies: this is a conservative policy, not a claim that the certificate is secret. Never make the plane directory readable to the worker to share a certificate: copy the public certificate into the worker's own private directory.

This prints paths, SANs, the fingerprint and validity, offline. It is not service health.

<!-- deploy: state-status -->
```sh
callsheet plane status --state-dir "$STATE"
```

## Give clients the CA

Transfer only `pki/ca.crt`, by a file transfer you control: the plane operator sends it, and the recipient stores it at a path in its own account, owned by that account. The samples use `<worker-home>/callsheet/plane-ca.crt`. Then:

<!-- deploy: ca-mode -->
```sh
chmod 600 "$CA"
```

Compare the CA fingerprint printed by `plane init` or `plane status` with the copy's through an independently trusted channel. `sidecar enroll` prints the `ca_fingerprint` it recorded.

**Sidecars** enroll with the copy:

<!-- deploy: ca-enroll -->
```sh
callsheet sidecar enroll --plane "$PLANE" --ca "$CA" --state-dir "$SIDECAR_STATE"
```

Enrollment embeds the CA bytes in `enrollment.json`, so replacing the original copied certificate alone does not update enrollment; enroll again to change it.

The alternative fetches the CA once and verifies it against a fingerprint. It is mutually exclusive with `--ca` and requires a fingerprint you received independently; there is no unauthenticated trust-on-first-use.

<!-- deploy: ca-enroll-pinned -->
```sh
callsheet sidecar enroll --plane "$PLANE" --ca-fingerprint "$FINGERPRINT" --state-dir "$SIDECAR_STATE"
```

**Coordinator and operator machines** each receive their own CA copy and the plane URL. The coordinator guide owns per-CLI MCP registration, authentication and runbook loading: [Claude Code](coordinator.md#claude-code), [OpenAI Codex](coordinator.md#openai-codex), [Grok Build](coordinator.md#grok-build), [Cursor Agent](coordinator.md#cursor-agent). Never copy a worker credential store or `ca.key` to a coordinator. Local stdio `callsheet mcp` is started by the coordinator CLI; it is not a fifth service.

## Run as services

Four templates ship with this repository. Each begins with a comment listing what to replace, and none is runnable until edited.

| Template | Installed as |
|---|---|
| [callsheet-plane.service](examples/deploy/callsheet-plane.service) | Linux system unit `/etc/systemd/system/callsheet-plane.service` |
| [callsheet-sidecar.service](examples/deploy/callsheet-sidecar.service) | Linux system unit `/etc/systemd/system/callsheet-sidecar.service` |
| [org.callsheet.plane.plist](examples/deploy/org.callsheet.plane.plist) | macOS LaunchDaemon `/Library/LaunchDaemons/org.callsheet.plane.plist` |
| [org.callsheet.sidecar.plist](examples/deploy/org.callsheet.sidecar.plist) | macOS LaunchDaemon `/Library/LaunchDaemons/org.callsheet.sidecar.plist` |

### Accounts and paths

Use two distinct non-administrator accounts where plane and sidecar share a machine: `callsheet-plane` and `callsheet-worker`. Each has its own real home directory; a dedicated worker must never inherit the human operator's HOME. Do not run either process as root. OS administrator privileges are for account creation, shared binary installation and service registration only. The commands that use `sudo` below need administrator access; they never run Callsheet as root.

Ubuntu/systemd example, from `useradd --help` on Ubuntu 24.04. On another distribution, inspect your account tool's help first:

<!-- deploy: linux-accounts -->
```sh
sudo useradd --create-home --shell /bin/bash callsheet-plane
sudo useradd --create-home --shell /bin/bash callsheet-worker
```

macOS example: use the desktop route, System Settings > Users & Groups, and create two Standard accounts. The required outcome is the account name, a real home directory and no administrator rights. Callsheet does not prescribe numeric UIDs or GIDs, and no command-line recipe for macOS account creation is verified here.

Install the executable where both accounts can run it and neither can write it. `CALLSHEET` is the executable you installed or built by following the quickstart.

<!-- deploy: install-binary -->
```sh
sudo install -m 0755 "$CALLSHEET" /usr/local/bin/callsheet
```

There are only three kinds of replacement in the templates: the selected account homes (`/REPLACE_WITH_PLANE_HOME`, `/REPLACE_WITH_WORKER_HOME`), the Callsheet path if it is not `/usr/local/bin/callsheet`, and the vendor executable and adapter (`/REPLACE_WITH_ABSOLUTE_WORKER_PATH`). The default account names need not change. Prefer simple home and executable paths without spaces, percent or dollar characters in service files, although interactive Callsheet commands support quoted paths with spaces. In a plist, escape `&` and `<` in a selected path as XML (`&amp;`, `&lt;`), and do not use `$HOME`, a tilde or any shell expansion inside XML arguments. Never put a newline or NUL in a replacement.

The actual paths are `<plane-home>/callsheet/plane` and `<worker-home>/callsheet/sidecar`, with the worker's CA copy at `<worker-home>/callsheet/plane-ca.crt`, its manuals at `<worker-home>/callsheet/manuals` and logs at `<account-home>/callsheet/logs`.

Existing tutorial state is not silently moved or chowned: for a persistent deployment choose fresh service state, or deliberately arrange the existing state's ownership while stopped and keep its exact state path and identity. Never create a second sidecar identity by accidentally changing its state directory.

Service files (root-owned, 0644) hold no secrets. Parent directories must be traversable by the selected account; do not recursively open plane permissions to the worker, and do not recursively chmod vendor credential stores: leave vendor-managed permissions alone.

Prepare the plane account. Log in as it:

<!-- deploy: login-plane -->
```sh
sudo -iu callsheet-plane
```

Create its directories (state directories are created by `plane init`):

<!-- deploy: plane-dirs -->
```sh
mkdir -m 700 "$HOME/callsheet"
mkdir -m 700 "$HOME/callsheet/logs"
```

Enter the plane's private address and set the paths. Variables do not travel between shells or machines, so define them on each.

<!-- deploy: plane-env -->
```sh
STATE="$HOME/callsheet/plane"
read -r PLANE_IP
PLANE="https://$PLANE_IP:8443"
```

Initialize the state as the plane user, before enabling its service. Record the printed `ca_fingerprint`.

<!-- deploy: plane-init -->
```sh
callsheet plane init --state-dir "$STATE" --bind "$PLANE_IP:8443" --san 127.0.0.1 --san "$PLANE_IP"
```

Prepare the worker account the same way:

<!-- deploy: login-worker -->
```sh
sudo -iu callsheet-worker
```

<!-- deploy: worker-dirs -->
```sh
mkdir -m 700 "$HOME/callsheet"
mkdir -m 700 "$HOME/callsheet/logs"
mkdir -m 700 "$HOME/callsheet/manuals"
```

<!-- deploy: worker-env -->
```sh
SIDECAR_STATE="$HOME/callsheet/sidecar"
CA="$HOME/callsheet/plane-ca.crt"
read -r PLANE_IP
PLANE="https://$PLANE_IP:8443"
read -r WORKER
"$WORKER" --version
```

`WORKER` is the absolute path of the vendor executable accessible to the worker account. Keep manuals at mode 0600, owned by the worker. File owners always match the receiving process.

### Authenticate the worker

Authentication is a prerequisite, not something inferred from `--version` or role readiness. As `callsheet-worker`, install the vendor CLI by its vendor's instructions and complete the vendor's own authentication and scoped configuration. Then test a harmless scratch [workspace task](quickstart.md#first-workspace-task) with the exact account, executable, HOME, PATH and model and effort you intend for the service. Do not copy the operator's credential files or place tokens in units or plists, and do not disable vendor safeguards on a failure.

Vendor executable shims may need interpreters or helpers on PATH. Choose an explicit PATH that contains their actual runtime directories: the samples' `/usr/local/bin:/usr/bin:/bin` is a baseline, not a guarantee for every installation. Never rely on shell profile initialization, which a service does not run.

**Unverified:** whether a selected vendor's credentials, keychain or session are usable in an unattended service under that account on your host. Repository readiness probes cannot establish this. Settle it with a real task launched through the installed service, and again after a reboot before any login to the worker account. In particular, macOS service access to user authentication is not promised. A failed probe blocks enabling or claiming that worker deployment: keep the foreground quickstart for diagnosis, and do not silently switch to another login account.

To use another vendor, change the single adapter flag and path. The [current worker recipes](real-adapters.md#current-worker-recipes) hold each vendor's policy, and all [platform restrictions](support-catalog.md) stay as they are. Nothing in the samples implies a factory model or a vendor token.

### Linux with systemd

Install system units, not user units. The sequence is: prepare accounts, the binary, authentication and state as above; edit copies of the two templates; check them; install; start the plane; verify it; enroll; start the sidecar; verify the roster.

No `REPLACE_WITH` token may remain (these print nothing when you are done):

<!-- deploy: linux-check -->
```sh
grep -n REPLACE_WITH callsheet-plane.service
grep -n REPLACE_WITH callsheet-sidecar.service
```

Lint the edited copies. This checks the unit syntax only, and needs the named executables to exist:

<!-- deploy: linux-lint -->
```sh
systemd-analyze verify callsheet-plane.service callsheet-sidecar.service
```

Install root-owned copies and start the plane:

<!-- deploy: linux-install -->
```sh
sudo install -m 0644 callsheet-plane.service /etc/systemd/system/callsheet-plane.service
sudo install -m 0644 callsheet-sidecar.service /etc/systemd/system/callsheet-sidecar.service
sudo systemctl daemon-reload
```

<!-- deploy: linux-start-plane -->
```sh
sudo systemctl enable --now callsheet-plane.service
sudo systemctl status callsheet-plane.service
```

From an account holding a CA copy, verify TLS against the running plane:

<!-- deploy: verify-plane -->
```sh
callsheet node ls --plane "$PLANE" --ca "$CA"
```

As `callsheet-worker`, after copying the CA and running the `chmod` above, enroll before enabling the sidecar's service. Enrollment contacts the plane, so it follows the plane's start:

<!-- deploy: worker-enroll -->
```sh
callsheet sidecar enroll --state-dir "$SIDECAR_STATE" --plane "$PLANE" --ca "$CA"
```

<!-- deploy: linux-start-sidecar -->
```sh
sudo systemctl enable --now callsheet-sidecar.service
sudo systemctl status callsheet-sidecar.service
```

A started service is not yet a connected node. Confirm the node shows `online` before you register roles:

<!-- deploy: verify-node-online -->
```sh
callsheet node ls --plane "$PLANE" --ca "$CA"
```

Until then, `role add` fails with `node <id> is offline; start its sidecar and retry`: wait for the sidecar and retry.

Register roles with the [quickstart recipe](quickstart.md#one-machine), then check the node and each role's `CAN_ACCEPT`:

<!-- deploy: verify-roster -->
```sh
callsheet node ls --plane "$PLANE" --ca "$CA"
callsheet role ls --plane "$PLANE" --ca "$CA"
```

Inspect the logs after every failed start. A running or restarting unit is not proof of readiness.

<!-- deploy: linux-logs -->
```sh
sudo journalctl -u callsheet-plane.service
sudo journalctl -u callsheet-sidecar.service
```

A failed configuration may trigger rate-limited retries (`Restart=on-failure`, `RestartSec=5s`): stop the unit and correct it. A service-manager stop does not request a restart. `SuccessExitStatus=130` acknowledges Callsheet's documented interrupt exit. The `network-online.target` ordering is not proof that a VPN address is ready; diagnose a missing interface from the logs and retry once the host network is ready. A sidecar unit has no dependency on a plane unit, because its plane may be remote.

Stop the sidecar, then the plane:

<!-- deploy: linux-stop -->
```sh
sudo systemctl stop callsheet-sidecar.service
sudo systemctl stop callsheet-plane.service
```

Start them again in the opposite order:

<!-- deploy: linux-start -->
```sh
sudo systemctl start callsheet-plane.service
sudo systemctl start callsheet-sidecar.service
```

Editing a unit requires stop, `daemon-reload`, start. To uninstall, disable and remove only the unit files; state stays where it is:

<!-- deploy: linux-uninstall -->
```sh
sudo systemctl disable --now callsheet-sidecar.service
sudo systemctl disable --now callsheet-plane.service
sudo rm /etc/systemd/system/callsheet-sidecar.service
sudo rm /etc/systemd/system/callsheet-plane.service
sudo systemctl daemon-reload
```

References: [systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html), [systemd.exec](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html), [systemd.kill](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html), [systemd-analyze](https://www.freedesktop.org/software/systemd/man/latest/systemd-analyze.html). `KillMode=control-group` terminates the worker's descendants with the service; verify idle shutdown on your host. It is not a promise that an active task is preserved.

### macOS with launchd

Worker choice on macOS: Claude and Codex may be enabled with `--claude-adapter` or `--codex-adapter`; the sidecar warns once at startup that their recipes are Linux-qualified and that macOS vendor sandbox and exit behavior are unverified. `--grok-adapter` and `--cursor-adapter` are accepted syntactically, but their roles are refused on macOS: they are not working macOS worker choices. See the [support catalog](support-catalog.md). The sample stays on Claude; the usable alternative is Codex, with the same unverified qualification.

Use the two system LaunchDaemons under `/Library/LaunchDaemons/`, with labels `org.callsheet.plane` and `org.callsheet.sidecar`. Do not substitute LaunchAgents: a user agent runs only while that user is logged in, which does not provide unattended service operation. The process users are the dedicated non-administrator accounts; `UserName` is never root.

**Unverified:** that your macOS version supports this `launchctl` syntax and these plist keys with the intended meaning. Before relying on the recipe, read `launchctl help bootstrap`, `launchctl help bootout`, `launchctl help print`, `man launchd.plist` and `plutil -help` on the target, and reconcile any difference. Reboot behavior, account login and keychain availability, file-ownership acceptance and descendant cleanup all need observation on the real host.

As each account, create its log files before the first load, with mode 0600:

<!-- deploy: macos-logs-plane -->
```sh
touch "$HOME/callsheet/logs/plane.out.log"
touch "$HOME/callsheet/logs/plane.err.log"
chmod 600 "$HOME/callsheet/logs/plane.out.log"
chmod 600 "$HOME/callsheet/logs/plane.err.log"
```

<!-- deploy: macos-logs-worker -->
```sh
touch "$HOME/callsheet/logs/sidecar.out.log"
touch "$HOME/callsheet/logs/sidecar.err.log"
chmod 600 "$HOME/callsheet/logs/sidecar.out.log"
chmod 600 "$HOME/callsheet/logs/sidecar.err.log"
```

Check edited copies of both templates; no `REPLACE_WITH` token may remain, and [plutil](https://it-training.apple.com/compliance/tutorials/course/sec050/) is part of the macOS base system:

<!-- deploy: macos-check -->
```sh
grep -n REPLACE_WITH org.callsheet.plane.plist
grep -n REPLACE_WITH org.callsheet.sidecar.plist
plutil -lint org.callsheet.plane.plist
plutil -lint org.callsheet.sidecar.plist
```

Install root-owned copies and load the plane. Lint is syntax only and does not settle runtime behavior.

<!-- deploy: macos-install -->
```sh
sudo install -m 0644 org.callsheet.plane.plist /Library/LaunchDaemons/org.callsheet.plane.plist
sudo install -m 0644 org.callsheet.sidecar.plist /Library/LaunchDaemons/org.callsheet.sidecar.plist
```

<!-- deploy: macos-start-plane -->
```sh
sudo launchctl bootstrap system /Library/LaunchDaemons/org.callsheet.plane.plist
sudo launchctl print system/org.callsheet.plane
```

Then, as on Linux, [verify the plane](#linux-with-systemd), copy the CA, enroll as `callsheet-worker`, and load the sidecar:

<!-- deploy: macos-start-sidecar -->
```sh
sudo launchctl bootstrap system /Library/LaunchDaemons/org.callsheet.sidecar.plist
sudo launchctl print system/org.callsheet.sidecar
```

Then, also as on Linux, confirm the node shows `online` before you register roles, and check the roster.

Unlike systemd's `on-failure`, `KeepAlive` restarts any exit while the job is loaded, so intentional maintenance must use `bootout`, not a signal. `ExitTimeOut` of 60 seconds is a manager cleanup limit, not a supported task-duration bound. Stop the sidecar, then the plane:

<!-- deploy: macos-stop -->
```sh
sudo launchctl bootout system/org.callsheet.sidecar
sudo launchctl bootout system/org.callsheet.plane
```

Restart means `bootout`, verify exit, then `bootstrap`. Do not use `kickstart` as an address-change stop. To remove a service, `bootout` it first and remove only the definition; state and logs stay:

<!-- deploy: macos-remove -->
```sh
sudo rm /Library/LaunchDaemons/org.callsheet.sidecar.plist
sudo rm /Library/LaunchDaemons/org.callsheet.plane.plist
```

Read the two named log files as the owning account:

<!-- deploy: macos-read-logs -->
```sh
tail -n 50 "$HOME/callsheet/logs/plane.err.log"
tail -n 50 "$HOME/callsheet/logs/plane.out.log"
```

The sidecar's are `sidecar.err.log` and `sidecar.out.log`, likewise. References: [Creating Launch Daemons and Agents](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html) and the [launchd.plist manual source](https://raw.githubusercontent.com/apple-oss-distributions/launchd/main/man/launchd.plist.5), a historical source that does not prove a current host's behavior.

### Maintenance

Maintenance is idle-only: let tasks finish, stop the sidecar and then the plane through their manager, and verify exit before editing state. A manager stop or restart can terminate worker processes, so do not expect active tasks to survive a sidecar restart. There is no task-cancellation stop script, and no kill-by-name or deletion step. Confirm that no Callsheet or task process remains for each account:

<!-- deploy: confirm-exit -->
```sh
ps -u callsheet-worker
ps -u callsheet-plane
```

Logs may contain operational information: retain them privately and inspect them before sharing. Log retention and rotation are your responsibility, not an automatic Callsheet feature.

### Back up the plane

Take this backup before anything has gone wrong, as routine maintenance. It is what the [backup branch](#complete-backup-available) of CA-loss recovery restores, and it cannot be created after the CA is lost. Callsheet has no backup command or schedule; this is a plain copy of stopped state.

Stop the plane and verify exit as above, then work as the plane account, from its home. `BACKUP_STATE` is an absolute, unused path in that account: choose the date suffix, and use a new name for each backup, because copying onto an existing directory would nest the copy.

<!-- deploy: backup-env -->
```sh
STATE="$HOME/callsheet/plane"
BACKUP_STATE="$HOME/callsheet/plane-backup-REPLACE_WITH_DATE"
```

<!-- deploy: backup-plane -->
```sh
cp -Rp "$STATE" "$BACKUP_STATE"
```

Record the plane's `ca_fingerprint` with the backup's name, then start the plane again. The copy holds `ca.key`, so it stays on the plane machine under the same private-key boundary.

## Change the plane address

This keeps the CA and the node identities. It is for an address change under the same CA, not the fresh-plane branch of [Recover after CA loss](#recover-after-ca-loss). Set `STATE`, `SIDECAR_STATE`, `CA`, `PLANE` and the replacement private IPv4 address `PLANE_IP` on each machine; they are not shared between shells. For IPv6, the bind and URL bracket the address (`[fd00::10]:8443`, `https://[fd00::10]:8443`) and the same steps apply.

1. Record the current CA fingerprint, the state path, the full desired SAN list and every client's URL.

   <!-- deploy: address-status -->
   ```sh
   callsheet plane status --state-dir "$STATE"
   ```

   Let active tasks finish. Stop service-managed sidecars, then the plane, through their manager, and confirm the processes exited. Do not merely kill a process that a manager will restart. Preserve state; do not reinitialize it.

2. If the listening address changes, edit **only** `bind` in `STATE/config.json` while stopped, retaining `schema_version` 1, ownership and mode. This illustrative example changes from one private address to another. Before:

   ```json
   { "schema_version": 1, "bind": "192.168.1.10:8443" }
   ```

   After:

   ```json
   { "schema_version": 1, "bind": "192.168.1.20:8443" }
   ```

   `plane run --bind NEW` is not a configuration update and refuses a mismatch with this diagnostic: `--bind %s differs from the configured bind %s; init never edits existing configuration: stop the plane and edit %s to change it`. The placeholders are the requested bind, the configured bind and the `config.json` path; the check is shared with initialization, so it says "init" even when `plane run` reports it. If only DNS or a SAN changes and the listening bind remains valid, no configuration edit is needed.

3. Reissue the server certificate as the plane user:

   <!-- deploy: address-reissue -->
   ```sh
   callsheet plane cert reissue --state-dir "$STATE" --san 127.0.0.1 --san "$PLANE_IP"
   ```

   These are a **complete replacement** list: include every DNS name and IP address still used, not just the new one. Ports do not belong in SANs. Inspect the result and verify the CA fingerprint is unchanged:

   <!-- deploy: address-verify-ca -->
   ```sh
   callsheet plane status --state-dir "$STATE"
   ```

4. Start the plane with the same state path; its service definition passes no bind or SAN override flags. Verify at the new URL:

   <!-- deploy: address-node-list -->
   ```sh
   callsheet node ls --plane "$PLANE" --ca "$CA"
   ```

5. Re-enroll a sidecar only when its enrolled URL is no longer valid; otherwise leave its enrollment unchanged and restart any sidecar stopped for maintenance. For a URL change, keep it stopped, enroll using the **same** sidecar state directory and the existing CA at the new URL, confirm the printed `node_id` is unchanged, then restart it with its adapter paths:

   <!-- deploy: address-reenroll -->
   ```sh
   callsheet sidecar enroll --state-dir "$SIDECAR_STATE" --plane "$PLANE" --ca "$CA"
   ```

   Update operator environments and each coordinator's MCP URL only where their URL changes, restarting the affected local MCP process or session. A client whose URL remains valid keeps its URL and CA.

6. Confirm node liveness and role readiness. If anything fails, stop and diagnose; never delete state, blindly redispatch or rotate the CA to fix an address mismatch. Rollback, if required, uses the recorded bind and the full old SAN set while stopped, then restores client URLs.

## Recover after CA loss

Callsheet has no CA reset, rotation or in-place PKI reconstruction command. It deliberately refuses incomplete trust, even when registry and workspace files survive, and it never wraps an existing roster in a new CA. Do not remove PKI files, overwrite the configuration or run `init` hoping it repairs them. If the private key is merely present with the wrong mode, this is not CA loss: see the permission diagnostics under [the backup branch](#complete-backup-available).

First stop dispatch and all affected services through their manager, then confirm that all Callsheet commands and task processes using the affected state have exited (see [Maintenance](#maintenance)). An unconfirmed cleanup is a blocker.

On the plane, as its owner and from its home, not from inside the state directory:

<!-- deploy: recover-plane-paths -->
```sh
STATE="$HOME/callsheet/plane"
OLD_STATE="$HOME/callsheet/plane-old-REPLACE_WITH_DATE"
```

`OLD_STATE` is an unused sibling; choose the date suffix and add a unique suffix if the name is taken. Check that `STATE` is on the same filesystem as the account home, so it is not a mount point: both lines of this output must name the same mount.

<!-- deploy: recover-plane-mount -->
```sh
df -P "$STATE" "$HOME"
```

A deployment under a custom mount or custom unit is outside this recipe: stop and obtain a host-specific procedure rather than copying or deleting mounted state. Otherwise, move the damaged directory aside. This aborts unless the source is the expected real directory, not a symlink, and the destination is absent, including no dangling symlink. It prints a line only on success. Never move onto an existing directory, which can nest the source, and never use a force or overwrite option.

<!-- deploy: recover-plane-move -->
```sh
test -d "$STATE" &&
  test ! -L "$STATE" &&
  test ! -e "$OLD_STATE" &&
  test ! -L "$OLD_STATE" &&
  mv "$STATE" "$OLD_STATE" &&
  test -d "$OLD_STATE" &&
  test ! -e "$STATE" &&
  printf 'archive ready\n'
```

Allow no concurrent operator or service activity during the move. The archive keeps its 0700 permissions and file modes; its contents, not inode metadata such as the rename time, are what is preserved. Keep it until you deliberately delete it. Then take one of the two branches.

### Complete backup available

Select an existing complete backup: one taken with the plane stopped, on this machine, [before the loss](#back-up-the-plane). Do not take a new one now: `STATE` has been moved aside, and a copy of damaged state is not a backup. Set `BACKUP_STATE` to the selected backup's absolute path, with the name you chose when you took it:

<!-- deploy: restore-select -->
```sh
BACKUP_STATE="$HOME/callsheet/plane-backup-REPLACE_WITH_DATE"
```

Restore it into the now-vacant canonical path on the same machine, including configuration, all PKI, registry, tasks and workspaces with their ownership and private permissions. Never merge fragments or overwrite the only damaged copy.

<!-- deploy: restore-backup -->
```sh
cp -Rp "$BACKUP_STATE" "$STATE"
```

Validate the status and the expected CA fingerprint before starting the plane with its unchanged service definition:

<!-- deploy: restore-status -->
```sh
callsheet plane status --state-dir "$STATE"
```

A restore with group or other access on the root fails with `trust_failed`: `state directory %s has mode %04o; it must not be accessible by group or others (chmod 700; permissions are never repaired automatically)`. Wrong key modes fail with `private key %s has mode %04o; it must be owner-only (0600 or 0400; permissions are never repaired automatically)`. Restore root mode 0700 and key modes 0600 or 0400: correct the permissions and do not re-initialize. The root check rejects group and other bits, not every owner-only mode different from 0700.

Existing clients keep their CA if the restored CA is identical. State rolls back to the backup point: inspect tasks and workspace results, and do not automatically redispatch missing work. Keep backups containing `ca.key` on the plane machine under the same private-key boundary; this is not an off-host disaster-backup promise.

Even a complete plane backup can lack tasks admitted after its snapshot. If a reused sidecar reports unknown old tasks and stays unready, preserve it and use the same fresh-sidecar, new-node procedure below, re-registering affected roles deliberately. Do not erase journals or guess that a reconnect will repair it.

### No complete backup

This is a new plane, not a recovery of the old registry, history or workspace database. No manual import or merge is supported; the old directory stays for inspection.

Initialize fresh state at the original canonical path, with the intended bind and full SANs, and inspect its new fingerprint. Keep the service path unchanged.

<!-- deploy: fresh-env -->
```sh
read -r PLANE_IP
PLANE="https://$PLANE_IP:8443"
```

<!-- deploy: fresh-init -->
```sh
callsheet plane init --state-dir "$STATE" --bind "$PLANE_IP:8443" --san 127.0.0.1 --san "$PLANE_IP"
```

<!-- deploy: fresh-plane-run -->
```sh
callsheet plane run --state-dir "$STATE"
```

That is the same command as the service's; start it through the manager, using the foreground form only to diagnose. Distribute only the new `ca.crt` to **every** sidecar, coordinator and operator machine. Coordinator and operator CA copies keep their existing paths with the new certificate, and each coordinator's MCP connection restarts with the new trust.

On each worker, stop the old sidecar through its service manager and confirm its task processes have exited. Move the entire stopped old sidecar state directory to an unused sibling and preserve its contents untouched for inspection, including `identity.json`, `enrollment.json`, `tasks/` and `workspace-cache/`. Do not re-enroll or run that old directory against the new plane. As the worker, from its home:

<!-- deploy: recover-sidecar-paths -->
```sh
SIDECAR_STATE="$HOME/callsheet/sidecar"
OLD_SIDECAR_STATE="$HOME/callsheet/sidecar-old-REPLACE_WITH_DATE"
```

<!-- deploy: recover-sidecar-mount -->
```sh
df -P "$SIDECAR_STATE" "$HOME"
```

<!-- deploy: recover-sidecar-move -->
```sh
test -d "$SIDECAR_STATE" &&
  test ! -L "$SIDECAR_STATE" &&
  test ! -e "$OLD_SIDECAR_STATE" &&
  test ! -L "$OLD_SIDECAR_STATE" &&
  mv "$SIDECAR_STATE" "$OLD_SIDECAR_STATE" &&
  test -d "$OLD_SIDECAR_STATE" &&
  test ! -e "$SIDECAR_STATE" &&
  printf 'archive ready\n'
```

Before enrolling, replace the public CA copy in place at `<worker-home>/callsheet/plane-ca.crt`, retaining worker ownership and mode 0600; `CA` continues to name that path:

<!-- deploy: fresh-ca-mode -->
```sh
CA="$HOME/callsheet/plane-ca.crt"
chmod 600 "$CA"
read -r PLANE_IP
PLANE="https://$PLANE_IP:8443"
```

Enroll fresh state at the now-vacant canonical path, with the new CA and URL. It gets a **new node ID**:

<!-- deploy: fresh-enroll -->
```sh
callsheet sidecar enroll --state-dir "$SIDECAR_STATE" --plane "$PLANE" --ca "$CA"
```

Keep the sidecar service's state path unchanged and start it with the same explicit adapter paths:

<!-- deploy: fresh-sidecar-run -->
```sh
callsheet sidecar run --state-dir "$SIDECAR_STATE" --claude-adapter "$WORKER"
```

Confirm the new node ID shows `online` before you register roles:

<!-- deploy: fresh-node-online -->
```sh
callsheet node ls --plane "$PLANE" --ca "$CA"
```

Until then, `role add` fails with `node <id> is offline; start its sidecar and retry`: wait for the sidecar and retry.

Explicitly re-register roles on the new plane, using the new node IDs and your retained manuals and model and effort settings, with the [quickstart registration recipe](quickstart.md#one-machine). Recreate or reseed needed workspaces from separately retained source and results; `workspace-cache/` is a disposable cache, not a backup or an import path. The new sidecar builds its own cache for new work, and this path leaves the stopped old cache untouched; running the old directory under changed trust could alter it. Do not copy plane private keys or vendor credentials.

Old in-flight tasks and results do not transfer. Do not automatically redispatch old work; review its side effects first. Check:

<!-- deploy: fresh-task-list -->
```sh
callsheet task ls --plane "$PLANE" --ca "$CA"
```

The new plane shows no old-plane tasks or results, even if the stopped old sidecar still has files under `tasks/`. Keep that old state for inspection until you choose to delete it. A fresh plane has an empty list before new dispatch; after new work, only its own tasks appear.

Old state is preserved because the new plane cannot adopt it. A sidecar that reports an execution the plane does not know is told to stop it, keeps its journal as local evidence, and stays unready: dispatch to its roles is refused with `worker_unready`. Its journals are not automatically forgotten, so an old node cannot be assumed ready. This procedure renames the old directories for preservation, and neither deletes nor imports their contents. Neither service definition changes, because both keep their canonical state paths.

## Connection not trusted

Every trust failure carries the phrase `connection not trusted`, and exits 6. A missing or wrong CA and a name mismatch can therefore produce the same diagnostic; read the reason after the phrase, and check in this order:

1. The URL's hostname or IP against the server SANs from the offline `plane status` on the plane.
2. The expected CA copy exists, is readable by the actual process user and belongs to this plane. Re-enroll sidecars whose embedded CA is stale.
3. The client and plane clocks, and certificate validity.

After these, check expiry. Use `plane cert reissue` for the server certificate under a valid surviving CA. CA loss or an expired CA goes to [Recover after CA loss](#recover-after-ca-loss). Never disable verification, which Callsheet does not offer, and never copy private keys.

## Verify the deployment

The offline checks in this repository prove the documented commands against the real command tree and loopback fixtures. They do not certify vendor authentication, host boot behavior, service-manager operation or usability. Before relying on a deployment, do this on a clean host, following only these documents:

- [ ] Install Callsheet from an available release or the source fallback.
- [ ] Provision the dedicated users and authenticate the selected vendor under the actual worker account.
- [ ] Initialize the plane, copy the CA, enroll the sidecar and configure the coordinator from its guide.
- [ ] Install both services, verify node and role readiness, and complete the linked quickstart workspace task through delivery.
- [ ] Reboot: confirm daemon start without an interactive terminal, and real vendor authentication then.
- [ ] Confirm restart after failure, and no respawn after a deliberate manager stop.
- [ ] Check logs and file ownership.
- [ ] Exercise an address reissue with client updates, and recovery in a disposable deployment.
- [ ] Confirm the plane service outlives the coordinator's exit, and that the sidecar account cannot read the plane's keys.

Time your first dispatch and record every snag. Record what you observe; a failure stays a documented blocker, never a reason to broaden permissions or to label a vendor verified.

| OS and architecture | Callsheet and vendor versions | Service account | Start and end time | Action | Expected result | Observed result | Snag |
|---|---|---|---|---|---|---|---|
|  |  |  |  |  |  |  |  |
