# Workspaces: local push and pull

The plane hosts named workspaces: durable git hubs over its verified TLS endpoint (iteration 09a: `ws create`, `ls`, `show`, `rm`, `prune`, `status`, `diff`, `ref set`). Iteration 09b adds the two local transfers, `ws push` and `ws pull`, on the CLI and as the MCP tools `ws_push` and `ws_pull`. Iteration 10c adds workspace dispatch on the CLI and MCP, the task-ID forms of pull, status and diff, and the [coordinator workflow](#coordinator-workflow) for chaining tasks. They run on the machine where the command or `callsheet mcp` runs: push reads local files, pull writes them. Only paths, selectors and the result metadata below cross the CLI or MCP boundary: never file contents, file lists, commit messages, absolute local paths, credentials or pack diagnostics.

go-git v5.16.3 is embedded and is the only git engine. No `git` executable is needed or run, and no hook, filter, LFS process, remote helper, credential helper, SSH agent or persisted remote is ever used. Linux and macOS are supported.

## Commands

```
callsheet ws push --instance TOKEN [--branch BRANCH] --plane URL (--ca FILE | --ca-fingerprint SHA256) [--json] NAME [PATH]
callsheet ws pull --plane URL (--ca FILE | --ca-fingerprint SHA256) [--json] (TASK_ID | NAME REF) [PATH]
```

- `NAME` is an existing workspace; nothing is created or inferred. Flags come before the operands.
- `TASK_ID` (iteration 10c) pulls that task's published result: the task's own record selects its bound workspace, instance and immutable result commit (see [Task results by task ID](#task-results-by-task-id)). A first operand that is a task ID selects this form (workspace names cannot contain `_`); a malformed operand starting `t_` is `invalid_argument`, never a workspace name.
- `PATH` defaults to the current directory. A relative path resolves against the process working directory (for `ws_push`/`ws_pull`, the `callsheet mcp` server's working directory when the call is admitted), never against a path a model supplies. There is no `~` or variable expansion; an empty path, invalid UTF-8, NUL or a path over the native limit (4095 bytes on Linux, 1023 on macOS) is `invalid_argument`.
- `push` needs `--instance` (from `ws show`), like `rm`, `prune` and `ref set`. `--branch` is the target branch, short (`topic/x`) or `refs/heads/...`, default `main`, with the 09a portable name rules (lowercase ASCII, components of 1-200 bytes, at most 512 bytes as a full ref).
- `pull` needs no instance: it records the instance it observed and checks it again before anything local is published. `REF` is a branch (short or `refs/heads/...`), a full 40-hex commit hash reachable from a workspace ref, a complete `refs/callsheet/tasks/t_<32 hex>` ref (created by workspace tasks since iteration 10b, see [Workspace tasks](#workspace-tasks)), or, since iteration 10c, a bare `t_<32 hex>` task ID meaning that complete task ref. A branch named like a task ID is selected as `refs/heads/t_...`. A complete task ref keeps its 09b meaning (also for a ref planted without a task record); the `NAME REF` form never looks the task up. There are no short hashes, revision expressions or task-name lookups.
- There is no `--force`, `--commit` or remote option.

MCP tools (after the eight 09a workspace tools; 23 tools in all):

| Tool | Accepted forms (exactly one) | Optional |
|---|---|---|
| `ws_push` | `name`, `instance` | `branch` (default main), `path` (default: the server's working directory) |
| `ws_pull` | `{name, ref}` or `{task_id}` | `path` in both forms (default: the server's working directory) |

An omitted optional argument differs from `null` or `""`, which are invalid. Both tools are mutations (`readOnlyHint:false`, `destructiveHint:false`, `idempotentHint:false`) and are never retried automatically. `ws_pull`, `ws_status` and `ws_diff` keep one flat input schema each (the union of their two forms' properties, nothing required at the top level): exactly one complete form, no mixed keys and the all-or-none `after`/`instance`/`generation` continuation are checked when the call runs, and a violation is `invalid_argument` before anything is contacted.

## Results

`--json` prints one object and a newline; text prints the same fields as `key: value` lines in this order, `none` for null and lowercase booleans.

| Operation | Fields |
|---|---|
| push | `name`, `instance`, `branch` (the full ref), `old_commit` (null when the branch was created), `commit`, `source_kind` (`git` or `folder`), `changed` |
| pull | `name`, `instance`, `selector` (the full ref or hash), `commit`, `destination_kind` (`git` or `folder`), `local_ref` and `old_commit` (null for a folder), `changed` |

`changed` means a ref moved or a folder was published. A repeated push of the same commit, or a pull whose Callsheet ref already has the commit, is `changed: false` after the same compare-and-swap check. The result makes no claim about the workspace's generation after the push.

## Pushing a git repository

`PATH` must be the root of an ordinary non-bare repository with a real `.git` directory. The push sends the committed `HEAD` (detached is fine; an unborn `HEAD` says "commit first") and its complete history with the exact commit IDs, authors, messages and trees: tracked files that are now ignored and every historical version travel too. Tags, other local branches and unreachable objects do not. The selected source branch does not choose the target branch.

The source must be clean, or the push is refused with "source has uncommitted or untracked non-ignored changes; commit first" (conflict, exit 4). Cleanliness compares normalized content: a tracked file whose working-tree bytes clean, through the effective `core.autocrlf`, `text` and `eol` rules, to exactly the indexed blob is clean, even when `git status` would list it as modified for line-ending or stat-only reasons (for example LF committed and CRLF on disk). Callsheet never rewrites your files, and a folder export writes the stored blob bytes. If you want one line-ending format everywhere, put `* text=auto eol=lf` in `.gitattributes` and run `git add --renormalize .` once. Dirty means any staged difference (HEAD tree against the index, including a staged change undone in the working tree and intent-to-add entries), any unstaged difference of a tracked file (content, deletion, type, and the executable bit when `core.fileMode` is true), or any nonignored untracked file or symlink. Ignored untracked files, empty directories and sockets or FIFOs that are not tracked are not dirt. Callsheet computes this itself: every tracked file is hashed (no stat-cache shortcut) in Git's clean form, and nothing is written (no index refresh).

| Setting | Behavior |
|---|---|
| `core.fileMode` | true (default): an owner-execute bit change is dirty; false: ignored. Other permission bits never matter; type changes are always dirty |
| `core.symlinks` | true (default): link text compared, never followed (broken links are fine); false: a regular file holding the link text matches |
| `core.autocrlf`, `core.eol`, `text`, `crlf`, `eol`, `ident` attributes | Git's check-in conversion: CRLF to LF for text (with `text=auto`'s binary detection and its "CRLF already in the index" safeguard), `$Id: ...$` collapsed to `$Id$`; bare CRs and binary data are preserved |
| attribute sources | `.git/info/attributes` > nearest `.gitattributes` (index copy when the working-tree file is missing) > parent directories > `core.attributesFile` (default `$XDG_CONFIG_HOME/git/attributes` or `~/.config/git/attributes`) > `/etc/gitattributes` |
| `core.ignoreCase` | names relate to index paths with ASCII case folding; two names that differ only by case are refused |
| `core.precomposeUnicode` | macOS only, when effective true: names read from the working tree are compared in NFC; index and HEAD paths and ignore patterns stay as stored bytes. An NFC index path with an NFD file on disk is clean; an NFD index path with an NFD file on disk is a deletion plus an untracked file (dirty), as `git status` reports. Unset is false; Linux is always byte-exact |

Configuration is read with Git's precedence: the system file `/etc/gitconfig` (unless `GIT_CONFIG_NOSYSTEM`; `GIT_CONFIG_SYSTEM` replaces its path), `$XDG_CONFIG_HOME/git/config` or `~/.config/git/config`, then `~/.gitconfig` (`GIT_CONFIG_GLOBAL` replaces both), the repository's `.git/config`, then `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`. `include.path` is followed (relative to the including file, `~/` from `HOME`) with cycle detection; `includeIf` supports `gitdir:`, `gitdir/i:` and `onbranch:`. Other conditions (for example `hasconfig:`), `GIT_CONFIG_PARAMETERS` and unreadable or unparsable files are refused as `unsupported_repository`; nothing is executed. Only the status and ignore settings are used; credentials are never read.

Before the pack is sent, `HEAD`, the index entries, the configuration files and every file and directory that was read are checked again; any change is refused with `source_changed` (conflict). Keep the source quiescent while pushing: this detects ordinary edits, not an adversarial concurrent writer.

## Pushing a plain folder

A directory that is not in a repository (no `.git` in it or in any parent, no bare layout) is snapshotted:

- Regular files keep their exact bytes; mode 100755 if any execute bit is set, else 100644. Owners, other permission bits, ACLs, xattrs and times are ignored. Hard links are separate entries.
- Symlinks store their link text and are never followed (absolute or dangling targets are data).
- Empty directories are omitted. Names are the raw bytes the filesystem returns, never normalized.
- A `.git` entry anywhere is a nested repository and refuses the push (`unsupported_repository`); an ignored directory is not read at all. A socket, FIFO or device that is not ignored refuses the push (`unsafe_tree`); an ignored one is skipped without being opened.

The snapshot is one commit with fixed identity: author and committer `Callsheet <workspace@callsheet.invalid>` at Unix time 0 UTC, message `Callsheet workspace snapshot`, no signature. Its single parent is the target branch's tip as observed (none for a new branch), so the same parent and tree always give the same commit ID, independent of time, user, machine or path. The snapshot is the whole folder: a file missing from the folder is missing from the snapshot (this is not a merge). If the tree equals the tip's tree, no commit is made and the push is a same-value compare-and-swap with `changed: false`. An empty folder on a new branch is the empty-tree root commit.

Every folder push fetches the target branch's complete history into a fresh private temporary store to build the parent (there is no persistent local cache), including unchanged no-op pushes: inbound transfer and verification are proportional to the branch's reachable history, and only the outbound pack is incremental.

## Ignore rules

Two separate policies:

| Source | Rules, lowest to highest priority |
|---|---|
| Plain folder | the root `.gitignore`, then deeper `.gitignore` files only. No `.git/info/exclude`, no global excludes, no `/etc/gitignore`: `HOME`, `XDG_CONFIG_HOME` and `core.excludesFile` never change a snapshot. Byte-exact matching. |
| Git repository (untracked paths only) | the global excludes file (`core.excludesFile`, default `$XDG_CONFIG_HOME/git/ignore` or `~/.config/git/ignore`; an explicit empty value disables it), then `.git/info/exclude`, then the root `.gitignore`, then deeper `.gitignore` files. Tracked files are never filtered. |

Within a file the last matching line wins; a deeper file wins over a shallower one. The syntax is Git's: `#` comments, `\#` and `\!` escapes, trailing spaces ignored unless escaped, CRLF files, `?` and `*` not crossing `/`, bracket expressions with ranges, negation and POSIX classes (`[[:digit:]]`), a leading `/` anchors, a pattern with a `/` is relative to its directory, a pattern without one matches at any depth, a trailing `/` matches directories only, `**/`, `/**` and `/**/` match zero or more directories, and `!` re-includes. An excluded directory is pruned and nothing below it can be re-included:

- `dir/` plus `!dir/keep`: `dir/keep` stays excluded (its parent is excluded).
- `dir/*` plus `!dir/keep`: `dir/keep` is included (`dir` itself stays traversable).

The root `.gitignore` is read before the root's entries are judged, even when a rule ignores `.gitignore` files as content. Symlinked `.gitignore` and `.gitattributes` files are not read as rules. Missing global or info files are empty; unreadable ones refuse the push.

## Supported repositories

| Layout or feature | Push | Pull into it |
|---|---|---|
| Ordinary non-bare repository, `PATH` at its root | yes | yes |
| A subdirectory of a repository (or a folder inside one) | refused: "use the repository root" | refused |
| `.git` file (linked worktree, submodule, separate git dir), symlinked `.git`, bare repository | refused | refused |
| Shallow, partial or promisor clones, alternate object stores, SHA-256, at repository format version 1 reftable or any extension Git 2.43 does not know (at version 0 Git ignores unknown extensions, and so does Callsheet), `core.worktree` or a true `core.bare` in `.git/config`, `extensions.worktreeConfig` | refused | refused |
| Sparse checkout, sparse or split index, skip-worktree or assume-unchanged entries, unresolved merge stages | refused | refused |
| Submodules (gitlinks, configured `submodule.<name>.*` entries (general settings such as `submodule.recurse` are fine), `.gitmodules` sections, `.git/modules`), Git LFS (a configured LFS filter that any attributes file Git reads routes paths to: system, global or `core.attributesFile`, `.git/info/attributes`, or a `.gitattributes` at any depth of the working tree or tracked in the index; or a pointer in the history) | refused | refused |
| Active external filters, `working-tree-encoding`, user attribute macros that change conversions, a redefined `binary` macro that changes its conversion (Git honors `[attr]binary ...`) | refused | not needed (no filter runs) |
| `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_COMMON_DIR` naming another location; `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_NAMESPACE` | refused | refused |

Repository metadata is read with Git's own rules, never trimmed into validity, and every value that makes `git status` fail makes the transfer refuse the repository (`unsupported_repository`):

- **Configuration.** Every key Git 2.43 validates when it reads its configuration is checked with Git's parser for that key, in every file Git reads (system, global, the repository's `.git/config`, includes) and at every occurrence, even one a later value overrides: booleans (`git_config_bool`, so a trailing `\v` or `\f` fails), integers with their ranges and `k`/`m`/`g` units, keywords such as `core.autocrlf=input`, `core.safecrlf=warn` or `push.default`, color specifications, `column.*`, `core.sharedRepository` permissions and the keys that require a value. Keys Git does not validate are never refused for their value. The repository format (`core.repositoryFormatVersion`, `extensions.*`, `core.worktree`, a true `core.bare`) counts only in `.git/config`, where Git reads it; an unknown `core.eol` is unset, as in Git. The rules come from checked-in oracles of Git 2.43.0: every key `git help --config` lists (and the repository extensions) probed with malformed values, and a grammar fuzz of every keyword, color and list parser (keywords joined, prefixed and suffixed by every ASCII byte, truncated and extended); developers regenerate them with `go test -tags=gitoracle -run '^TestGenerateGit' ./internal/workspacetransfer`. For example `diff.wsErrorHighlight` takes keywords separated by commas only (`new,old`; `newxold` or `new;old` fails).
- **Refs.** `HEAD` and loose refs drop only trailing space, tab, CR and LF. Every `packed-refs` record must follow Git's grammar: an optional `# pack-refs with:` first line, then `<40 hex> <space, tab, CR or LF><name>` lines of at least 42 bytes, each optionally followed by one `^<40 hex>` line, every line LF-terminated. A name Git rejects but considers safe (for example a trailing CR or space) is ignored, as Git ignores a broken ref; an empty or dangerous name (`//`, `.` or `..` components, a leading or trailing slash, per Git's `refname_is_safe`) refuses the repository. Git checks a record only when a lookup or iteration reaches it; Callsheet checks every record, so it also refuses the few malformed records `git status` happens not to reach.
- **Replacement refs.** Unless `GIT_NO_REPLACE_OBJECTS` is set or `core.useReplaceRefs` is false, Git replaces objects it reads by the refs under `refs/replace/` (loose or packed; a ref names the object it replaces by the first 40 hexadecimal digits of its last path component). `GIT_REPLACE_REF_BASE`, which relocates them, is refused whenever it is set, to any value including the empty string (Git treats an empty base as every ref and aborts on a complete ref name). Callsheet transfers and compares the original objects, so it refuses any active replacement of HEAD's commit or of its trees (Git fails when such a replacement is missing, broken, of the wrong type or cyclic, and shows a different commit when it is valid) and two refs replacing the same object (Git fails). Replacements of other objects are accepted, as Git accepts them.
- **Other files and state.** Any `shallow` file (an empty one makes Git treat the repository as shallow), a shallow file named by `GIT_SHALLOW_FILE`, any alternates entry (a line that is neither empty nor a comment), a `commondir`, a promisor pack and `GIT_NAMESPACE` are refused. Operation state (`MERGE_HEAD`, the stash and its reflog, `info/grafts`, other refs, an upstream, linked worktrees' files, `.keep` files, unreadable packs, the `commit-graph` chain) is not read and Git does not fail on it either; a corrupt HEAD commit fails both.
- **Where Git is lenient, Callsheet refuses:** `GIT_REPLACE_REF_BASE` set to any value, a NUL byte in a ref, a packed record or a parsed value (Git truncates the C string), an alternates entry Git would skip as a missing directory, a negative format version, a key outside any section (Git warns and ignores it), and an unparsable `.gitmodules`.

A repository around a folder is found the way Git's discovery finds one: an ancestor whose `.git` is a valid git directory (a `HEAD` holding `ref: refs/...` or a full hash, plus `objects/` and `refs/`, through `commondir` when present), an ancestor with a `.git` file, or an ancestor that is itself a git directory. An empty or invalid `.git` directory in an ancestor (for example a sandbox's read-only stub) is not a repository, and the search continues upward as Git's does.

Every refusal is `invalid_argument` with `details.reason` `unsupported_repository` and names the feature; none is reported as a dirty source and none falls back to treating the directory as a plain folder.

## Fast-forward only

The push observes the target branch's tip once (the receive-pack advertisement, with the instance header) and sends exactly one command with that observed value as the compare-and-swap old value and the selected commit as the new one. A tip that is not the commit or one of its ancestors (all parents are searched) is refused before anything is sent: "push would replace history; push to a new branch, then explicitly move the target with ws ref set" (conflict, `non_fast_forward`). There is no force and no automatic `ref set`. A branch moved by someone else after the observation fails the plane's compare-and-swap (conflict); nothing is refreshed or retried. Success is only the plane's durable report-status, never an HTTP 200 alone.

Pushing the tip the branch already has sends the same compare-and-swap with a valid zero-object pack (`PACK`, version 2, zero objects, trailer). This encoding is proven against the production receive handler and is the one that ships; the one-object fallback was not needed.

If the answer to a push is lost, the push may have succeeded: inspect with `callsheet ws status` or `ws show` before pushing again. Callsheet never retries a push.

## Pulling into a git repository

`PATH` is the root of an existing ordinary repository; uncommitted changes are fine and stay untouched. The pull installs the verified objects in `.git/objects` and sets exactly one Callsheet ref:

| Selected | Local ref |
|---|---|
| `refs/heads/BRANCH` | `refs/callsheet/NAME/heads/BRANCH` |
| `refs/callsheet/tasks/TASK_ID` | `refs/callsheet/NAME/tasks/TASK_ID` |
| full commit hash `HASH` | `refs/callsheet/NAME/commits/HASH` |

Checkout, index, `HEAD`, branches, tags, config, remotes, `FETCH_HEAD` and reflogs are never changed; no checkout, reset, stash, filter or hook runs. Branch and task observation refs move in either direction (they are a cache of your explicit pull, not a branch you work on) under a local compare-and-swap: the value observed before the download is compared again under git-compatible locks (`packed-refs.lock`, then `<ref>.lock`, both created exclusively); an existing lock or a changed value is `local_ref_conflict` and nothing is retried. A symbolic Callsheet ref, a `HEAD`, linked-worktree `HEAD` or branch symbolic chain that reaches it through any ref namespace (for example `HEAD` → `refs/heads/main` → `refs/tags/alias` → the Callsheet ref), a malformed, cyclic or over-five-hop chain, a namespace (prefix) conflict or a case alias is refused. Every symbolic ref target must be a well-formed name under `refs/` (no `..`, empty, dot-leading or `.lock` components) before it is read, so no ref ever resolves outside the git directory. A hash ref that holds another commit is refused, never repaired. The loose ref shadows a packed one; `packed-refs` is not rewritten.

Objects are written as loose objects through a private temporary file, synced, and published without replacing an existing object (an existing one must hash correctly). Objects installed before a later failure may stay unreachable, as in any git object store. The ref is published only after the objects and their directories are durable.

To deliver the result, push it to your own remote yourself (S18), for example:

```sh
git push <your-remote> <commit>:refs/heads/<delivery-branch>
```

or inspect `refs/callsheet/NAME/...` first. Callsheet never runs that command, never stores remote URLs or credentials and never chooses the delivery branch.

## Pulling into a folder

Any other `PATH` must be a new name in an existing directory, or an existing empty directory (a hidden file makes it nonempty), outside any repository. A nonempty directory is refused with "destination must be new or empty" (conflict, `destination_not_empty`) and never touched; a file, symlink or special file is `invalid_argument`; missing parents are never created; the filesystem root is refused. Parent symlinks are resolved once; the final component is never followed.

The commit is fetched and verified completely, and its whole tree is validated before anything is created: every name must be a safe single component (not empty, `.` or `..`, no `/` or NUL, not `.git` in any case, and on macOS not a name HFS+/APFS treats as `.git`), fit the native component and path limits including the staging prefix, and have a regular, executable or symlink mode (gitlinks are refused). Symlinks must have nonempty relative targets shorter than 4096 bytes whose complete resolution through the tree's own directories and links stays inside the export, without cycles, overlong chains or traversal through a file; dangling links inside the export are fine. On macOS, and on any destination whose filesystem Callsheet's probe (a private name and its case or normalization alias, inside the staging directory) finds aliasing names, the resolution is also checked with names compared the way such a filesystem compares them (Unicode case folding and normalization), so a link such as `B -> a/../escape` beside `A -> .` is refused. An unsafe tree is `invalid_argument` with `unsafe_tree`; pull it into a git repository instead.

The files are written into a private sibling directory `.callsheet-pull-<32 hex>` (mode 0700, same filesystem) with exclusive, no-follow creation. A name the filesystem aliases to one already written (case folding or Unicode normalization on macOS) collides and refuses the export instead of overwriting. Modes: 100644 → 0644, 100755 → 0755 (set explicitly, whatever the umask), legacy 100664 → 0644, directories 0755, a new root 0755, an existing empty root keeps its permission bits. Files and then directories are synced bottom-up, and the staging directory is published by one atomic rename: `renameat2(RENAME_NOREPLACE)` (Linux) or `renameatx_np(RENAME_EXCL)` (macOS) onto an absent name, or a rename over the still-empty directory (which the kernel refuses once it is populated). A destination that appears or fills up meanwhile is never replaced (`destination_not_empty`). A cancellation or failure before the rename removes only the staging directory and publishes nothing (likewise, a pull into a repository cancelled before its ref rename removes only its own lock); a killed process may leave a private staging sibling, never a partial destination. After the rename the parent directory is synced; if that or the answer fails, the complete tree may be there: inspect it before pulling again.

## Resolution and cost

Pull resolves one immutable commit before downloading: branch and task refs through the status pages pinned by instance and generation (a change between pages aborts, it never restarts), a hash through `ws diff` with base `empty` and limit 1. That diff is the plane's reachability probe: even with limit 1 the plane validates reachability and computes metadata over the selected commit's whole tree (work proportional to the reachable graph and tree, with no blob content returned). After the fetch the workspace is shown again and must have the same instance; a moved branch does not invalidate a commit already fetched and verified, and the result reports the commit actually selected.

Transfers stream over the plane's TLS endpoint without a total timeout: the dial and handshake keep their bounds, and every request write, the wait for the answer and every response read must make progress within 30 seconds. Push scanning hashes every tracked file (work proportional to the paths and the tracked bytes, memory proportional to the path metadata, not to file sizes). Objects are staged on disk in a private temporary directory (mode 0700, removed afterwards; a killed process may leave one): expect temporary disk use of the selected history plus the pack, besides the plane's own generation copy (09a). There are no file-count or size quotas; a full disk is a clean `storage_failure` before anything is published.

## Workspace tasks

Since iteration 10b (protocol 6) a task can run in a workspace. The selection is the dispatch request's `workspace`, `base` and `workspace_instance` (HTTP `POST /api/v1/tasks`), and since iteration 10c the CLI's `callsheet dispatch --workspace NAME [--base SELECTOR] [--workspace-instance TOKEN]` and the MCP `dispatch` tool's optional `workspace`, `base` and `workspace_instance` arguments, all checked by the same contract validator:

```sh
callsheet dispatch --role-name implementer --goal "fix the parser" --acceptance "tests pass" \
  --workspace myproject --base main --workspace-instance <instance> \
  --plane https://plane.example:8443 --ca plane-ca.crt
```

- `workspace` names the workspace; omitted means a task without a workspace, while `""` or `null` is `invalid_argument`. `base` (requires `workspace`) is omitted for `refs/heads/main` (an unborn `main` is `not_found`, never an implicit empty base), `empty` for an empty tree with no parent, a branch (short or `refs/heads/...`), a full 40-hex commit reachable from a ref, a full task ref, or a bare `t_<32 hex>` task ID meaning that task's ref. `workspace_instance` (requires `workspace`) is a compare-only precondition: another instance is `conflict` before anything is dispatched.
- Admission resolves the instance and base commit once and stores the immutable binding (`workspace_binding` in the task view: `name`, `instance`, `base_selector`, `base_commit`). Later branch movement never changes it. Resolution is not a retention lease: a prune or `ws rm` before the worker's fetch makes the start fail (`workspace_base_unavailable` or `workspace_unavailable`).
- The worker fetches the bound base through an assignment-guarded node route into its shared cache, copies the selected history into the task's own object database and checks it out into a private working directory with its own `.git` (detached `HEAD` at the base; for `empty` an unborn `main`). The child may use git there; Callsheet never runs git.
- The node routes (`/api/v1/node-workspaces/<task_id>.git/...` and the task's `workspace-publication` endpoints) compare the claimed node ID, execution, start digest and workspace instance with the durable task, and the publication ID is a correlation token. These are correctness checks against accidental cross-task operations, not credentials: a claimed node ID is not authenticated, and anyone on the trusted network with the CA can impersonate a node or use the ordinary coordinator APIs (R-TLS-9). The worker answers a workspace start `preparing` on its stream and fetches over HTTPS straight away, so the fetch can reach the plane before that reply is applied. The plane then holds the fetch until the reply is applied, the stream ends or the request is abandoned, and judges it by the usual live-preparation check. It never refuses a preparation only because the fetch arrived first.
- After the child's process group is gone, the visible files become the result tree with the plain-folder policy of [Ignore rules](#ignore-rules): root and nested `.gitignore` files only, byte-exact, applied to every path, so a base file that a new rule ignores is a deletion. The top-level `.git` the child owns is excluded; anything about the child's own commits, branches, index or `HEAD` has no effect. A nested repository (`.git` in any case), a Git LFS pointer, a socket, FIFO or device file, or an unsafe symlink fails the publication with `unsupported_repository` or `workspace_snapshot_failed`; it never flattens or skips content. This is a deliberate folder policy, not `git add -A`.
- Every eligible task (succeeded, failed, cancelled after its child ran, timed out) publishes exactly one create-once ref `refs/callsheet/tasks/<task_id>`, even with an unchanged tree. Its commit's only parent is the base (none for `empty`), author and committer `Callsheet <workspace@callsheet.invalid>` at the Unix epoch, and the message names the task, role, effective model and terminal state. Lost and rejected tasks publish nothing; a task cancelled before its adapter started is `not_started`.
- The result's `workspace` object reports `publication` (`published`, `failed`, `not_started`, `not_applicable`), the commit and ref when published, a fixed `error` when failed, tree-metadata totals (`diffstat`: added, modified and deleted paths and old/new bytes, never line counts) and at most 100 change rows and 32 KiB (`changes_truncated`, `next_after`), with exact totals. `result_commit`, `diffstat` and `changed_paths` mirror it. A failed publication never changes the task's state: **check `workspace.publication` before chaining a task's result.**
- Publication survives crashes on either side. The plane records an intent before the ref and the terminal record after it. A worker restarted after an intent only observes the settlement and never pushes again. The plane settles an intent nobody completes at its five-minute expiry, or at its own restart. While a receive's hub storage outcome is ambiguous (a sync failure after its commit point), the task stays completion-pending until the plane's restart recovers the workspace. `ws rm` and `ws prune`, even when already queued, wait for an active publication instead of erasing its evidence.
- Peak disk for workspace tasks: the plane's generation copy during each receive (09a), plus on each node its disposable cache (an idle target of 1 GiB of logical bytes, least recently used entries evicted; a single larger history is used privately and not kept) and its staging, plus per simultaneous task one independent copy of the selected history, its checkout, and the result's objects and pack staging. There is no task quota; a full disk fails the preparation or the publication with a fixed reason.
- The task's own working directory is the private checkout: a role's configured work directory is not supported for workspace tasks, and a task without a workspace (scratch) has no result commit at all.
- Dispatch success and `task show`/`task wait` terminal views carry `workspace_binding`, `workspace_phase` (`preparing`, `executing` or `publishing`; null when terminal) and the terminal `result.workspace`. CLI text adds `workspace`, `workspace_instance`, `base_commit`, `workspace_phase` and, with a terminal workspace result, `publication`, `result_commit`, `result_ref`, `publication_error`, `diffstat` and one escaped `changed_path` line per path (`changed_paths_truncated: true` and `changed_paths_next_after` when cut); a null value prints as `-`. A nonterminal `task wait` answer stays the compact still-running row: use `task show` for the binding and phase.

## Task results by task ID

Iteration 10c lets a coordinator name a task instead of a workspace and ref. These are the only task-aware doors: CLI `dispatch --base` and MCP `dispatch.base`; `ws pull TASK_ID`, `ws status TASK_ID` and `ws diff TASK_ID` and the MCP `task_id` forms of `ws_pull`, `ws_status` and `ws_diff`; and the explicit `ws pull NAME REF`/`ws_pull.ref` and `ws diff` base and target (`--base`, `TARGET`, `ws_diff.base` and `.target`), where a bare task ID means its complete task ref. `ws ref set` (and `ws_ref_set.target`) and the raw hub API are unchanged: there `t_...` is still a branch name. Use `refs/heads/t_...` to select a branch named like a task ID at a task-aware door.

```
callsheet ws status [--json] --plane URL (--ca FILE | --ca-fingerprint SHA256) TASK_ID
callsheet ws diff [--after CURSOR --instance TOKEN --generation TOKEN] [--limit N] [--json] --plane URL (--ca FILE | --ca-fingerprint SHA256) TASK_ID
callsheet ws pull [--json] --plane URL (--ca FILE | --ca-fingerprint SHA256) TASK_ID [PATH]
```

- **Lookup.** The task's durable record decides: no binding is `invalid_argument` (`no_task_workspace`), checked first; a task that is not terminal yet (preparing, executing or publishing) is `conflict` (`task_result_pending`); a terminal task whose publication is `failed`, `not_started` or `not_applicable` (lost, rejected, cancelled before start) is `conflict` (`task_result_unavailable`); an unknown task is `not_found`. There is no name inference, fuzzy match, latest-task or role-name lookup.
- **Exact result.** Pull and diff then check that the workspace still has the bound instance and holds exactly the recorded task ref and hash: a removed or recreated workspace is `conflict` (`workspace_instance_mismatch`), even if the replacement holds the same commits; a pruned ref is `not_found`; a ref naming another commit is `conflict` (`task_result_unavailable`). Nothing falls back to a new workspace of the same name or to a local cache.
- **Pull.** The guarded transfer fetches the recorded hash and checks the instance again before anything local is published, even when the objects are already local. Into a repository it installs only objects and `refs/callsheet/NAME/tasks/TASK_ID` (checkout, index, `HEAD`, branches, config, remotes and `FETCH_HEAD` untouched; uncommitted changes are fine); into a new or empty folder it exports with the [folder](#pulling-into-a-folder) rules. It prints the ordinary pull result; its `selector` is the complete task ref.
- **Status.** `ws status TASK_ID` (no paging flags; MCP `{task_id}`) reads `GET /api/v1/tasks/<id>/workspace`: `{"version":6,"workspace":{task_id, state, workspace_phase, binding, result, available}}`. `result` is the bounded terminal result (null while running), and `available` is true only when the publication is `published` and the current matching instance holds the exact ref and hash. A prune, removal or recreation makes it `false` with the historical result intact; a real storage failure is an error, never `available: false`. It is a fresh observation, not a retention lease. `ws status NAME` stays the hub inventory (task refs with their `published_at`).
- **Diff.** `ws diff TASK_ID` (no `--base`, no `TARGET`; MCP `{task_id}`) compares the admitted base commit (or the empty tree) with the published result commit, both fixed by the record, with the 09a pages (at most 100 rows and 1 MiB each, paths sorted by raw bytes, metadata only). Every page rechecks the bound instance and the exact ref. A continuation passes `--after`, `--instance` and `--generation` together (`--limit` is independent); its instance must be the task's, and a changed generation is `conflict` (restart): nothing restarts or mixes pages on its own. The simplest way to read all pages is to run `ws diff TASK_ID` again from the first page. To compare against another base use `ws diff --base BASE NAME TARGET`. The result's 32 KiB preview cursor (`next_after`) can seed a new explicit hash-based diff only after reading a current generation; it carries no generation itself.

## Chaining tasks

The continuation recipe: wait until task A is terminal; require `result.workspace.publication` to be `published`; take `result.workspace.commit` and `workspace_binding.name` and `.instance`; dispatch B with them as `base`, `workspace` and `workspace_instance`, and check B's returned `workspace_binding` against that instance and hash. `result.result_commit` is the same commit (its top-level mirror). The instance guard is checked at admission under the same workspace lock as the base resolution, so B never runs on an identically named replacement, even one seeded with the same commit; do not use a check-then-dispatch-then-cancel race instead.

B's result commit has exactly A's result commit as its parent, also when A failed, was cancelled or timed out and you deliberately continue from its partial result. Nothing chains automatically on a terminal state, on an unpublished local hash or on `main`; both task refs stay, and no branch (`main` included) moves. A's task ID is also accepted as `base` (its task ref), but the recipe uses the hash to state the exact ancestry. A prune between A and B's admission or fetch makes the hash unavailable: B fails normally, and nothing reruns A or picks another base.

## Coordinator workflow

A copyable local sequence (the plane, a worker node and this machine), executed as written by the function test `TestWorkspaceOperatorWorkflow/documentation`. Lines starting `callsheet` run; a comment `note the printed KEY as VAR` names a value of a later command's `KEY: value` output; the final `git` line is your own optional step and never a Callsheet action.

<!-- workflow:begin -->
```sh
TRUST="--plane https://plane.example:8443 --ca plane-ca.crt"
ROLE=implementer
# 1. Create a workspace; note the printed instance as INSTANCE.
callsheet ws create $TRUST myproject
# 2. Seed it from a plain folder (or the root of a clean repository).
callsheet ws push --instance $INSTANCE $TRUST myproject ./myproject
# 3. Dispatch task A on main with the instance guard; note the printed task_id as TASK_A.
callsheet dispatch --role-name $ROLE --goal "implement the parser" --acceptance "tests pass" --workspace myproject --base main --workspace-instance $INSTANCE $TRUST
# 4. Wait for A and read its result; note the printed result_commit as COMMIT_A (publication must be published).
callsheet task wait --wait 5m $TRUST $TASK_A
callsheet task show --lines 0 $TRUST $TASK_A
# 5. Inspect A's workspace status and its diff against the admitted base.
callsheet ws status $TRUST $TASK_A
callsheet ws diff $TRUST $TASK_A
# 6. Dispatch task B on A's exact result commit and instance; note the printed task_id as TASK_B.
callsheet dispatch --role-name $ROLE --goal "review the parser" --acceptance "findings listed" --workspace myproject --base $COMMIT_A --workspace-instance $INSTANCE $TRUST
callsheet task wait --wait 5m $TRUST $TASK_B
callsheet ws diff $TRUST $TASK_B
# 7. Deliver B into your existing (even dirty) repository, or into a new folder.
callsheet ws pull $TRUST $TASK_B ~/src/myproject
callsheet ws pull $TRUST $TASK_B ./delivery
# 8. Optional, your own step: push the result to your own remote yourself.
git -C ~/src/myproject push <your-remote> refs/callsheet/myproject/tasks/$TASK_B:refs/heads/<delivery-branch>
```
<!-- workflow:end -->

What to expect:

- **Result trees.** The visible files after the child exits are committed directly onto the admitted base (one parent; none for `empty`), whatever commits or branches the child made itself; with the [plain-folder ignore policy](#ignore-rules) (root and nested `.gitignore` only, byte-exact). The child's `.git` and the runtime's own files are excluded. Every eligible task gets exactly one result commit, even for an unchanged tree. The commit's identity and message are fixed (`Callsheet <workspace@callsheet.invalid>`, epoch time, task, role, model and state), and the base's bytes are used as stored (normalized check-in form).
- **Metadata only.** Results, status and diffs carry names, tokens, hashes, modes, sizes and fixed codes, bounded (100 rows and 32 KiB per result, 1 MiB per page): never file contents, patches, symlink targets, commit messages or pack diagnostics. Task output and final messages keep their own channels.
- **Time bounds.** Preparation and finalization each have five minutes; an authorized publication expires after five minutes. A publication failure (`workspace.publication: failed` with a fixed `error`) is separate from the child's outcome: it never changes the task's state.
- **Trust.** Node routes check the claimed node ID, execution, start digest and instance against the durable task: correctness checks against accidents on a trusted network, not authentication. Anyone with network access and the CA can act as a coordinator or impersonate a node.
- **Disk.** Each node keeps a disposable workspace cache (an idle target of 1 GiB, least recently used entries evicted) plus one private copy per running task; a large history therefore costs disk on every node that runs it, and the plane copies a generation per receive.
- **Native names.** A tree a platform cannot represent safely (names a filesystem aliases, unsafe symlinks, nested repositories) fails preparation or publication with a fixed code instead of being altered.
- **Crashes and pruning.** A restarted worker or plane recovers a publication without publishing twice (one stable receipt); interrupted checkouts and staging are cleaned on restart. `ws prune` removes old task refs: a pruned result stays in its task's history as metadata but is no longer `available` and cannot be pulled, diffed or chained.
- **Delivery.** Callsheet stores no remote URLs or credentials and never advances a branch. Delivering a result to your own git remote is your own `git push` (09b's next step), never a Callsheet action.

## Manual M4 checks

These are manual checks for the two-machine M3+M4 acceptance session (a laptop coordinator and a remote worker node). Local automation, including `TestWorkspaceOperatorWorkflow`, does not perform or qualify them.

1. **M4-1 Laptop push:** push a project from the laptop into a new workspace.
2. **M4-2 Remote cwd is the base:** task A on the remote node reports a working directory whose files equal the selected base commit.
3. **M4-3 A metadata and diff:** A's task ref, `ws status TASK_ID` metadata and `ws diff TASK_ID` are visible from the laptop.
4. **M4-4 Second hop:** task B dispatched on A's exact result hash and instance sees A's changes, and B's result parent is A's commit.
5. **M4-5 Parallel sibling:** a sibling task on the original base sees neither A's nor B's changes.
6. **M4-6 Partial results:** a failed, a cancelled and a timed-out task each publish an inspectable partial result.
7. **M4-7 Lost task:** a lost task has no ref.
8. **M4-8 Restart around publication:** a node or plane restart around a publication yields one stable receipt.
9. **M4-9 Laptop pull:** pulling a result preserves the laptop's dirty checkout, and `main` never moves.
10. **M4-10 External delivery (optional):** the user pushes a pulled result to an external remote with their own `git push`.

Record for each check: the OS of both machines, the Callsheet and adapter versions, the task IDs, the workspace instance, the base and result hashes, and the terminal state and publication status. Never record file contents or credentials.

## Errors

| Code (exit) | When |
|---|---|
| `invalid_argument` (2) | arguments, paths, unsupported repositories (`unsupported_repository`), unsafe trees (`unsafe_tree`), a task without a workspace (`no_task_workspace`) |
| `not_found` (3) | missing source path, workspace, branch, task, task ref (a pruned result) or unreachable hash |
| `conflict` (4) | dirty source (`dirty_source`), non-fast-forward (`non_fast_forward`), stale instance or compare-and-swap, source changed during the push (`source_changed`), occupied destination (`destination_not_empty`), local ref lock or value (`local_ref_conflict`), a task not terminal yet (`task_result_pending`) or without an available published result (`task_result_unavailable`), a removed or recreated task workspace (`workspace_instance_mismatch`), a changed generation between pages |
| `unavailable` (5) | network failures, the 30 s no-progress bound |
| `trust_failed` (6) | TLS verification |
| `protocol_mismatch` (7) | version mismatch |
| `internal` (1) | unexpected local I/O, a full disk (`storage_failure`), integrity failures |

SIGINT and SIGTERM exit 130. Messages are fixed and safe: no raw library error, file content or pathname.

## Examples

S6, seed a workspace from a project folder and keep it current:

```sh
callsheet ws create --plane https://plane.example:8443 --ca plane-ca.crt myproject
callsheet ws show --plane https://plane.example:8443 --ca plane-ca.crt myproject     # note the instance
callsheet ws push --instance <instance> --plane https://plane.example:8443 --ca plane-ca.crt myproject ./myproject
# later, after editing ./myproject:
callsheet ws push --instance <instance> --plane https://plane.example:8443 --ca plane-ca.crt myproject ./myproject
```

S18, deliver a result either into your own repository or into a new folder:

```sh
# into your repository (objects and refs/callsheet/myproject/heads/main only):
callsheet ws pull --plane https://plane.example:8443 --ca plane-ca.crt myproject main ~/src/myproject
git -C ~/src/myproject push <your-remote> <commit>:refs/heads/<delivery-branch>
# or into a new folder:
callsheet ws pull --plane https://plane.example:8443 --ca plane-ca.crt myproject main ./delivery
```
