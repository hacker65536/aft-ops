#!/usr/bin/env bash
# Refuse organization-identifying values before they reach a public repo.
#
# git-secrets' --register-aws patterns catch credentials: an access key, or a
# 12-digit number *assigned* to something named like an account id. The values
# that leaked from this repository were neither. They were bare string
# literals in a test table, a map key named "id" (which is what AFT's real
# metadata schema calls it, not "account_id"), an account name, and a GitHub
# organization. Every one of them passed.
#
# So this scan inverts the default: a 12-digit number is refused unless
# .gitallowed names it, and identifiers are matched as plain words rather than
# as the right-hand side of an assignment. The cost is false positives, which
# is the correct trade for a repository that is public and describes real
# infrastructure.
#
# Organization-specific words are deliberately absent from this file. A
# denylist of identifying values, published in a public repository, is itself
# the disclosure. They are read at run time from git-secrets' pattern store
# (git config secrets.patterns), which is never committed — so CI, which runs
# on the public repository, enforces only the shape rules below, and the
# name-specific ones bind locally where the patterns exist.
#
# What this deliberately does NOT cover: account names. A name like
# "payments-prod" is not attributable on its own — what made the leaked values
# identifiable was the organization name sitting three files away, not the
# account name. Blocking names would mean a denylist of every real account,
# maintained forever, which cannot live in a public repo anyway (the denylist
# would itself be the leak) and would collide with the demo fixture's own
# payments-prod / platform-stg. The two rules that carry the weight cost
# nothing to maintain: the organization name, and any 12-digit number. In this
# domain the second one covers most of the first's blast radius by accident,
# because the account id is the primary key — pipelines are named
# <account-id>-customizations-pipeline, and the metadata table is keyed by id,
# so real data almost always arrives with its account id attached.
#
# Usage:
#   scan-secrets.sh            scan every tracked file
#   scan-secrets.sh --staged   scan what is about to be committed
#
# Kept POSIX-ish on purpose: macOS ships bash 3.2, so no mapfile/readarray.
set -u

root=$(git rev-parse --show-toplevel) || exit 2
cd "$root" || exit 2

allowed=.gitallowed
list=$(mktemp) || exit 2
pats=$(mktemp) || exit 2
trap 'rm -f "$list" "$pats"' EXIT

case "${1:-}" in
  --staged) git diff --cached --name-only --diff-filter=ACM > "$list" ;;
  "")       git ls-files > "$list" ;;
  *)        echo "usage: $0 [--staged]" >&2; exit 2 ;;
esac
[ -s "$list" ] || exit 0

# A reported line is dropped if it matches any allowlist entry. Keep that file
# short and every entry justified: it is the last thing between a real value
# and a push.
if [ -f "$allowed" ]; then
  grep -vE '^[[:space:]]*(#|$)' "$allowed" > "$pats" 2>/dev/null || :
fi
[ -s "$pats" ] || printf '\000NOTHING_MATCHES_THIS\000\n' > "$pats"

fail=0
rule() { # label regex [extra grep flags]
  label=$1
  re=$2
  flags=${3:-}
  # $flags unquoted on purpose: it is a flag list, empty by default.
  out=$(tr '\n' '\0' < "$list" | xargs -0 grep -nHEI $flags "$re" -- 2>/dev/null | grep -vE -f "$pats")
  if [ -n "$out" ]; then
    printf '\n[DENY] %s\n' "$label"
    printf '%s\n' "$out" | cut -c1-160 | sed 's/^/    /'
    fail=1
  fi
}

rule "AWS account id (12 digits, not allowlisted)" '\b[0-9]{12}\b'
# Organization names are NOT listed here. Writing them down in a file this
# repository publishes would disclose the very thing the rule exists to keep
# out — and a line labelled "these words identify my employer" says more than
# an unexplained occurrence of the word ever did.
#
# They come instead from git-secrets' own pattern store, which lives in git
# config and is never committed:
#
#   git config --global --add secrets.patterns '[Ee][Xx][Aa][Mm][Pp][Ll][Ee]'
#
# Matching is case-insensitive: a name is the same name however it is cased,
# and spelling every letter as a class invites a quiet gap. Anchor patterns
# with \b when the letters could occur inside ordinary words.
locals=$(git config --get-all secrets.patterns 2>/dev/null | paste -sd'|' -)
if [ -n "$locals" ]; then
  rule "locally configured pattern (see secrets.patterns)" "$locals" -i
fi
# The prefix itself identifies nothing — it is a local naming convention. It
# is here as a shape: a full profile string is awssso-<account-name>-<account
# -id>:<role>, so this fires on the one case the 12-digit rule cannot see,
# where someone replaced the id with a placeholder but left the name.
rule "SSO profile prefix"                          'awssso-|awspoc-'
rule "AWS access key id"                           '(A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}'
rule "email address outside example.com"           '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}'

if [ "$fail" -ne 0 ]; then
  cat >&2 <<'MSG'

Refusing: the lines above look like real organization data.

Replace them with placeholders — AWS documents 123456789012, 111122223333,
444455556666 and 777788889999 for exactly this — or, only when the value is
genuinely public or synthetic, add a justified entry to .gitallowed.

To bypass once (you had better be sure): git commit --no-verify
MSG
  exit 1
fi

printf 'scan-secrets: clean (%s files)\n' "$(wc -l < "$list" | tr -d ' ')"
