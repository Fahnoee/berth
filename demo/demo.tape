# Render with: vhs demo/demo.tape   (from the repo root; needs go, git, python3)
Output demo/demo.gif

Set Shell bash
Set FontSize 14
Set Width 1400
Set Height 620
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms

Hide
Type "source demo/setup.sh && clear" Enter
Sleep 8s
Show

Type "cd code/shop && berth port web" Enter
Sleep 1.2s
Type "cd ../shop-checkout && berth port web    # every worktree gets its own" Enter
Sleep 2s
Type "berth run -d -- python3 -m http.server {port}" Enter
Sleep 3s
Type "berth ls" Enter
Sleep 4.5s
Type "clear && berth" Enter
Sleep 3.5s
Down
Sleep 0.6s
Down
Sleep 0.6s
Enter
Sleep 2.5s
Escape
Sleep 0.8s
Type "a"
Sleep 2.5s
Type "R"
Sleep 1.5s
Type "y"
Sleep 3s
Type "a"
Sleep 2.5s
Type "q"
Sleep 0.5s
Type "berth reap --detached    # orphans someone started with &" Enter
Sleep 3.5s
