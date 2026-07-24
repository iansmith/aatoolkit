# aa-server-status — design

Status: **design / agreed direction.** Written 2026-07-06. Staging doc for ticket creation (the tickets are the long-lasting record).

`aa-server-status` is a Go program that starts, stops, and reports the health of every server in the system — MLX model servers, Python (venv) servers, a `caddy`-style reverse proxy, and the `app` core driver itself. It replaces the hand-rolled `scripts/*.sh` launchers and centralizes all lifecycle + environment management in one place.

Guiding principle throughout: **aa-server-status is the single source of truth.** It must know the real state unambiguously, fail loud rather than best-effort, and never paper over a discrepancy.

---

## 1. Shape: a long-lived, single-owner REPL supervisor

`aa-server-status` is **not** a one-shot CLI. It is a long-running process that owns the child processes it launches (holds their `exec.Cmd` handles) and exposes an interactive prompt.

```
$ aa-server-status
<prints the status table>
aa-server-status> up
aa-server-status> status
aa-server-status> bye
```

- On launch it **acquires an exclusive lock** (`build/run/aa-server-status.lock`, `flock`). The process holding the lock is *the* supervisor.
- A second launch that can't acquire the lock **fails loudly**, naming the running PID. Only one supervisor may exist.
- The original one-shot shell grammar (`aa-server-status up`, `aa-server-status down`) is **retired** — a one-shot process can't be the long-lived owner. All verbs are entered at the prompt.

Rationale: a single in-process owner is the most accurate identity model (it holds the actual handles) and matches the "one source of truth" goal. This is a small dev box; durability across terminal sessions is explicitly *not* required (the nightly ritual is `bye`/`down` before closing the lid).

### 1.1 Signals

- **Ctrl-C (`SIGINT`):** a single or double SIGINT is **swallowed** (fleet keeps running). **Three SIGINTs within ~2 s** → run `down`, then exit. This prevents an accidental keystroke from tearing down the fleet.
- **Ctrl-Z (`SIGTSTP`):** trapped and **ignored** — the supervisor must not be suspended out from under its children.
- Terminal signal isolation is achieved by launching every child in its **own process group** (see §6), so the TTY delivers Ctrl-C only to the supervisor, which then decides what to do.

---

## 2. Commands (REPL verbs)

| input | action |
|---|---|
| `status`, or a **bare Enter** | print the status table for all servers |
| `<name>` | status of one server |
| `up` | reconcile-up: launch every enabled server that's down; rebuild+relaunch any **owned source server that is stale** |
| `<name> up` | imperative up for one server (regardless of `enabled`) |
| `down` | kill every **enabled** server that's running; **warn** about strays, don't touch them |
| `<name> down` | imperative kill for one server |
| `dead` | same as `down`, **plus** kill the strays too |
| `build` | rebuild all `source` servers; restart those that were running (see §5) |
| `<name> build` | rebuild one source server (`build` on a non-source server → loud error) |
| `<name> bounce` | imperative down-then-up for one server, composing `<name> down` and `<name> up` through their exact code paths. No bare `bounce`: cycling the whole fleet is a materially different risk (long warm-ups, externally-facing ports) than any other bare verb, and is deliberately not offered |
| `logs <name>` | point at the current `build/logs/<name>-<ts>.log` |
| `details <name>` | everything known about a server, incl. log path — **deferred** |
| `help` | command help |
| `quit` / `exit` / `bye` / **Ctrl-D** | run `down`, then exit |

Exit always tears down (no "detach and leave running" option — detached children just become foreign processes the next launch rejects). A supervisor **crash** orphans the children (own process groups); the next `up` refuses and names them, and the user resolves manually. Accepted tradeoff.

---

## 3. Desired state and the reconciliation model

Each server has an **`enabled`** flag = its desired state.

- **`up`** reconciles reality toward a *healthy* desired state: launch the down, rebuild+relaunch stale owned source servers. On a normally-running system, `up` either does nothing or just recompiles+relaunches `app` when stale. Staleness of an owned source server is the **only** reason aa-server-status restarts something it already owns.
- **`down`** reconciles the other direction: kill the enabled servers that are running. Strays (running-but-disabled) are **warned, never killed** — e.g. `"spare-model is up but not enabled, so ignoring it"`.
- **`dead`** = `down` + also kill strays.
- **`<name> up`/`<name> down`** are imperative overrides (this is how you start a disabled server like `spare-model` for testing, or kill an enabled one).

A **stray** = a server that *is in the config* but `enabled = false`, yet is currently running.

---

## 4. The four server types

