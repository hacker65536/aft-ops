# aft-ops — CLI demo (recorded with charmbracelet/vhs).
#
# Regenerate with:  make demo   (see docs/demo/record.sh)
#
# The driver exports:
#   PATH          -> a freshly built aft-ops binary
#   AFT_OPS_DEMO  -> docs/demo/fixture.json (offline; no AWS account or login)
#   XDG_*_HOME    -> a scratch dir, so the recorder's real config and cache
#                    stay out of the frame
#
# stderr is left visible on purpose: the target banner, the status counts and
# the cache-freshness note are part of what the tool is for.
#
# `pipeline show` is the widest output here — five columns ending in a
# 60-cell DETAIL, about 151 columns in all. Size the terminal for that (cols
# roughly = Width / FontSize) or the error text wraps onto a second line.

Output docs/demo/cli.gif

Require aft-ops

Set Shell "bash"
Set FontSize 14
Set Width 1500
Set Height 900
Set Padding 20
Set Theme "Dracula"
Set WindowBar Colorful
Set TypingSpeed 40ms

# --- hidden preamble: a clean prompt ----------------------------------------
Hide
Type `export PS1="aft-ops-demo $ "` Enter
Type `export PROMPT_COMMAND=` Enter
Type "clear" Enter
Show

# 1) Every account pipeline and its latest run, fanned out under a rate limit.
Type "aft-ops pipeline list" Sleep 400ms Enter
Sleep 6s
Hide
Type "clear" Enter
Show

# 2) Triage: just what is broken. The status cache is bypassed for the
#    selection, so this is current state, not a minutes-old note.
Type "aft-ops pipeline list --status Failed" Sleep 400ms Enter
Sleep 4s
Hide
Type "clear" Enter
Show

# 3) The same answer for a script or an agent: a versioned JSON envelope.
Type "aft-ops pipeline list --status Failed -o json | head -24" Sleep 400ms Enter
Sleep 4s
Hide
Type "clear" Enter
Show

# 4) Drill into one: current stage/action state plus recent history.
Type "aft-ops pipeline show payments-stg" Sleep 400ms Enter
Sleep 5s
Hide
Type "clear" Enter
Show

# 5) Why it failed — the terraform diagnostic, lifted out of a 200-line
#    CodeBuild log without opening the console.
Type "aft-ops pipeline logs payments-stg --summary" Sleep 400ms Enter
Sleep 5s
Hide
Type "clear" Enter
Show

# 6) Re-release everything that failed. Dry run first, always.
Type "aft-ops pipeline release --status Failed --dry-run" Sleep 400ms Enter
Sleep 5s

Sleep 1s
