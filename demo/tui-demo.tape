Output demo/tui-demo.gif
Set Width 1800
Set Height 920
Set Theme "Catppuccin Mocha"
Set FontSize 18
Set Margin 0
Set TypingSpeed 180ms
Set CursorBlink true

Type "ec2-spot-interrupter"
Enter
Sleep 3s

# Simple interruption flow.
Down
Down
Space
Sleep 2s
Enter
Sleep 2s
Backspace
Backspace
Backspace
Type "5s"
Enter
Sleep 12s

# Back to instances.
Type "b"
Sleep 2s

# Tag filter (Service=worker).
Type "t"
Sleep 5s
Backspace
Type "Service"
Sleep 2s
Tab
Sleep 2s
Backspace
Type "worker"
Sleep 2s
Enter
Sleep 5s

# Chaos mode flow with confirmation.
Type "c"
Sleep 2s
Backspace
Type "1"
Sleep 2s
Tab
Sleep 2s
Backspace
Backspace
Type "5s"
Sleep 3s
Enter
Sleep 8s
Enter
Sleep 11s

# Open latest active experiment monitor.
Type "e"
Sleep 10s

# Quit monitor and app.
Escape
Sleep 10s
Type "q"
Sleep 1s
