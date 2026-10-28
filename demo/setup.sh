# Builds the scene for demo.tape: a repo with a few worktrees and the kind of
# mess coding agents leave behind. Source it from bash; it needs go, git and
# python3 on PATH.
set -e
D=/tmp/berth-demo
pkill -f '^python3 -m http.server' 2>/dev/null || true
pkill -f '^sleep 100000' 2>/dev/null || true
rm -rf "$D" && mkdir -p "$D/bin" "$D/code"
(cd "$(dirname "${BASH_SOURCE[0]}")/.." && go build -o "$D/bin/berth" .)

# Stand-in agents: the process name is what berth recognises.
for agent in claude codex; do
  printf '#!%s\nberth run -d --name "$1" -- "${@:2}" >/dev/null\nsleep 100000\n' \
    "$(command -v bash)" >"$D/bin/$agent"
  chmod +x "$D/bin/$agent"
done

export HOME=$D BERTH_HOME=$D/state PATH=$D/bin:$PATH BERTH_IGNORE='t3,ttyd,*chrom*' TERM=xterm-256color COLORTERM=truecolor
unset NO_COLOR FORCE_COLOR
git config --global user.email demo@example.com
git config --global user.name demo
git config --global init.defaultBranch main
cd "$D/code"
git init -q shop && cd shop && git commit -q --allow-empty -m init
git worktree add -q ../shop-checkout -b feat/checkout
git worktree add -q ../shop-search -b feat/search
git worktree add -q ../shop-old -b spike/old

serve='python3 -m http.server {port}'
(claude web $serve >/dev/null 2>&1 &)
codex_pid=$(cd ../shop-search && { codex api $serve >/dev/null 2>&1 & echo $!; })
(cd ../shop-old && berth run -d --name web -- $serve >/dev/null)
(setsid python3 -m http.server 5173 >/dev/null 2>&1 &)
sleep 2
kill -KILL "$codex_pid"
rm -rf ../shop-old && git worktree prune

cd "$D"
PS1='\[\e[1;36m\]\W\[\e[0m\] \[\e[35m\]❯\[\e[0m\] '
set +e
