# VHS tape for the README demo. Regenerate with: demo/record.sh
#
# Everything it shows is real: a real aigem TUI, real tool calls against the
# sample project in demo/workspace, rendered by the real markdown renderer. Only
# the model is scripted (demo/mockmodel), so the recording is deterministic and
# needs no credentials.

Output docs/assets/demo.gif

Require aigem

Set Shell bash
Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 18
Set TypingSpeed 55ms
Set Theme "Catppuccin Mocha"

# Start in the sample project. record.sh points the config and state dirs at a
# throwaway directory, so nothing here touches a real aigem install.
Hide
Type "cd $AIGEM_DEMO_DIR && clear"
Enter
Sleep 500ms
Type "aigem --model demo/aigem-demo"
Enter
Sleep 4s
Show

Sleep 1s
Type "where are retries bounded?"
Sleep 800ms
Enter

# list_dir, grep, read_file, then the answer streaming in word by word.
Sleep 13s

# Hold on the finished answer long enough to read the table.
Sleep 4s

Hide
Ctrl+C
Sleep 500ms
