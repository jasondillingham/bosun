#!/usr/bin/env bash
# Bosun demo playground.
#
# Spins up a fresh sandbox git repo, walks through init → claim → commit →
# done → merge with a sample plan.md. Pauses between steps with a short
# explanation so you can follow along.
#
# Usage:
#   examples/demo.sh           # interactive (pauses between steps)
#   examples/demo.sh --no-wait # run all the way through
#   examples/demo.sh --keep    # don't delete the sandbox on exit
#
# Requires: bash, git, and the bosun binary (built automatically if missing).

set -euo pipefail

WAIT=true
KEEP=false
SESSIONS=4

for arg in "$@"; do
  case "$arg" in
    --no-wait) WAIT=false ;;
    --keep)    KEEP=true ;;
    -h|--help)
      sed -n '2,14p' "$0"
      exit 0
      ;;
    *) echo "unknown option: $arg" >&2; exit 1 ;;
  esac
done

# Locate the bosun repo root + binary.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BOSUN="$ROOT/bosun"
if [[ ! -x "$BOSUN" ]]; then
  echo "→ Building bosun..."
  (cd "$ROOT" && go build -o bosun ./cmd/bosun)
fi

# Pretty-print helpers.
BOLD=$(tput bold 2>/dev/null || true)
CYAN=$(tput setaf 6 2>/dev/null || true)
DIM=$(tput dim 2>/dev/null || true)
RESET=$(tput sgr0 2>/dev/null || true)

say()   { printf '\n%s▶ %s%s\n' "${BOLD}${CYAN}" "$*" "$RESET"; }
note()  { printf '%s  %s%s\n' "$DIM" "$*" "$RESET"; }
cmd()   { printf '%s  $ %s%s\n' "$DIM" "$*" "$RESET"; eval "$@"; }
pause() {
  if [[ "$WAIT" == "true" ]]; then
    printf '\n%s[press Enter to continue]%s ' "$DIM" "$RESET"
    read -r _ || true
  fi
}

# Build a fresh sandbox.
SANDBOX="$(mktemp -d -t bosun-demo.XXXXXX)"
if [[ "$KEEP" == "false" ]]; then
  trap 'rm -rf "$SANDBOX"' EXIT
fi

say "Sandbox: $SANDBOX"

PROJ="$SANDBOX/myproj"
mkdir -p "$PROJ"
cd "$PROJ"

say "Step 0 — bootstrap a fake project"
cmd "git init -q -b main"
cmd "git config user.email demo@example.com"
cmd "git config user.name 'Bosun Demo'"
mkdir -p internal/auth internal/http internal/storage
cat > README.md <<'EOF'
# Demo project
A tiny stand-in repo for the bosun playground.
EOF
cat > internal/auth/handler.go <<'EOF'
package auth
// pretend this is a real handler
EOF
cat > internal/http/router.go <<'EOF'
package http
// pretend this is a real router
EOF
cat > internal/storage/db.go <<'EOF'
package storage
// pretend this is a real db client
EOF
cmd "git add . && git commit -q -m 'initial'"
pause

say "Step 1 — write a plan with $SESSIONS session briefs"
cat > plan.md <<'EOF'
# Refactor plan

## session-1
Refactor `internal/auth/handler.go` to use the new identity provider.

## session-2
Update `internal/http/router.go` to wire the new auth middleware.
Note: this overlaps with session-1 in `internal/auth/middleware.go`.

## session-3
Migrate `internal/storage/db.go` from pgx v4 to v5.

## session-4
Reserved for follow-up work. Will not be touched in this demo.
EOF
note "plan.md written."
pause

say "Step 2 — bosun init $SESSIONS --brief plan.md"
note "Creates $SESSIONS worktrees + branches, drops BOSUN_BRIEF.md into each."
cmd "$BOSUN init $SESSIONS --brief plan.md"
pause

say "Step 3 — first look at status"
note "All sessions are WORKING, zero ahead, zero dirty, zero claimed."
cmd "$BOSUN status"
pause

