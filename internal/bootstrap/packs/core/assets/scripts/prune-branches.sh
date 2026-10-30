#!/usr/bin/env bash
# prune-branches — clean stale gc/* worktree branches from all rigs.
#
# These branches are created by coding agents for worktree isolation.
# After work is merged and the remote branch deleted, local tracking
# branches persist indefinitely. This script prunes them.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY:-.}"
PRUNED=0

# Get active rig paths with their configured integration branches.
RIGS=$(gc rig list --json 2>/dev/null | jq -r '.rigs[] | select((.suspended // false) == false) | [.path, (.default_branch // "main")] | @tsv' 2>/dev/null) || exit 0
if [ -z "$RIGS" ]; then
    exit 0
fi

while IFS=$'\t' read -r rig_path default_branch; do
    # Linked worktrees have a .git file rather than a directory.
    git -C "$rig_path" rev-parse --git-dir >/dev/null 2>&1 || continue
    default_branch="${default_branch:-main}"

    # Fetch and prune remote refs.
    git -C "$rig_path" fetch --prune origin 2>/dev/null || continue

    # List gc/* branches.
    BRANCHES=$(git -C "$rig_path" branch --list 'gc/*' --format='%(refname:short)' 2>/dev/null) || continue
    if [ -z "$BRANCHES" ]; then
        continue
    fi

    CURRENT=$(git -C "$rig_path" branch --show-current 2>/dev/null) || true

    while IFS= read -r branch; do
        # Skip current branch.
        [ "$branch" = "$CURRENT" ] && continue

        # Delete if merged to default branch (safe -d, not -D).
        if git -C "$rig_path" merge-base --is-ancestor "$branch" "origin/$default_branch" 2>/dev/null; then
            git -C "$rig_path" branch -d "$branch" 2>/dev/null && PRUNED=$((PRUNED + 1))
            continue
        fi

        # Delete if remote tracking branch is gone.
        if ! git -C "$rig_path" show-ref --verify --quiet "refs/remotes/origin/$branch" 2>/dev/null; then
            git -C "$rig_path" branch -d "$branch" 2>/dev/null && PRUNED=$((PRUNED + 1))
        fi
    done <<< "$BRANCHES"
done <<< "$RIGS"

if [ "$PRUNED" -gt 0 ]; then
    echo "prune-branches: deleted $PRUNED stale branches"
fi
