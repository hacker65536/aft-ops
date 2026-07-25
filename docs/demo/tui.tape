# aft-ops — TUI demo: the four-level drill-down.
#
#   Pipeline list --> Executions --> Actions --> Log
#
# Regenerate with:  make demo   (see docs/demo/record.sh)
# Offline: the driver points AFT_OPS_DEMO at docs/demo/fixture.json.

Output docs/demo/tui.gif

Require aft-ops

Set Shell "bash"
Set FontSize 15
Set Width 1400
Set Height 720
Set Padding 20
Set Theme "Dracula"
Set WindowBar Colorful
Set TypingSpeed 45ms

Hide
Type `export PS1="aft-ops-demo $ "` Enter
Type `export PROMPT_COMMAND=` Enter
Type "clear" Enter
Show

# Bare `aft-ops` launches the TUI.
Type "aft-ops" Sleep 400ms Enter
Sleep 5s

# f cycles the status filter: all -> Failed. Four rows want attention.
Type "f"
Sleep 2500ms

# l drills in: the executions of payments-stg, newest first, with the
# commit message that triggered each one.
Type "l"
Sleep 3s

# l again: the actions of that run, in pipeline order — the two source
# actions, then terraform twice (global, then account customizations).
Type "l"
Sleep 3s

# Walking down to the failed Apply. Each build's terraform verdict is
# fetched lazily from its log and shown under the table for the selected
# row: the API's own summary says a command exited non-zero, the verdict
# says which resource it was.
Down
Sleep 900ms
Down
Sleep 1200ms
Down
Sleep 3500ms

# Open its log.
Type "l"
Sleep 3s

# The log opens on the terraform portion — the CodeBuild preamble and the
# POST_BUILD tail are cut. m cycles terraform -> raw -> summary.
Type "m"
Sleep 2500ms
Type "m"
Sleep 3s
Type "m"
Sleep 1500ms

# less-style search: / to search, n for the next match.
Type "/"
Sleep 600ms
Type "Error"
Sleep 600ms
Enter
Sleep 2500ms

# ] moves to the other build of the same run: an AFT customizations run
# executes terraform twice, so "the log" is really two logs.
Type "]"
Sleep 3s

# h walks back up: log -> actions -> executions -> list.
Type "h"
Sleep 900ms
Type "h"
Sleep 900ms
Type "h"
Sleep 1500ms

# v from the list jumps straight to the most relevant log of another
# pipeline — the failed action's, skipping the two screens in between.
Down
Sleep 500ms
Type "v"
Sleep 4s

Type "h"
Sleep 1200ms
Type "q"
Sleep 1200ms
