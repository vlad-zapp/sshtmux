# Streaming Executor Design

## Summary

Replace the polling + capture-pane executor with %output streaming and two-phase send.

## Current Flow

1. clear-history
2. set-option @sshtmux-done 0
3. send-keys `cmd; __rv=$?; tmux set-option -p @sshtmux-rv "$__rv"; tmux set-option -p @sshtmux-done 1`
4. Poll @sshtmux-done until "1"
5. capture-pane to get output
6. display-message to read @sshtmux-rv
7. postProcessOutput to strip echo and trailing blanks

## New Flow

1. Reset rv: `set-option -p -t %0 @sshtmux-rv ""`
2. Type command (no Enter): `send-keys -t %0 -l 'cmd; tmux set-option -p @sshtmux-rv $?'`
3. Consume echo: read %output channel, accumulate until buffer contains `tmux set-option -p @sshtmux-rv $?`
4. Discard echo buffer
5. Press Enter: `send-keys -t %0 '' Enter`
6. Collect output: read %output into buffer while polling @sshtmux-rv for non-empty value
7. Drain remaining %output after rv is set
8. Parse rv as exit code, trim trailing blank lines, return

Shell line: `cmd; tmux set-option -p @sshtmux-rv $?`

## What Gets Removed

- `capture-pane` step
- `clear-history` step
- `@sshtmux-done` pane option (rv doubles as done signal)
- `__rv` shell variable
- `postProcessOutput` function (echo stripping)
- `pollForDone` function

## Controller Changes

- Add buffered `outputCh chan string` (capacity ~1000) to RealController
- readLoop routes %output notifications to outputCh (non-blocking send, drop if full)
- Add `OutputCh() <-chan string` method to Controller interface
- Executor drains the channel during echo and output phases

## Echo Detection

After typing the command via `send-keys -l` (literal, no Enter), the terminal echoes back the typed text via %output. We accumulate %output messages until the buffer contains the exact tail `tmux set-option -p @sshtmux-rv $?`. Then discard the buffer and press Enter.

This works because:
- Enter hasn't been pressed, so no command output can arrive yet
- The echo is the terminal reflecting our input characters
- Long commands may wrap across terminal lines, but the tail substring match handles this

## Completion Detection

Poll @sshtmux-rv via `display-message -p -t %0 '#{@sshtmux-rv}'`. When it returns a non-empty value, the command is done.

## Ordering Guarantee

All %output notifications and command responses flow through the same control connection, processed by the single readLoop in order. When display-message returns the rv value, all %output that tmux sent before processing the display-message is already dispatched to the channel. Since set-option is the last thing the shell does, no command output comes after it.

## Race Conditions and Edge Cases

1. **Echo in multiple %output chunks** — accumulate, match substring on the full buffer
2. **Output before echo consumed** — impossible, Enter not pressed yet
3. **Stale %output from previous command** — drain channel before typing
4. **Terminal reformats long echo** — match on tail substring, not exact text
5. **Command produces no output** — poll rv with timeout, don't block on %output
6. **Command output contains `tmux set-option`** — only matters during echo phase; after Enter, collect everything
7. **Context cancellation during echo wait** — return error
8. **Context cancellation during output collection** — return partial output + error
9. **%output channel full** — non-blocking send in readLoop, generous buffer
10. **Concurrent Exec** — semaphore serializes, no change
11. **RunInit** — same two-phase approach, discard all output
12. **Binary/non-UTF8 output** — %output uses octal escaping, already handled by parser
