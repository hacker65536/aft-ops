# aft-ops — CLI demo: watching the runs that are still going.
#
# `pipeline list --watch` redraws on an interval, refetching only the rows
# that can still change: the freshness line under each frame reports how
# many statuses were refetched versus served from cache, so what the watch
# costs in API calls is visible rather than assumed.
#
# Regenerate with:  make demo   (see docs/demo/record.sh)

Output docs/demo/watch.gif

Require aft-ops

Set Shell "bash"
Set FontSize 15
Set Width 1300
Set Height 420
Set Padding 20
Set Theme "Dracula"
Set WindowBar Colorful
Set TypingSpeed 40ms

Hide
Type `export PS1="aft-ops-demo $ "` Enter
Type `export PROMPT_COMMAND=` Enter
Type "clear" Enter
Show

# Everything that is running or broken, refreshed every five seconds.
# In-flight statuses are refetched every pass regardless of the cache TTL —
# they are exactly what a watcher is waiting on.
Type "aft-ops pipeline list --watch --interval 5s --status InProgress,Failed" Sleep 400ms Enter

# Frames: the running rows tick upward, then one finishes and drops out of
# the filter while the other keeps going.
Sleep 32s

Ctrl+C
Sleep 1500ms
