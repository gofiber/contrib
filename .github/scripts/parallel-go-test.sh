#!/usr/bin/env bash
# parallel-go-test.sh (refreshed)
# Recursively find go.mod files under the current directory and run tests in each module.
# - truncates tests-errors.log at start
# - runs tests in parallel (jobs = CPU cores)
# - prefers `gotestsum` when available; otherwise uses `go test`
# - on failures, appends annotated error blocks (source line + one-line context) to tests-errors.log immediately
# - filters out successful test noise (=== RUN / --- PASS / PASS / ok ...)
# - atomic appends using mkdir-lock so parallel jobs don't interleave
# - colored terminal output (OK = green, FAIL = red, WARN = yellow)
# - handles Ctrl+C / SIGTERM: kills children and cleans up
# - option: -m to run `go mod download` and `go mod vendor` in each module before tests

set -euo pipefail
IFS=$'\n\t'

JOBS="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
LOGFILE="tests-errors.log"
RUN_GOMOD=0

usage() {
  cat <<'USAGE'
Usage: ./parallel-go-test.sh [-j JOBS] [-o LOGFILE] [-m]

Options:
  -j JOBS     Number of parallel jobs (defaults to CPU cores)
  -o LOGFILE  Path to central logfile (defaults to tests-errors.log)
  -m          Run `go mod download` and `go mod vendor` in each module before running tests

Examples:
  ./parallel-go-test.sh            # default behaviour
  ./parallel-go-test.sh -m         # run `go mod download` + `go mod vendor` first
  ./parallel-go-test.sh -j 8 -m    # 8 parallel jobs and run mod step
USAGE
}

# parse options
while getopts ":j:o:mh" opt; do
  case "$opt" in
    j) JOBS="$OPTARG" ;;
    o) LOGFILE="$OPTARG" ;;
    m) RUN_GOMOD=1 ;;
    h) usage; exit 0 ;;
    \?) printf 'Unknown option: -%s\n' "$OPTARG"; usage; exit 2 ;;
    :) printf 'Option -%s requires an argument.\n' "$OPTARG"; usage; exit 2 ;;
  esac
done
shift $((OPTIND -1))

# Colors (only if stdout is a tty)
if [ -t 1 ]; then
  GREEN=$'\e[32m'
  RED=$'\e[31m'
  YELLOW=$'\e[33m'
  RESET=$'\e[0m'
else
  GREEN=""
  RED=""
  YELLOW=""
  RESET=""
fi

# truncate central logfile at start
: > "$LOGFILE"

# detect gotestsum
USE_GOTESTSUM=0
if command -v gotestsum >/dev/null 2>&1; then
  USE_GOTESTSUM=1
fi

# find all module directories (skip common vendor trees)
mapfile -t MODULE_DIRS < <(
  find . \( -path "./.git" -o -path "./vendor" -o -path "./node_modules" \) -prune -o \
    -type f -name 'go.mod' -print0 |
  xargs -0 -n1 dirname |
  sort -u
)

if [ "${#MODULE_DIRS[@]}" -eq 0 ]; then
  printf '%s\n' "No go.mod files found. Nothing to do."
  exit 0
fi

# create absolute temp dir
TMPDIR="$(mktemp -d 2>/dev/null || mktemp -d /tmp/tests-logs.XXXXXX)"
if [ -z "$TMPDIR" ] || [ ! -d "$TMPDIR" ]; then
  printf '%s\n' "Failed to create temporary directory" >&2
  exit 2
fi

# cleanup routine
cleanup_tmpdir() {
  local attempts=0
  while [ "$attempts" -lt 5 ]; do
    if rm -rf "$TMPDIR" 2>/dev/null; then
      break
    fi
    attempts=$((attempts+1))
    sleep 0.1
  done
  if [ -d "$TMPDIR" ]; then
    printf '%s\n' "WARNING: could not fully remove temporary dir: $TMPDIR"
  fi
}

# signal handling: kill children, wait, cleanup, exit
pids=()
on_interrupt() {
  printf '%b\n' "${YELLOW}Received interrupt. Killing background jobs...${RESET}"
  for pid in "${pids[@]:-}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  sleep 0.1
  for pid in "${pids[@]:-}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
  cleanup_tmpdir
  printf '%b\n' "${YELLOW}Aborted by user.${RESET}"
  exit 130
}
trap on_interrupt INT TERM

# helper: sanitize a dir to a filename-safe token
sanitize_name() {
  local d="$1"
  d="${d#./}"
  d="${d//\//__}"
  d="${d// /_}"
  printf '%s' "${d//[^A-Za-z0-9._-]/_}"
}

