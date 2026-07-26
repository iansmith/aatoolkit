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
3. **Health probe** — a **mandatory** readiness check reports ready. This is the authoritative "serving" signal. No fallback to a lesser one.

The check takes one of two forms, and every server declares exactly one:

- **HTTP** (`health.path`) — a `GET` returning 2xx. No `any_http` fallback, and no configurable verb: GET is the only method. Health path is per-server: `/healthz` wherever we control the server (spelled out in the TOML), `/v1/models` for mlx-serve, admin `/config/` on `:2019` for the proxy.
- **exec** (`health.exec`) — a command the supervisor runs, where **exit 0 means ready** and any other status means not ready.

The exec form exists because a datastore, queue, or cache does not speak HTTP, so no path can be supplied for it truthfully. Inventing one (`health = { path = "/" }`) yields a check that fails forever, which is worse than no check: it teaches an operator to ignore the one indicator meant to mean something, and it will mask a real outage of that server later.

**Why exec and not a TCP connect.** A TCP check is protocol-agnostic and trivial, but it is *optimistic* — postgres binds and accepts connections before it can serve queries, most visibly on first run while `initdb` builds the cluster — so it reports ready while every query still fails. That is the same failure `warm` (§7.1) already exists to prevent for HTTP servers, where the endpoint answers before the model behind it can. An exec probe generalises the fix past HTTP: the probe program issues a real request, and the protocol knowledge lives in that program rather than in the supervisor, which only ever runs a command and reads a status code.

**Bounded, and never silent.** The probe is killed at `health_timeout`, and its output pipes are given the same budget again to drain — a probe process can exit promptly while a child it spawned still holds them, which would otherwise hang the supervisor one level down from where it was looking. A failing probe's combined stdout and stderr travels with the ready-timeout error, because the probe is a separate process and the supervised server's own log never sees it.

aa-server-status ships no probe programs. The mechanism is the deliverable; a consumer supplies its own.

**The source-exec form: a probe program the fleet builds, not just runs (AATK-41).** A consumer's probe program is often something *this fleet's own tree* builds from source — a datastore's readiness check is code the consuming project wrote, not a system binary already on `$PATH`. Nothing joined `health.exec` to a build step, so a clean checkout (or any `clean` target) left the supervisor trying to exec an argv that did not exist, and reported that as *the datastore being unhealthy* — the operator goes looking at a database that is fine while the actual fault is an unbuilt artifact in the fleet's own tree.

`health.source` declares exactly that: `{ build, binary }`, the same build-command-plus-output-path shape a `source` server's own `build`/`binary` pair already uses (§5) — building this probe is the same operation as building a `source` server's own binary, so the existing build machinery (staleness probe, hash-compare, atomic replace) is reused rather than restated. `health.exec[0]` must name exactly the path `health.source.binary` builds:

```toml
[[server]]
name = "datastore"
type = "exec"                                          # launch is "docker compose up", not something this fleet compiles
enabled = true
host = "127.0.0.1"
port = 5432
command = "docker"
args = ["compose", "up", "-d"]
health = { exec = ["build/db-probe"], source = { build = "go build -o build/db-probe ./cmd/db-probe", binary = "build/db-probe" } }
```

**Why the build is hoisted out of the probe, not run inside it.** The obvious alternative — `health.exec = ["go", "run", "./cmd/db-probe"]`, letting the toolchain build on demand — was measured and rejected. Every probe is capped at `health_timeout` (default **2s**, §7.1), and a real probe program measured **0.13s warm, 4.72s cold**. The cold case — a fresh checkout, a cleaned build cache, a toolchain bump — is exactly the case on-demand building exists to serve, and it exceeds the 2s cap: the build is killed mid-compile, and a missing artifact is reported as a *timeout*, reading as an unresponsive dependency rather than what it is.

`health.source` instead builds **once, during `up`'s start sequence, before the entry's first health probe** — a step with no 2s cap, since it is not itself a probe. A probe therefore never contains a compile: the 2s cap only ever bounds an exec that's already an existing binary. And the build only happens when the on-disk artifact is stale relative to a fresh compile (§5's same hash-compare, reused): a fresh artifact costs nothing on repeated starts, so the common case — every `up` after the first — pays no build cost at all.

aa-server-status still ships no probe programs; `health.source` only gives a consumer's probe an owner for *building* it, not a probe of its own.

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

One file, read from the `--config` path:

