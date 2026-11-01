# Callsheet quickstart

Target: a first completed dispatch within 15 minutes after Callsheet and an authenticated vendor CLI are installed and the prerequisites below are satisfied. This is a target, not a measured result; record installation and prerequisite preparation time separately. Model latency and vendor login are outside it.

Callsheet binaries support Linux and macOS on amd64 and arm64; Windows is unsupported. Callsheet itself is supported on macOS, but that does not qualify a vendor worker there: see [Prerequisites](#prerequisites).

## Install Callsheet

Open the [releases page](https://github.com/wedevwork/callsheet/releases), select a published version (an RC is marked prerelease), and copy its tag below. If no release is published yet, use the [source-build fallback](#source-build-fallback). A workflow in the repository does not mean a binary is already available.

Select your asset from `uname -s` and `uname -m`:

| `uname -s` | `uname -m` | Asset |
|---|---|---|
| Linux | x86_64 | `callsheet-linux-amd64` |
| Linux | aarch64 or arm64 | `callsheet-linux-arm64` |
| Darwin | x86_64 | `callsheet-darwin-amd64` |
| Darwin | arm64 | `callsheet-darwin-arm64` |

The steps below use bash (the macOS system bash is enough), curl, awk and the platform checksum utility: `sha256sum` on Linux, `shasum -a 256` on macOS. A browser download also works, but curl is the tested path. Work from a fresh folder that holds neither plane state nor a workspace.

Run this command first from your usual terminal (including zsh on macOS). It starts a child bash shell; paste the following installation commands only there. Do not use exec. On success type exit once to return to your original shell; on an error bash may return you there automatically. Your original shell stays open. Before retrying after a download or checksum failure, return to the original shell and run `mv callsheet-download callsheet-download-old` (if taken, choose an unused name). Then restart the child from that folder and paste the corrected recipe. The parent stayed in its original folder when the child changed directory; simply restarting bash there without moving the old directory will fail again. A placeholder rejection happens before directory creation and needs only corrected assignments. Do not continue later fences after a failed step.

<!-- quickstart: install-shell -->
```sh
bash --noprofile --norc
```

Edit both assignment values before pasting: replace `VERSION` with a tag copied from the releases page and `ASSET` with one basename from the table. The placeholders are deliberately invalid and name no existing release. Paste one block at a time, never several together.

<!-- quickstart: download -->
```bash
{
set -euo pipefail
VERSION='REPLACE_WITH_PUBLISHED_TAG'
ASSET='REPLACE_WITH_ASSET_NAME'
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$ ]] || { printf 'Replace VERSION with a published release tag\n' >&2; exit 1; }
case "$ASSET" in
  callsheet-linux-amd64|callsheet-linux-arm64|callsheet-darwin-amd64|callsheet-darwin-arm64) ;;
  *) printf 'Choose an asset from the table\n' >&2; exit 1 ;;
esac
mkdir callsheet-download
cd callsheet-download
curl --fail --location --output "$ASSET" "https://github.com/wedevwork/callsheet/releases/download/$VERSION/$ASSET"
curl --fail --location --output SHA256SUMS "https://github.com/wedevwork/callsheet/releases/download/$VERSION/SHA256SUMS"
awk -v asset="$ASSET" '$2 == asset { print; found++ } END { if (found != 1) exit 1 }' SHA256SUMS > selected.sha256
}
```

Stop immediately if a command fails. In particular, never execute an HTML error page or partial download, never proceed after a checksum mismatch, and never overwrite an existing download directory.

Then run the checksum for your platform. On Linux:

<!-- quickstart: checksum-linux -->
```bash
sha256sum --check selected.sha256
```

On macOS:

<!-- quickstart: checksum-macos -->
```bash
shasum -a 256 --check selected.sha256
```

Only after that reports OK, install:

<!-- quickstart: install -->
```bash
{
mkdir -p "$HOME/.local/bin"
chmod 755 "$ASSET"
cp "$ASSET" "$HOME/.local/bin/callsheet"
export PATH="$HOME/.local/bin:$PATH"
callsheet version
}
```

Expected output is `callsheet <selected-tag> protocol=<number>`; a different version usually means a different executable on PATH. After leaving the child bash, run `export PATH="$HOME/.local/bin:$PATH"` in the original shell and in each tutorial terminal; persisting it in a shell profile is your choice. Never run later install blocks in the parent after an error returned you there.

Do not use sudo or pipe a download into a shell, and do not remove macOS quarantine automatically. If macOS blocks the binary, record that as a snag and inspect it through the OS controls: releases are not signed or notarized. Checksums protect integrity relative to the release manifest, not publisher authenticity.

### Source-build fallback

This path is available before the first release and for contributors. Get a source checkout from the [repository](https://github.com/wedevwork/callsheet) (a browser download of the source archive is enough; git is not required). Install the Go version named in [go.mod](../go.mod), then run this from the checkout's root:

<!-- quickstart: source-build -->
```bash
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/callsheet" ./cmd/callsheet
export PATH="$HOME/.local/bin:$PATH"
callsheet version
```

Source builds print `dev`, not a released version. The release path needs no Go toolchain.

## Prerequisites

One real vendor CLI can be both coordinator and worker; this tutorial uses Claude Code to avoid two installations. Install and authenticate it as the **same OS user running the sidecar**, select a model available to that account, and inspect its effective permission configuration. Readiness checks version and invocability, **not login, entitlement, model availability or useful headless tool execution**. Use the model from your already working local setup as `MODEL`; no model default is promised. The tutorial's `--effort low` is explicit and allowed for Claude.

Current details live in the references rather than here:

- [Selection and versions](real-adapters.md#selection-and-versions): the four runtime minima and allowed effort sets. A minimum is eligibility, not a guarantee of compatibility with later versions.
- [Current worker recipes](real-adapters.md#current-worker-recipes): the automatic modes, Claude `--permission-mode auto`, Codex `--approve-for-me`, Grok `--permission-mode auto` and Cursor `--trust --auto-review`. These are Callsheet's adapter policies, not commands you append to `sidecar run`. The account and tool permissions a worker needs must already work headlessly; Callsheet does not edit configuration or broaden permissions after a denial.
- [Enabling a worker](real-adapters.md#enabling-a-worker): an absolute executable path on every sidecar start; an omitted adapter is disabled. Authentication stays on the worker and is never copied with plane state.
- [Support catalog](support-catalog.md): observed evidence, the minimum-versus-captured-version distinction and unverified facts. Grok useful auto tools and Cursor model-effort encoding need their documented operator probes. Grok and Cursor workers are Linux-only; Claude and Codex macOS workers can be enabled explicitly but vendor sandbox and exit behavior are unverified. Installing a macOS binary does not qualify a vendor worker.
- [Coordinator setup](coordinator.md): the selected coordinator's own authentication, MCP authorization and runbook-loading recipe. A different coordinator CLI can use the same plane. No live qualification harness is a prerequisite.

Use a trusted network and a dedicated OS user; TLS provides no client authorization (see [Security](../README.md#security) and the deployment [security posture](deploy.md#security-posture)). For first use choose a fresh nonsensitive plain folder and do not seed a real or private repository. No shell example needs a vendor token or credential.

## One machine

Use three terminals: **A** runs the plane, **B** the sidecar, **C** is your operator and coordinator terminal. Nothing is daemonized or backgrounded. Variables do not travel between terminals, so each one defines its own. Stay outside any existing git repository. Paths may contain spaces; keep the quotes.

In terminal A, create the experiment folder. `mkdir` fails if it already exists: do not overwrite an earlier experiment.

<!-- quickstart: env-plane -->
```sh
QS="$HOME/callsheet-quickstart"
export PATH="$HOME/.local/bin:$PATH"
mkdir "$QS"
```

Initialize the plane. It prints the CA certificate path and fingerprint. Never share `pki/ca.key` or `server.key`.

<!-- quickstart: plane-init -->
```sh
callsheet plane init --state-dir "$QS/plane" --bind 127.0.0.1:8443 --san 127.0.0.1
```

Run it and leave it in the foreground; it logs that it is listening on stderr. On a port collision, inspect the existing process or start a fresh experiment with a different port in both the init command and `PLANE`; do not delete somebody's state.

<!-- quickstart: plane-run -->
```sh
callsheet plane run --state-dir "$QS/plane"
```

In terminal B:

<!-- quickstart: env-sidecar -->
```sh
QS="$HOME/callsheet-quickstart"
export PATH="$HOME/.local/bin:$PATH"
PLANE=https://127.0.0.1:8443
CA="$QS/plane/pki/ca.crt"
```

Enroll, and copy the printed `node_id` for terminal C.

<!-- quickstart: sidecar-enroll -->
```sh
callsheet sidecar enroll --state-dir "$QS/sidecar" --plane "$PLANE" --ca "$CA"
```

Enter the absolute path of your installed Claude executable and check it against the vendor setup guide. A shell alias is not a worker executable.

<!-- quickstart: worker-path -->
```sh
read -r WORKER
"$WORKER" --version
```

Run the sidecar and leave it in the foreground:

<!-- quickstart: sidecar-run -->
```sh
callsheet sidecar run --state-dir "$QS/sidecar" --claude-adapter "$WORKER"
```

In terminal C:

<!-- quickstart: env-operator -->
```sh
QS="$HOME/callsheet-quickstart"
export PATH="$HOME/.local/bin:$PATH"
PLANE=https://127.0.0.1:8443
CA="$QS/plane/pki/ca.crt"
```

Write the worker manuals and the seed folder. These are worker manuals, not the coordinator runbook; they live outside the seed, and registration reads them on the worker.

<!-- quickstart: manuals-and-seed -->
```sh
mkdir "$QS/manuals"
mkdir "$QS/seed"
printf 'Work only in the task checkout. Follow the goal, preserve existing files, and report what you changed.\n' > "$QS/manuals/instruction.md"
printf 'Read README.md. Create the requested file, then check its exact contents. Do not install dependencies or access external services.\n' > "$QS/manuals/runbook.md"
printf 'Callsheet quickstart scratch project.\n' > "$QS/seed/README.md"
```

Enter the `node_id` from enrollment, then a model name your authenticated account can use:

<!-- quickstart: node-and-model -->
```sh
read -r NODE
read -r MODEL
```

Confirm the node is online:

<!-- quickstart: node-list -->
```sh
callsheet node ls --plane "$PLANE" --ca "$CA"
```

Register the role:

<!-- quickstart: role-add -->
```sh
callsheet role add quickstart-local --name quickstart --node "$NODE" --adapter claude \
  --instruction "$QS/manuals/instruction.md" --runbook "$QS/manuals/runbook.md" \
  --model "$MODEL" --effort low --concurrency 1 --timeout 5m --plane "$PLANE" --ca "$CA"
```

Confirm it can accept work:

<!-- quickstart: role-list -->
```sh
callsheet role ls --plane "$PLANE" --ca "$CA"
```

Expect the node online and the role's `CAN_ACCEPT` true. If not, resolve that before dispatching. Never retry `role add` blindly on a conflict: existing IDs or state are not a clean initial setup.

### Configure your coordinator

Before the first task, configure an interactive coordinator using the [Claude Code section of the coordinator guide](coordinator.md#claude-code). Supply the absolute installed `callsheet` path, `PLANE` and `CA`; the guide owns the registration and launch syntax. This small runbook is the coordinator's:

```text
Use the Callsheet MCP server to discover nodes and roles. Do not dispatch until I ask. Record every admitted task ID; never redispatch after a lost answer. For this quickstart I will run the first dispatch and wait in my operator terminal so I can see the exact commands. Inspect its task ID when I give it to you. Later tasks may use MCP dispatch and short task_wait polls; follow the coordinator guide for harness-managed background waits. Check state, final message and workspace publication before declaring completion.
```

Write it to `$QS/coordinator.md`:

<!-- quickstart: coordinator-runbook -->
```sh
printf '%s\n' 'Use the Callsheet MCP server to discover nodes and roles. Do not dispatch until I ask. Record every admitted task ID; never redispatch after a lost answer. For this quickstart I will run the first dispatch and wait in my operator terminal so I can see the exact commands. Inspect its task ID when I give it to you. Later tasks may use MCP dispatch and short task_wait polls; follow the coordinator guide for harness-managed background waits. Check state, final message and workspace publication before declaring completion.' > "$QS/coordinator.md"
```

Launch the coordinator with that runbook using the linked recipe and ask it to list nodes and roles with Callsheet MCP. It must see `quickstart-local`. This establishes the real coordinator connection while the first write stays reproducible in terminal C. Do not run a second dispatch from the coordinator; it observes this first task. This does not promise a vendor wake or raise the default MCP wait budget. Where a sandboxed vendor shell cannot reach host loopback, use the human operator terminal for this path and follow the coordinator guide; do not disable sandboxing.

### First workspace task

Stay in terminal C. Create the workspace:

<!-- quickstart: workspace-create -->
```sh
callsheet ws create --plane "$PLANE" --ca "$CA" quickstart
```

Enter the printed instance:

<!-- quickstart: read-instance -->
```sh
read -r INSTANCE
```

Push the seed folder:

<!-- quickstart: workspace-push -->
```sh
callsheet ws push --instance "$INSTANCE" --plane "$PLANE" --ca "$CA" quickstart "$QS/seed"
```

Dispatch the task:

<!-- quickstart: dispatch -->
```sh
callsheet dispatch --role-id quickstart-local \
  --goal "Create hello.txt containing exactly 'hello from Callsheet' followed by a newline. Preserve README.md." \
  --acceptance "hello.txt has the requested single line and README.md is unchanged" \
  --workspace quickstart --base main --workspace-instance "$INSTANCE" --plane "$PLANE" --ca "$CA"
```

Record the admission time and enter the printed `task_id`. Do not infer an ID or dispatch again if the response is lost; consult the task listing and recovery guidance in the [coordinator guide](coordinator.md).

<!-- quickstart: read-task -->
```sh
read -r TASK
```

Wait in this human shell, not in a blocking model turn:

<!-- quickstart: wait -->
```sh
callsheet task wait "$TASK" --until-done --plane "$PLANE" --ca "$CA"
```

It is silent until the task is terminal and retries an unavailable plane indefinitely. Ctrl-C stops the watcher, not the task; diagnose the plane or node rather than dispatching again. Its exit 0 means a terminal answer, not task success. Check the task:

<!-- quickstart: task-show -->
```sh
callsheet task show "$TASK" --lines 0 --plane "$PLANE" --ca "$CA"
```

Check its publication:

<!-- quickstart: workspace-status -->
```sh
callsheet ws status --plane "$PLANE" --ca "$CA" "$TASK"
```

Require state `succeeded`, the expected final message, workspace publication `published` and `available: true`. The acceptance string is an instruction only; Callsheet does not evaluate it. Record the completion time, then ask the coordinator to inspect this `TASK` through MCP `task_show` as a read-only follow-up.

Pull the result. The delivery folder must be absent or empty:

<!-- quickstart: pull -->
```sh
callsheet ws pull --plane "$PLANE" --ca "$CA" "$TASK" "$QS/delivery"
```

Inspect it:

<!-- quickstart: inspect-result -->
```sh
cat "$QS/delivery/hello.txt"
cmp "$QS/seed/README.md" "$QS/delivery/README.md"
```

Expect the single line `hello from Callsheet` and no `cmp` output. A plain-folder pull exports files; it does not overwrite a dirty git checkout or push to GitHub. See [workspaces](workspaces.md) for other pull modes. This completes the first path.

To stop, after the task completes: Ctrl-C the coordinator session, then the foreground sidecar in B, then the plane in A, and wait for each foreground command to return. State persists under `$QS`; there is no destructive cleanup step.

## Add a second machine

Here **machine A** and **machine B** are distinct computers, unlike the terminal labels above. Keep machine A as plane and coordinator and add machine B as a worker. This extends a successful, idle one-machine setup: no tasks in flight. Install the **same Callsheet version** on machine B using [Install Callsheet](#install-callsheet), authenticate the worker there and prepare its automatic-mode prerequisites.

Both machines use a private LAN or VPN address of machine A, never 127.0.0.1 or 0.0.0.0. Allow only trusted-network peers to reach that port; the sidecar opens no inbound listener.

`plane run --bind NEW` is not a configuration update: the plane refuses a bind that differs from its initialized one. So change it while stopped. This is the tutorial's teaching example; [Change the plane address](deploy.md#change-the-plane-address) is the canonical maintenance procedure.

1. On machine A stop the local sidecar and plane foreground commands and wait for them to exit. Do not delete or reinitialize state.
2. With the plane stopped, edit **only** `bind` in `$QS/plane/config.json` to `PLANE_IP:8443`, using the actual private address assigned to machine A, leaving `schema_version` 1 and the file's ownership and mode intact. This illustrative example uses `192.168.1.10`; use your real private address. Before:

   ```json
   { "schema_version": 1, "bind": "127.0.0.1:8443" }
   ```

   After:

   ```json
   { "schema_version": 1, "bind": "192.168.1.10:8443" }
   ```

3. Enter that address and reissue the server certificate. The complete list keeps the original SAN and adds the new address; the CA is unchanged.

   <!-- quickstart: remote-address -->
   ```sh
   read -r PLANE_IP
   ```

   <!-- quickstart: remote-cert -->
   ```sh
   callsheet plane cert reissue --state-dir "$QS/plane" --san 127.0.0.1 --san "$PLANE_IP"
   ```

4. Restart the plane in its foreground terminal. It now listens at the private address only.

   <!-- quickstart: remote-plane-run -->
   ```sh
   callsheet plane run --state-dir "$QS/plane"
   ```

5. All clients now use the new URL. In each machine A terminal, enter the address and set it; the original CA stays.

   <!-- quickstart: remote-url -->
   ```sh
   read -r PLANE_IP
   PLANE="https://$PLANE_IP:8443"
   ```

   Re-enroll the local sidecar in its **same** state directory. Its node ID remains unchanged.

   <!-- quickstart: local-reenroll -->
   ```sh
   callsheet sidecar enroll --state-dir "$QS/sidecar" --plane "$PLANE" --ca "$CA"
   ```

   Restart it with the same absolute worker path:

   <!-- quickstart: local-restart -->
   ```sh
   callsheet sidecar run --state-dir "$QS/sidecar" --claude-adapter "$WORKER"
   ```

   Update the plane URL in the vendor-owned coordinator MCP configuration as the [coordinator guide](coordinator.md) describes and restart the coordinator. Do not add a second server name.

6. Copy only `$QS/plane/pki/ca.crt` from machine A to machine B through your trusted transfer channel, and compare its fingerprint against the init report out of band if needed. Never copy plane state, `ca.key`, `server.key`, sidecar state, node identity or vendor credentials. A manual secure transfer is sufficient; SSH is not assumed.

7. On machine B create a fresh folder outside any git repository and store the certificate in it as `$QS/plane-ca.crt`:

   <!-- quickstart: remote-env -->
   ```sh
   QS="$HOME/callsheet-quickstart-remote"
   export PATH="$HOME/.local/bin:$PATH"
   mkdir "$QS"
   read -r PLANE_IP
   PLANE="https://$PLANE_IP:8443"
   CA="$QS/plane-ca.crt"
   ```

   Write the same two worker manuals:

   <!-- quickstart: remote-manuals -->
   ```sh
   mkdir "$QS/manuals"
   printf 'Work only in the task checkout. Follow the goal, preserve existing files, and report what you changed.\n' > "$QS/manuals/instruction.md"
   printf 'Read README.md. Create the requested file, then check its exact contents. Do not install dependencies or access external services.\n' > "$QS/manuals/runbook.md"
   ```

   Enroll, and keep the printed `node_id`: it is machine B's own, distinct node ID. Never copy machine A's.

   <!-- quickstart: remote-enroll -->
   ```sh
   callsheet sidecar enroll --state-dir "$QS/sidecar" --plane "$PLANE" --ca "$CA"
   ```

   Enter machine B's own absolute Claude path and run the sidecar in a foreground terminal:

   <!-- quickstart: remote-worker-path -->
   ```sh
   read -r WORKER
   ```

   <!-- quickstart: remote-sidecar-run -->
   ```sh
   callsheet sidecar run --state-dir "$QS/sidecar" --claude-adapter "$WORKER"
   ```

8. In a machine B operator terminal with the same variables, enter that node ID and a model for machine B's account, then register the remote role:

   <!-- quickstart: remote-node-and-model -->
   ```sh
   read -r REMOTE_NODE
   read -r MODEL
   ```

   <!-- quickstart: remote-role-add -->
   ```sh
   callsheet role add quickstart-remote --name quickstart --node "$REMOTE_NODE" --adapter claude \
     --instruction "$QS/manuals/instruction.md" --runbook "$QS/manuals/runbook.md" \
     --model "$MODEL" --effort low --concurrency 1 --timeout 5m --plane "$PLANE" --ca "$CA"
   ```

   From machine A, verify the remote node is online and its role can accept:

   <!-- quickstart: remote-roster -->
   ```sh
   callsheet node ls --plane "$PLANE" --ca "$CA"
   callsheet role ls --plane "$PLANE" --ca "$CA"
   ```

9. Back on machine A dispatch the identical workspace goal to the remote role. Use `--role-id`: `--role-name` could select the original local node and is forbidden here.

   <!-- quickstart: remote-dispatch -->
   ```sh
   callsheet dispatch --role-id quickstart-remote \
     --goal "Create hello.txt containing exactly 'hello from Callsheet' followed by a newline. Preserve README.md." \
     --acceptance "hello.txt has the requested single line and README.md is unchanged" \
     --workspace quickstart --base main --workspace-instance "$INSTANCE" --plane "$PLANE" --ca "$CA"
   ```

   Enter the printed `task_id`, then wait, inspect and check publication, confirming that the returned node is machine B's:

   <!-- quickstart: remote-read-task -->
   ```sh
   read -r REMOTE_TASK
   ```

   <!-- quickstart: remote-wait -->
   ```sh
   callsheet task wait "$REMOTE_TASK" --until-done --plane "$PLANE" --ca "$CA"
   ```

   <!-- quickstart: remote-show -->
   ```sh
   callsheet task show "$REMOTE_TASK" --lines 0 --plane "$PLANE" --ca "$CA"
   ```

   <!-- quickstart: remote-status -->
   ```sh
   callsheet ws status --plane "$PLANE" --ca "$CA" "$REMOTE_TASK"
   ```

   Pull on machine A. The original seed and the earlier delivery stay as they are.

   <!-- quickstart: remote-pull -->
   ```sh
   callsheet ws pull --plane "$PLANE" --ca "$CA" "$REMOTE_TASK" "$QS/delivery-remote"
   ```

The plane alone stores workspace history: no manual transfer of seed, results or vendor authentication is needed.

## If something goes wrong

| Symptom | Check |
|---|---|
| Download 404 | Confirm the selected tag and asset exist on the releases page. No release yet means the source-build fallback. |
| Checksum mismatch | Stop and discard the bad download. Verify the tag and asset pair and download both files again from the same release. Never chmod or run it anyway. |
| TLS or trust failure | The URL's hostname or IP must match a SAN, the correct CA file must exist and the system clock must be right. Never use an insecure flag or copy a key. |
| Offline or no capacity | Check the plane and sidecar terminals are alive, the node is correct and the role's `CAN_ACCEPT` and concurrency allow work. There is no queue or automatic reroute. |
| Unready worker | Check the absolute executable flag on this start, the runtime minimum and readable manuals. Readiness does not prove login. |
| Failed or refused task | Read `task show` and `task logs`. Authenticate the same OS user and check model entitlement and automatic-mode policy in the [adapters guide](real-adapters.md). A succeeded process may still hold refusal prose or lack the desired edit. |
| `timed_out` | The tutorial role has a 5m execution deadline, and a slow first vendor turn may reach it without a worker malfunction. Inspect partial output and publication. For an intentional new attempt pass a larger explicit dispatch `--timeout` (for example 10m), or change the role timeout through the role reference. Do not relabel this state failed or retry automatically; check the prior task before starting more work. |
| Pull unavailable | Inspect publication and the `available` flag; a terminal exit is not sufficient. A missing, pruned or failed publication is not repaired by pulling main. A nonempty delivery folder needs a new directory. |
| Silent wait | Check the plane. Ctrl-C stops observation only. Never duplicate work on a missing reply. |

Read a task's logs:

<!-- quickstart: diagnostics -->
```sh
callsheet task logs "$TASK" --plane "$PLANE" --ca "$CA"
```

The node, role and task commands above are reusable for diagnosis.

## Next steps

- [Coordinator setup](coordinator.md): other coordinators, runbooks and background waits.
- [Workspaces](workspaces.md): repositories, pull modes and retention.
- [Development-flow example](examples/dev-flow/README.md): a reusable starting point.
- [Deployment](deploy.md): sustained operation, services, address changes and recovery.