| type | launch | notes |
|---|---|---|
| `mlx` | `mlx-serve serve <model> --host <host> --port <port>` | `model`, `host`, `port` from config; `--host/--port` auto-appended |
| `python` | `<venv>/bin/<entry>` + `--host/--port` | venv + package **preflight** (§7); `--host/--port` auto-appended |
| `exec` | `command` + `args`, **verbatim** | no auto-flags; server creates its own listeners (proxy) |
| `source` | build a Go binary, run `binary` | three states: up / down / **stale** (§5); explicit `args`, no auto-flags |

Launch-flag rule: **mlx + python** get `--host <host> --port <port>` auto-appended. **exec + source** are launched from explicit `args` (their ports come from their own config/args, since they may be multi-port).

**`dir`** (optional, any server type) — sets the child's working directory (`exec.Cmd.Dir`) at launch. A leading `~/` is expanded against the user's home directory (same convention as `dir`'s pre-existing build-time use, below); otherwise, when relative, `dir` resolves against **aa-server-status's own launch cwd** (it is *not* config-file-relative — only the supervisor's `base_dir` is, see §7). A relative `venv`/`entry`/`binary` on that server then resolves against `dir` instead of the launch cwd. Unset `dir` leaves the child's working directory as today (inherits the supervisor's cwd). For `source` servers, `dir` is reused from its pre-existing build-time role (`go -C <dir>`, §5) — the same field now also sets the post-build launch's `cmd.Dir`.

---

## 5. `source` staleness and `build`

A `source` server (currently just `app`) can be up / down / **stale**. Stale = "the on-disk binary differs from what a fresh compile of current source would produce."

**Detection (accurate, does not touch the deployed binary):** run the canonical build with output to a **temp path** (`go build -o <tmp> -buildvcs=false ./cmd/app`), hash `<tmp>` vs `build/app`; differ → stale. `-buildvcs=false` and identical flags avoid false alarms from VCS/dirty-tree stamps. The Go build cache keeps a no-change check sub-second. Computed on `status` and on `<source> up`.

**`build` verb** (source servers only):
1. Build to temp; hash-compare vs the on-disk binary.
2. If **identical** → no-op.
3. If **different** → refresh the on-disk binary, and mirror prior lifecycle:
   - **was running** → stop → replace binary → start on fresh code;
   - **was down** → replace the on-disk binary, **leave it stopped** (`build` never *starts* a stopped server — starting is `up`'s job).

The canonical build command lives in the TOML (`build = "go build -o build/app ./cmd/app"`) — single source of truth reused by both the staleness probe and the real rebuild.

---

## 6. Liveness, identity, and teardown

### 6.1 What "up / serving" means

Three-stage gate, all required:

1. **Port listening** — a *necessary precondition*.
2. **Process identity** — for children the supervisor started, it holds the handle (trivial). For processes it did *not* start (strays, foreign holders), identity is matched via **cmdline** (see §6.2).
3. **Health probe** — a **mandatory** `GET` health endpoint returns 2xx. This is the authoritative "serving" signal. No `any_http` fallback.

Health path is per-server: `/healthz` wherever we control the server (spelled out in the TOML), `/v1/models` for mlx-serve, admin `/config/` on `:2019` for the proxy.

### 6.2 Observed state via `gopsutil`

Actual OS state is observed with **`github.com/shirou/gopsutil/v4`** (a committed dependency), which gives per-PID **cmdline** (identity) and **listening ports** (introspection) without aa-server-status parsing `lsof` text itself.

**Exact-listen-set contract:** a running server's declared set `{port} ∪ listens` is treated as **exhaustive**, checked across the server's whole **process tree** (uvicorn workers, mlx subprocesses included):

- actual ⊋ declared → **stray listening port** — loud anomaly.
- actual ⊊ declared → **partial**.
- actual = declared + health 2xx → **up**.

### 6.3 No adoption — refuse foreign

aa-server-status only ever manages processes it launched.

- **`up` is precondition-gated:** before launching a target set, it checks *every* port those servers need (`port` and `listens`). If any is held by a process that is **not our own live child for that same server**, it **refuses and names the holder** (PID + cmdline). The user decides.
- A port held by **our own healthy child** for that server → **already-up, skip** (idempotent `up`).
- A leftover process that merely *looks* like our server (prior crashed session) is still **foreign** → refused. (Tradeoff: after an unclean exit, `up` won't work until you kill the leftovers — consistent with "user decides.")

### 6.4 Startup and teardown

- **Startup:** parallel — soft dependencies between servers; callers handle a backend being briefly absent. No dependency graph in v1.
- **Own process group per child** (`SysProcAttr{Setpgid: true}`): isolates children from terminal signals *and* enables whole-tree group-kill.
- **Teardown** (per server, in **reverse config order**): `SIGTERM` the process group → wait `grace_period` (default 5 s, configurable) → if anything survives or a declared port still listens, `SIGKILL` the group → **re-probe and verify every declared port is free and health is dead**; if a listener survives, **loud error** (never report a kill you didn't achieve). Foreign strays killed by `dead` are group-killed by PID via gopsutil.

### 6.5 Failure semantics

- **Config / startup errors** (bad TOML, unknown type, port collision) → **hard-exit the program, loudly**. Can't run on a broken config.
- **Runtime command errors** (a server won't come up; health never green) → **abort that command loudly, return to the prompt.** The supervisor never dies because one server misbehaved.
- **Multi-server commands** attempt **all** servers, then print a **loud aggregate** of exactly which succeeded and which failed (e.g. `model-server ✓, worker ✗ (health never green after 60s, see build/logs/…)`). "Don't do best-effort" = honesty, not fail-fast.

---

## 7. Configuration

Split by sensitivity, deep-merged at load (local wins):

- **`aa-server-status.toml`** — *committed.* Server topology + all **non-secret** env (tuning vars, model names, ports).
- **`aa-server-status.local.toml`** — *gitignored.* Same schema, **secret** env only (provider creds, API keys).
- **`aa-server-status.local.toml.example`** — *committed.* Documents expected secrets.

**Strict decode:** unknown/misspelled keys are a **hard error**. Structural validation (all hard-exit): duplicate server names; port collisions across `{port} ∪ listens` sets; per-type required fields (mlx→`model`; python→`venv`+`entry`+`packages`; source→`build`+`binary`; exec→`command`; all need a health spec + at least one port); `health.port` must be a member of that server's declared port set.

### 7.1 Schema

```toml
[supervisor]
log_dir       = "build/logs"
lock_file     = "build/run/aa-server-status.lock"
base_dir      = "build"    # optional — anchors log_dir/lock_file (see below)
grace_period  = "5s"       # default; per-server override
ready_timeout = "15s"      # default; per-server override
poll_interval = "500ms"

# --- mlx: single launch port ---
[[server]]
name  = "model-server"                               # primary chat model
type  = "mlx"
enabled = true
host  = "127.0.0.1"
port  = 1235                                         # launched with --port 1235
model = "mlx-community/example-30b-it-8bit"
health = { path = "/v1/models" }                     # mlx native; not /healthz
ready_timeout = "90s"                                # cold 30GB weight-load

[[server]]
name  = "spare-model"                                # provisioned, not yet in use
type  = "mlx"
enabled = false                                      # → stray if found running
host  = "127.0.0.1"
port  = 1234
model = "mlx-community/example-alt-model"
health = { path = "/v1/models" }
ready_timeout = "90s"

# --- python: venv + preflight ---
[[server]]
name = "worker"                                      # example TTS worker
type = "python"
enabled = true
host = "127.0.0.1"
port = 7788
venv = ".venv"
entry = "example-tts serve"                          # first token resolved against <venv>/bin
packages = ["example_tts"]
health = { path = "/healthz" }                       # we control it → add /healthz

[[server]]
name = "ingest"                                      # example STT ingest
type = "python"
enabled = true
host = "127.0.0.1"
port = 7789
venv = ".venv"
entry = "python scripts/ingest_server.py"
packages = ["example_stt", "fastapi", "uvicorn", "multipart"]
env = { APP_STT_MODEL = "mlx-community/example-stt-model" }
health = { path = "/healthz" }                       # we control it → add /healthz

# --- source: build + multi-port ---
[[server]]
name = "app"                                         # core driver — becoming an HTTP server
type = "source"
enabled = true
host = "127.0.0.1"
listens = [9730]                                     # app-cli↔HTTP now; grows to [9730, 9740] with a second listener
build = "go build -o build/app ./cmd/app"
binary = "build/app"
health = { port = 9730, path = "/healthz" }
env = { APP_MAX_SEC = "180", APP_IDLE_MS = "12000" }  # non-secret tuning (read by the app)

# --- exec: verbatim, multi-port, external bind ---
[[server]]
name = "proxy"                                       # public TLS + reverse proxy
type = "exec"
enabled = true
host = "0.0.0.0"                                      # external interface
listens = [80, 443, 2019]
health = { host = "127.0.0.1", port = 2019, path = "/config/" }  # admin endpoint
command = "go"
args = ["tool", "caddy", "run", "--config", "Caddyfile"]
```

Field notes:
- **`port`** (scalar, optional) — the launch/health port for mlx & python; `--port <port>` auto-appended.
- **`listens`** (list, optional) — ports a self-listening server (source/exec) should be verified on; no launch flags.
- **`health`** = `{ host?, port?, path }` — `host` defaults to `host`, `port` defaults to `port` (must name a member of `listens` for source/exec).
- **`env`** — per-server map exported into the child at launch. Secrets go in the `.local.toml` overlay.
- **`ready_timeout`** — how long `up` polls health after launch before declaring failure (90 s for cold MLX models).
- **`base_dir`** (supervisor, optional) — anchors relative `log_dir`/`lock_file` to something other than the supervisor's own launch cwd. When relative, `base_dir` resolves against **the directory containing the `--config` file** — never against process cwd. Unset `base_dir` leaves `log_dir`/`lock_file` resolving against launch cwd exactly as today. An already-absolute `log_dir`/`lock_file` is never touched, `base_dir` or not.

**`dir` vs. `base_dir` — they resolve differently, on purpose:** a server's own `dir` is relative to *aa-server-status's launch cwd*; the supervisor's `base_dir` is relative to *the config file's own directory*. This asymmetry exists because `dir` was already load-bearing for `source`-type build sourcing (§5) before this field gained a launch-time role, while `base_dir` is new and picked config-file-relative resolution specifically so a config file can live away from the servers it launches (see the worked example below) without every server needing its own `dir` just to find the supervisor's own log/lock files.

**Worked example — a config file living outside the servers it launches.** `~/infra/router/models-dev.toml` supervises `alt-model`, `lite-model`, and `router`, but those servers' own files (and the supervisor's logs) live under `~/project` and `~/infra/router` respectively — not next to `models-dev.toml` itself in every case:

```toml
# ~/infra/router/models-dev.toml
[supervisor]
base_dir  = "."                       # this file's own directory: ~/infra/router
log_dir   = "build/logs"              # resolves to ~/infra/router/build/logs
lock_file = "build/run/router.lock"   # resolves to ~/infra/router/build/run/router.lock

[[server]]
name  = "lite-model"
type  = "python"
dir   = "~/project"                   # this server's own venv/entry resolve from here, NOT from
venv  = ".venv"                       # aa-server-status's launch cwd and NOT from base_dir — dir is
entry = "python litellm_config.yaml"  # its own field, cwd-relative, unrelated to base_dir's scope
```

---

## 8. Status rendering

On-demand reprint (no live TUI), printed on launch and on every `status` / bare Enter.

```
SERVER        TYPE    DESIRED    STATE      PORTS              PID    HEALTH
model-server  mlx     enabled    up         1235 ✓             4821   /v1/models 200
spare-model   mlx     disabled   down       1234               —      —
worker        python  enabled    up         7788 ✓             5012   /healthz 200
ingest        python  enabled    up         7789 ✓             5033   /healthz 200
app           source  enabled    STALE      9730 ✓             5101   /healthz 200
proxy         exec    enabled    PARTIAL    80 ✓ 443 ✗ 2019 ✓  5140   /config/ 200
```

Color: **up** green · **down/disabled** dim · **stale** yellow · **stray / partial / extra-listener / foreign-conflict** red. Anomalies annotate inline: `STRAY (pid 9999, foreign)`, `+8081 ✗unexpected`, `BLOCKED (pid 7777 — not ours)`. Log path is not a column — use `logs <name>` (or the deferred `details`).

---

## 9. Logging

Per-server stdout+stderr → `build/logs/<name>-<ts>.log` (`build/` is gitignored).

**Every launch starts its own log file**, named `build/logs/<name>-2006-01-02-15-04-05.log` from the launch time (Go reference layout, e.g. `model-server-2026-07-06-14-03-11.log`). A log's filename therefore names the single run it contains, and the `tail -f` hint printed after `up` can never point at a previous run's output.

The one exception is granularity: the timestamp resolves to the second, so two launches within the same second (a fast `down`/`up` cycle) share a path. Logs are opened **append-only, never truncated**, so the earlier run's output survives; a timestamped launch-banner line marks where the later run begins.

`logs <name>` / `details` resolve to the newest file by mtime — i.e. the latest run. Rotation/size-caps are deferred.

---

## 10. Implementation side-work (outside the aa-server-status binary)

To make every server honor the mandatory-`/healthz` and one-launch-convention rules:

- **`scripts/ingest_server.py`** — convert to accept `--host/--port` flags (argparse); add a `GET /healthz` route.
- **`worker` FastAPI wrapper** — add a `GET /healthz` route (may require introducing a thin wrapper like `ingest_server.py`).
- **`app`** — add a `GET /healthz` route once it becomes an HTTP server; expose the `9730` app-cli↔HTTP listener.
- **`proxy`** — health via the admin endpoint on `:2019` (no Caddyfile change needed).
- **Delete `scripts/worker.sh` and `scripts/ingest.sh`** — their venv/package preflight and env/param handling move into aa-server-status.

---

## 11. Deferred (explicitly out of v1 scope)

- `details <name>` command.
- Live-updating / `watch` status mode.
- Dependency-graph startup ordering (soft deps only for now; parallel start).
- Log rotation beyond the size/timestamp scheme in §9.
- `app-cli` (the local HTTP tester for the `app` server) — separate work.
- Any control socket / out-of-band one-shot CLI (would only be added if out-of-band access is later needed).