- **`aa-server-status.toml`** — *committed.* Server topology plus all **non-secret** env (tuning vars, model names, ports).

Secret and per-machine values are **not** in config. They are exported into the supervisor's own environment and inherited by every child it launches (`mergeEnv` layers a server's declared `env` on top). Nothing here reads a credential out of a file.

There was once a second, gitignored `aa-server-status.local.toml`, deep-merged on top of the committed one. AATK-33 deleted it: it had been silently dropping any `[server.prompt]` declared only in the overlay ever since prompts were introduced, and nobody noticed — which was the clearest evidence available that nothing depended on it. A leftover file on disk is now inert rather than an error, so an operator who still has one gets the committed config.

**Strict decode:** unknown/misspelled keys are a **hard error**. Structural validation (all hard-exit): duplicate server names; port collisions across `{port} ∪ listens` sets; per-type required fields (mlx→`model`; python→`venv`+`entry`+`packages`; source→`build`+`binary`; exec→`command`; all need a health spec + at least one port); `health.port` must be a member of that server's declared port set.

**Exactly one health form.** A server declaring neither `health.path` nor `health.exec` is rejected — health stays mandatory, and the error names both alternatives so the operator adding a non-HTTP dependency is not left guessing that exec exists. A server declaring *both* is rejected as ambiguous rather than resolved by precedence: a precedence rule would silently ignore one of the two things the operator wrote, which is config that looks like it works. `health.host` and `health.port` alongside `health.exec` are rejected for the same reason — they cannot reach a command. So is `warm`: a warm-up is an HTTP request to the server's own port (§7.1), which a server declaring an exec check by definition does not answer, and for that form the probe program is already where a real request belongs.

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

# --- exec: a datastore, whose readiness is a command, not a GET ---
[[server]]
name = "datastore"                                    # speaks its own wire protocol
type = "exec"
enabled = true
host = "127.0.0.1"
port = 5432
command = "docker"
args = ["compose", "up"]
health = { exec = ["db-probe", "--port", "5432"] }    # exit 0 = ready; we ship no such program
```

Field notes:
- **`port`** (scalar, optional) — the launch/health port for mlx & python; `--port <port>` auto-appended.
- **`listens`** (list, optional) — ports a self-listening server (source/exec) should be verified on; no launch flags.
- **`health`** = `{ host?, port?, path }` **or** `{ exec = [...] }` — exactly one form (§7 strict decode). In the HTTP form, `host` defaults to `host` and `port` defaults to `port` (must name a member of `listens` for source/exec); a `"GET /path"` string is accepted as shorthand for it. The exec form is argv, **never a shell line** — the supervisor runs the program directly, so an argument containing a space survives as one argument. There is deliberately no string shorthand for it: splitting a command line on spaces would break exactly that case, and TOML already spells argv as a list. `host`/`port` have no meaning there and are rejected.
- **`env`** — per-server map exported into the child at launch, layered on top of the environment the supervisor itself inherited. Secrets belong in that inherited environment, not in this file — the committed config is the only config there is.
- **`prompt`** = `{ question, yes_args?, no_args?, yes_env?, no_env? }` — makes a server's launch args and environment an interactive y/n choice instead of a config line the operator has to hand-edit between runs. Every question is asked before *any* server launches, so a bulk `up` can't interleave two readers on one stdin. Declaring only one branch is legal; the omitted branch contributes nothing. `yes_args`/`no_args` require a type whose launch path actually reads args (`exec`, `source`) — declaring them on `mlx`/`python` is a hard config error rather than a silently discarded answer. `yes_env`/`no_env` carry no such restriction: env reaches every type, so an env-only prompt is valid on all four. The chosen branch's env merges *over* the server's static `env` for the keys it names and leaves every other key alone. A prompt answer is per-launch and never remembered, so the `build` verb re-asks before it relaunches — and only when the staleness probe means it is actually going to relaunch, so a `build` that finds nothing to do asks nothing. A prompt that cannot be answered there refuses the relaunch rather than guessing a branch, leaving the server down with its binary replaced.

**A prompt env value that restates a default defined in another module goes stale silently.** When a branch sets a variable whose default lives in another module's code — a flag default, a fallback constant — the two definitions sit either side of a module boundary and cannot be single-sourced. Change one and nothing breaks loudly: this config goes on naming a value the other side no longer uses, and the mismatch surfaces only as behavior nobody asked for. There is no mechanism that catches it, so comment both sides with a pointer to the other.
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
