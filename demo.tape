# VHS tape for generating README demo GIF.
# Install: go install github.com/charmbracelet/vhs@latest
# Run:     vhs demo.tape
# Output:  demo.gif

Output demo.gif

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 600
Set Padding 20
Set Theme "Catppuccin Mocha"

Type "# mesh — single-binary networking tool"
Enter
Sleep 500ms

Type "cat configs/example.yml | head -20"
Enter
Sleep 2s

Type "mesh -f configs/example.yml config"
Enter
Sleep 2s

Type "mesh -f configs/example.yml up &"
Enter
Sleep 3s

Type "mesh status"
Enter
Sleep 2s

Type "curl -s http://127.0.0.1:7777/healthz"
Enter
Sleep 1s

Type "mesh down"
Enter
Sleep 2s

Type "# Done! See README.md for more."
Enter
Sleep 1s
