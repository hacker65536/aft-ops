# aft-ops — TUI demo: batch Release change.
#
# space multi-selects, x opens a confirmation that re-checks the targets'
# current status, and the released rows are then watched to completion by
# the list's own poll.
#
# Regenerate with:  make demo   (see docs/demo/record.sh)
# The releases are real StartPipelineExecution calls against the fixture —
# they start runs that genuinely take release.takes to finish — but no AWS
# account is involved.

Output docs/demo/tui-release.gif

Require aft-ops

Set Shell "bash"
Set FontSize 15
Set Width 1400
Set Height 640
Set Padding 20
Set Theme "Dracula"
Set WindowBar Colorful
Set TypingSpeed 45ms

Hide
Type `export PS1="aft-ops-demo $ "` Enter
Type `export PROMPT_COMMAND=` Enter
Type "clear" Enter
Show

Type "aft-ops" Sleep 400ms Enter
Sleep 5s

# Narrow to the failures.
Type "f"
Sleep 2500ms

# space marks a row for release; the header counts the selection.
Space
Sleep 700ms
Down
Sleep 400ms
Space
Sleep 700ms
Down
Sleep 400ms
Space
Sleep 1500ms

# x opens the confirmation. It refetches the targets first: the guard must
# not decide from a cached status, and a pipeline that started running in
# the meantime is skipped rather than superseded.
Type "x"
Sleep 4s

# y commits, and the result screen reports what actually happened per
# target: started, skipped, or failed.
Type "y"
Sleep 4s

# Any key returns to the list, which refetches the released rows: they are
# no longer Failed, so they leave this filter — only the one pipeline that
# was not released is still here.
Enter
Sleep 4s

# f again, to InProgress: the three released runs, plus the two that were
# already going.
Type "f"
Sleep 3s

# From here the list polls just the running rows — terminal rows cannot
# change on their own — and they drop out of the filter as they finish.
Sleep 8s
Sleep 8s
Sleep 6s

Type "q"
Sleep 1200ms