say "Step 4 — simulate work across the sessions"
note "Each session worktree is at \$PROJ-bosun-<timestamp>-N. We'll cd into each and commit."

PARENT="$(dirname "$PROJ")"
BASENAME="$(basename "$PROJ")"

# Resolve session N's worktree dir by globbing the parent for the v0.10
# UID-per-worktree naming (<basename>-bosun-<YYYYMMDD-HHMMSS>-N).
# Falls back to the legacy `<basename>-bosun-N` form so old environments
# keep working.
resolve_wt() {
  local n="$1"
  local match
  match=$(ls -d "$PARENT/$BASENAME-bosun-"*"-$n" 2>/dev/null | head -n1)
  if [ -z "$match" ]; then
    match="$PARENT/$BASENAME-bosun-$n"
  fi
  printf '%s\n' "$match"
}

for i in 1 2 3 4; do
  WT="$(resolve_wt "$i")"
  case $i in
    1)
      note "session-1: edit auth handler + claim two paths"
      echo "// auth handler updated by session-1" >> "$WT/internal/auth/handler.go"
      (cd "$WT" && git add . && git commit -q -m "auth: use new identity provider")
      cmd "$BOSUN claim session-1 internal/auth/handler.go internal/auth/middleware.go"
      ;;
    2)
      note "session-2: edit router + create middleware (overlaps with session-1's claim)"
      echo "// router wires the auth middleware" >> "$WT/internal/http/router.go"
      cat > "$WT/internal/auth/middleware.go" <<'EOF'
package auth
// new middleware
EOF
      (cd "$WT" && git add . && git commit -q -m "http: wire auth middleware")
      cmd "$BOSUN claim session-2 internal/http/router.go internal/auth/middleware.go"
      ;;
    3)
      note "session-3: migrate storage to pgx v5"
      echo "// pgx v5 migration" >> "$WT/internal/storage/db.go"
      (cd "$WT" && git add . && git commit -q -m "storage: pgx v5")
      cmd "$BOSUN claim session-3 internal/storage/db.go"
      ;;
    4)
      note "session-4: intentionally left untouched (no commits, no claims)"
      ;;
  esac
done
pause

say "Step 5 — bosun status --with-overlaps"
note "session-1 and session-2 both claimed internal/auth/middleware.go."
cmd "$BOSUN status --with-overlaps"
pause

say "Step 6 — mark sessions DONE"
note "session-1 and session-3 are ready. session-2 stays WORKING; session-4 has nothing to merge."
cmd "$BOSUN done session-1 -m 'auth refactored'"
cmd "$BOSUN done session-3 -m 'pgx v5 migration complete'"
pause

say "Step 7 — status again (note the STATE column)"
cmd "$BOSUN status"
pause

say "Step 8 — bosun merge"
note "Defaults to --ready-only: only DONE sessions merge."
note "session-1 and session-3 are squash-merged onto main."
note "session-2 (not DONE) and session-4 (0 ahead) are skipped."
cmd "$BOSUN merge"
pause

say "Step 9 — final status"
note "Merged sessions cleared their state + claims; their branches still exist for inspection."
cmd "$BOSUN status"
pause

say "Step 10 — clean up every session"
note "Merged sessions (1, 3) still show ahead=1 because their squashed commits"
note "are content-equivalent but not literally on main. Same for session-2 (unmerged)."
note "All three need --force; session-4 is clean and doesn't."
cmd "$BOSUN remove session-1 --force"
cmd "$BOSUN remove session-2 --force"
cmd "$BOSUN remove session-3 --force"
cmd "$BOSUN remove session-4"
pause

say "Step 11 — main branch log shows the merged work"
cmd "git log --oneline -10"

say "Demo complete."
if [[ "$KEEP" == "true" ]]; then
  echo "Sandbox kept at: $SANDBOX"
else
  echo "Sandbox will be removed on exit (use --keep to preserve)."
fi
