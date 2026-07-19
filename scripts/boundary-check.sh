#!/usr/bin/env bash
# Boundary check for the OPEN aatoolkit module. It enforces the split's one hard
# rule from the far side: aatoolkit must not import the closed module, and must not
# contain closed/particular content. Run by the pre-commit hook AND by CI — the hook
# is a fast local aid, CI is the unbypassable backstop before anything is ever pushed.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
fail=0

# 1. No import of (or reference to) the closed module path.
if grep -rn 'github.com/iansmith/sophie' --include='*.go' --include='go.mod' . 2>/dev/null; then
  echo "BOUNDARY FAIL: aatoolkit must not reference the closed sophie module (above)." >&2
  fail=1
fi

# 2. Content denylist — closed/particular tokens that must never appear here.
#    Edit .boundary-denylist (one extended-regex per line; # starts a comment).
#    CLAUDE.md is EXEMPT: it carries a copy of the cross-project universal rules block,
#    which legitimately names the sibling repos (incl. the closed one). It is dev-meta,
#    not open product surface. Trade-off: this guard does NOT catch a closed leak inside
#    CLAUDE.md — it MUST be sanitized/reviewed by hand before the first public push
#    (repo-split-plan.md §6). See CLAUDE.md's own "Boundary check exempts this file".
if [ -f .boundary-denylist ]; then
  while IFS= read -r pat; do
    [ -z "$pat" ] && continue
    case "$pat" in \#*) continue ;; esac
    if grep -rniE "$pat" --include='*.go' --include='*.md' --exclude=CLAUDE.md . 2>/dev/null; then
      echo "BOUNDARY FAIL: denylisted term matched: /$pat/ (above)." >&2
      fail=1
    fi
  done < .boundary-denylist
fi

if [ "$fail" -eq 0 ]; then
  echo "boundary check: OK"
fi
exit "$fail"
