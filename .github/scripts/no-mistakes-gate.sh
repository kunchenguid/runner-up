#!/usr/bin/env bash
#
# Decides whether a pull request is release-please's own release PR, so
# release.yml's release-pr-gate-status job may stamp the required statuses on
# it. release-please opens its PRs with GITHUB_TOKEN, so GitHub creates no
# workflow runs for them and nothing else can report the required contexts
# there.
#
# This script does not decide the ordinary "PR must be raised via no-mistakes"
# required check. The shared composite action
# kunchenguid/no-mistakes/.github/actions/require-no-mistakes, pinned in
# .github/workflows/no-mistakes-required.yml, decides it, including its bot
# exemptions and the pipeline attestation.
#
# The test is deliberately STRUCTURAL, never author identity or an attestation.
# A PR passes only when all three hold:
#   1. its head branch starts with release-please's reserved prefix,
#   2. its head repository is this repository (a fork cannot fake that), and
#   3. its body carries release-please's generated footer.
# If release-please is ever switched to a PAT, its PRs could arrive as the human
# `kunchenguid`, who also opens ordinary human PRs. Exempting that login would
# exempt every human PR too.
#
# Inputs (environment): PR_BODY, PR_NUMBER, PR_HEAD_REF, PR_HEAD_REPO,
# PR_BASE_REPO.
# Exit status: 0 = a structural release-please PR, 1 = anything else.
set -eu

RELEASE_PLEASE_MARKER='This PR was generated with [Release Please]'
RELEASE_PLEASE_BRANCH_PREFIX='release-please--'
RELEASE_PLEASE_LEGACY_BRANCH_PREFIX='release-please/'

pr_body="${PR_BODY:-}"
pr_number="${PR_NUMBER:-unknown}"
pr_head_ref="${PR_HEAD_REF:-}"
pr_head_repo="${PR_HEAD_REPO:-}"
pr_base_repo="${PR_BASE_REPO:-}"

# Condition 1: a branch under release-please's reserved branch prefix. The
# legacy `release-please/` prefix is accepted for older release-please setups.
is_release_please_branch() {
    case "$pr_head_ref" in
        "$RELEASE_PLEASE_BRANCH_PREFIX"* | "$RELEASE_PLEASE_LEGACY_BRANCH_PREFIX"*) return 0 ;;
        *) return 1 ;;
    esac
}

# Condition 2: same-repo head. A fork can copy the branch name and the body but
# cannot make its head repository be this repository.
is_same_repo_head() {
    [ -n "$pr_head_repo" ] && [ -n "$pr_base_repo" ] && [ "$pr_head_repo" = "$pr_base_repo" ]
}

# Condition 3: release-please's generated body footer.
has_release_please_footer() {
    printf '%s' "$pr_body" | grep -qF -- "$RELEASE_PLEASE_MARKER"
}

if is_release_please_branch && is_same_repo_head && has_release_please_footer; then
    echo "PR #${pr_number} is a release-please release PR (same-repo branch '${pr_head_ref}' with the Release Please footer)."
    exit 0
fi

{
    echo "PR #${pr_number} is not a structural release-please PR."
    echo "It needs all three: a '${RELEASE_PLEASE_BRANCH_PREFIX}' (or legacy"
    echo "'${RELEASE_PLEASE_LEGACY_BRANCH_PREFIX}') head branch, a same-repository head,"
    echo "and the Release Please body footer."
    echo "Head branch: ${pr_head_ref:-unknown}"
    echo "Head repository: ${pr_head_repo:-unknown} (base ${pr_base_repo:-unknown})"
} >&2
exit 1
