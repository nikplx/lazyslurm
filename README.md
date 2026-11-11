# lazyslurm

A lazygit-style TUI frontend for Slurm, written in Go as a single static binary you can `scp` to a cluster.

- **Submit** shell commands as `sbatch` jobs from inside the TUI (CPUs / time / partition become `#SBATCH` directives; memory is sized by the cluster from the CPU count, so there is no mem setting).
- **Watch** live `squeue` status (state, time, **CPUs**, **memory**, partition, node) with vim-style navigation.
- **Tail logs** live from the app (re-read every second while following). The app injects `--output`/`--error`
  itself into `<logdir>/<name>-<jobid>.out|.err`, so every submitted job is viewable. Both streams are tailed;
  when the preferred one is empty — the usual failed-job case of empty stdout — the pane automatically
  shows the other stream with a banner. `e` toggles the preferred stream, `f` toggles follow.
- **History file** (`history.json`) records every submitted command + resulting job ids for later traceability.

## Install (copy one binary to the cluster)

```bash
# on your machine (or directly on the cluster login node):
CGO_ENABLED=0 go build -o lazyslurm .
scp lazyslurm <login-node>:~/bin/lazyslurm
# then on the cluster:
~/bin/lazyslurm
```

Or simply `make deploy`, which builds and copies the binary to `~/bin` on your
HPC.
(creating the directory if needed).

Requires `squeue`, `sbatch`, `scancel` on `PATH` (standard on any Slurm login node;
run from a login shell with your site's slurm module loaded, e.g. on your HPC the
tools come from `/sw/profile/init.sh`). `scontrol` is optional — used for extra details when available.

## Usage

```bash
lazyslurm                 # uses ~/.lazyslurm/config.json (created lazily)
lazyslurm -config /path/to/config.json
lazyslurm -version
```

Workflow:

1. Press `s` — type the shell command (e.g. `python train.py --epochs 10`), adjust name/cpus/time/partition, `enter` to submit.
   The job runs in the directory where you launched the TUI (via `#SBATCH --chdir`).
   A lone script path containing `#SBATCH` lines (`benzene_au.sh`, or `sbatch benzene_au.sh`) is
   submitted directly: only `--job-name`/`--output`/`--error` are overridden so the log viewer keeps
   working; the script's own directives (cpus, ntasks, mail, …) apply as written.
   CPUs steps from 8 to 192 in steps of 8 with `space`/`h`/`l` on the CPUs row.
   Tab in the Command field completes file names from that directory
   (`./benzene_au.sh`, `sub/dir/`, `~/…`, `/abs/…`); executables complete with a `./`
   prefix so they run directly; repeated Tab cycles through matches.
2. The new job appears in **Jobs**; `enter` tails its log in **Logs** (`f` toggles follow, `e` toggles stdout/stderr).
3. `x` cancels the selected job (with confirm). `/` filters the job list. `y` shows the command / log path in the status bar for copy-paste.
4. Everything submitted is appended to the history file; the **Submissions** pane (left) shows past commands — `enter` on one jumps to its live job, or pins its archived log (with details) when the job already finished. Long lists scroll inside their pane (`… N above · M below …`) instead of overflowing the window.

## Keybindings (vim / lazygit flavoured)

| Key | Action |
|---|---|
| `h`/`l`, `tab`/`shift+tab`, `1`/`2`/`3` | move between Submissions / Jobs / Logs |
| `j`/`k`, `↑`/`↓`, `ctrl-d`/`ctrl-u`, `g`/`G` | move / half-page / top / bottom (in Logs: scroll) |
| `s` / `c` / `n` | new submission (sbatch wrapper) |
| `enter` / `o` | Submissions→its live job (or its archived log if finished), Jobs→tail its log |
| `x` then `y`/`n` | cancel job (`scancel`) with confirm |
| `e` | toggle stdout (`.out`) / stderr (`.err`) log |
| `f` | toggle log follow |
| `/`, `enter`, `esc` | filter jobs by id/name/state/partition |
| `y` | yank: show command (Submissions) or log path (Jobs) in status bar |
| `r` | refresh squeue + history + log now |
| `d` | refresh `scontrol` details |
| `?` | help popup (`j`/`k`, `ctrl-d`/`ctrl-u` scroll) |
| `q` / `ctrl-c` | quit |
| `esc` | close popup / clear filter |

Submit popup: `tab`/`shift+tab` or `↑`/`↓` move fields, `enter` on **[ Submit ]** (or `ctrl-s` anywhere) submits, `esc` cancels.

## Files & config

Base locations default to `/beegfs/u/$USER/.lazyslurm` when that scratch
space exists, otherwise `~/.lazyslurm`. Override in `~/.lazyslurm/config.json`:

```json
{
  "default_partition": "",
  "default_cpus": 8,
  "default_time": "01:00:00",
  "default_job_name": "lsjob",
  "log_dir": "/beegfs/u/<you>/.lazyslurm/logs",
  "history_file": "/beegfs/u/<you>/.lazyslurm/history.json",
  "poll_interval_secs": 2,
  "squeue_user_only": true
}
```

An existing `~/.remoteslurm/history.json` (the old project name) is copied to
the new location once on startup, and a legacy `~/.remoteslurm/config.json` is
still honoured while `~/.lazyslurm/config.json` does not exist.

- **Logs**: `<log_dir>/<jobname>-<jobid>.out` and `.err`. If a job was submitted outside this tool, the viewer falls back to any file in the log dir containing the job id.
- **History**: JSON array, newest first — each entry has timestamp, command, resources, and job ids:
```json
[
  {
    "id": "20260926-231500.000",
    "timestamp": "2026-09-26T23:15:00Z",
    "command": "python train.py --epochs 10",
    "job_name": "exp1",
    "partition": "gpu",
    "cpus": 16,
    "time": "02:00:00",
    "job_ids": ["12345"]
  }
]
```
`squeue` only lists live jobs; finished jobs remain traceable via this file (plus `sacct`, if your cluster provides it).

## Layout

```
 lazyslurm  jobs=… hist=…  •  ? help
┌─Submissions──────┐┌─Jobs────────────┐┌─Details─────┐
│ saved commands + ││ JOBID NAME ST…  ││ CPUs/Mem/…  │
│ job ids          ││ live squeue     ││ scontrol    │
└──────────────────┘└─────────────────┘└─────────────┘
┌─Logs──────────────────────────────────────────────┐
│ tail of <name>-<jobid>.out (follow:on)            │
└───────────────────────────────────────────────────┘
 status / hints
```

The layout is responsive: on narrower or shorter terminals Details collapses
first, then the panes stack, and at very small sizes only the focused list
(plus logs when they fit) is shown. Terminal resizes — including changing the
font size — reflow the layout and re-clamp every list without drawing outside
the window. Popups are overlaid and clipped to the terminal. The help popup
scrolls with `j`/`k`, `ctrl-d`/`ctrl-u`, or `g`/`G`.

## Development

```bash
go vet ./...
go test ./...
go build -o lazyslurm .
```

Poll loop: `squeue` every `poll_interval_secs`, log file re-read every second while following.