# annotate a module's temp log and append to central logfile atomically
# This version: only appends when failure indicators are present and strips PASS/RUN noise
annotate_and_append() {
  local src_log="$1"
  local module_dir="$2"
  local lockdir="$TMPDIR/.lock"

  # quick check: only append logs that contain failure indicators
  if ! grep -E -q '(--- FAIL:|^FAIL\b|panic:|exit status|FAIL\t|FAIL:)' "$src_log"; then
    # nothing to do (only PASS/ok output)
    rm -f "$src_log" >/dev/null 2>&1 || true
    return
  fi

  local annotated
  annotated="$(mktemp "$TMPDIR/annotated.XXXXXX")" || {
    until mkdir "$lockdir" 2>/dev/null; do sleep 0.01; done
    printf '==== %s ====\n' "$module_dir" >> "$LOGFILE"
    # filter out PASS/OK noise when falling back
    grep -v -E '^(=== RUN|--- PASS:|^PASS$|^ok\s)' "$src_log" >> "$LOGFILE" || true
    rm -f "$src_log" || true
    rmdir "$lockdir" 2>/dev/null || true
    return
  }

  # process lines in src_log, skipping PASS/RUN lines and annotating file:line occurrences
  while IFS= read -r line || [ -n "$line" ]; do
    # skip successful-test noise
    if [[ $line =~ ^(===\ RUN|---\ PASS:|^PASS$|^ok\s) ]]; then
      continue
    fi

    # match paths like path/to/file.go:LINE or file.go:LINE:COL
    if [[ $line =~ ^([^:]+\.go):([0-9]+):?([0-9]*)[:[:space:]]*(.*)$ ]]; then
      local fp="${BASH_REMATCH[1]}"
      local ln="${BASH_REMATCH[2]}"
      local col="${BASH_REMATCH[3]}"
      local rest="${BASH_REMATCH[4]}"
      local candidate=""

      if [ -f "$fp" ]; then
        candidate="$fp"
      elif [ -f "$module_dir/$fp" ]; then
        candidate="$module_dir/$fp"
      elif [ -f "./$fp" ]; then
        candidate="./$fp"
      fi

      if [ -n "$candidate" ]; then
        local start=$(( ln > 1 ? ln - 1 : 1 ))
        local end=$(( ln + 1 ))
        printf '%s\n' "---- source: $candidate:$ln ----" >> "$annotated"
        awk -v s="$start" -v e="$end" 'NR>=s && NR<=e { printf("%6d  %s\n", NR, $0) }' "$candidate" >> "$annotated"
        printf '%s\n\n' "Error: $line" >> "$annotated"
      else
        printf '%s\n' "---- (source not found) $line ----" >> "$annotated"
      fi
    else
      printf '%s\n' "$line" >> "$annotated"
    fi
  done < "$src_log"

  # append atomically under lock
  until mkdir "$lockdir" 2>/dev/null; do
    sleep 0.01
  done
  {
    printf '==== %s ====\n' "$module_dir"
    cat "$annotated"
    printf '\n\n'
  } >> "$LOGFILE"
  rm -f "$annotated" "$src_log" || true
  rmdir "$lockdir" 2>/dev/null || true
}

# run tests for one module (with optional go mod download + vendor step)
run_tests() {
  local module_dir="$1"
  local safe
  safe="$(sanitize_name "$module_dir")"
  local mod_log="$TMPDIR/$safe.log"
  mkdir -p "$(dirname "$mod_log")"

  # optionally run `go mod download` and `go mod vendor` first
  if [ "$RUN_GOMOD" -eq 1 ]; then
    local mod_step_log="$TMPDIR/$safe.modlog"
    : > "$mod_step_log"
    ( cd "$module_dir" 2>/dev/null && go mod download >>"$mod_step_log" 2>&1 ) || true
    ( cd "$module_dir" 2>/dev/null && go mod vendor >>"$mod_step_log" 2>&1 ) || true
    if [ -s "$mod_step_log" ]; then
      printf '%b\n' "${YELLOW}MOD:  ${RESET}$module_dir (go mod output appended)"
      annotate_and_append "$mod_step_log" "$module_dir"
    else
      rm -f "$mod_step_log" >/dev/null 2>&1 || true
    fi
  fi

  # run tests: always capture stdout+stderr to per-module log so nothing prints on success
  if [ "$USE_GOTESTSUM" -eq 1 ]; then
    # use gotestsum but still capture its output to file
    if ( cd "$module_dir" 2>/dev/null && gotestsum --format standard-verbose -- -test.v ./... >"$mod_log" 2>&1 ); then
      printf '%b\n' "${GREEN}OK:   ${RESET}$module_dir"
      rm -f "$mod_log" >/dev/null 2>&1 || true
      return 0
    else
      printf '%b\n' "${RED}FAIL: ${RESET}$module_dir (appending to $LOGFILE)"
      annotate_and_append "$mod_log" "$module_dir"
      return 1
    fi
  else
    # fallback to go test; capture everything
    if ( cd "$module_dir" 2>/dev/null && go test ./... -run "" -v >"$mod_log" 2>&1 ); then
      printf '%b\n' "${GREEN}OK:   ${RESET}$module_dir"
      rm -f "$mod_log" >/dev/null 2>&1 || true
      return 0
    else
      printf '%b\n' "${RED}FAIL: ${RESET}$module_dir (appending to $LOGFILE)"
      annotate_and_append "$mod_log" "$module_dir"
      return 1
    fi
  fi
}

# main launcher: spawn jobs, throttle to JOBS, wait properly
fail_count=0

for md in "${MODULE_DIRS[@]}"; do
  run_tests "$md" &
  pids+=( "$!" )

  if [ "${#pids[@]}" -ge "$JOBS" ]; then
    if wait "${pids[0]}"; then
      :
    else
      fail_count=$((fail_count+1))
    fi
    pids=( "${pids[@]:1}" )
  fi
done

# wait remaining background jobs
for pid in "${pids[@]:-}"; do
  if wait "$pid"; then :; else fail_count=$((fail_count+1)); fi
done

# final cleanup and status
cleanup_tmpdir

if [ "$fail_count" -gt 0 ]; then
  printf '\n%b\n' "${RED}Done. $fail_count module(s) failed. See $LOGFILE for details (annotated snippets included).${RESET}"
  exit 1
else
  printf '\n%b\n' "${GREEN}Done. All modules tested successfully.${RESET}"
  if [ -f "$LOGFILE" ] && [ ! -s "$LOGFILE" ]; then
    rm -f "$LOGFILE"
  fi
  exit 0
fi
