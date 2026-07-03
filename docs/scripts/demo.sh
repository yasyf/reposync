#!/bin/sh
# Regenerates docs/assets/demo.png from a real `reposync sync` run.
#
# The run happens in a throwaway sandbox — its own XDG_CONFIG_HOME, local bare
# origins, and a private repo tree — so it never reads or writes your real
# reposync state, your checkouts, or the network. Three repos stage the three
# outcomes that matter: one behind origin (advanced), one with uncommitted work
# (busy, skipped), one current (up-to-date).
#
# Requires: reposync on PATH, git, and freeze (https://github.com/charmbracelet/freeze).
set -eu

cd "$(dirname "$0")/../.."

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export XDG_CONFIG_HOME="$work/config"

code="$work/code"
origins="$work/origins"
mkdir -p "$code" "$origins" "$XDG_CONFIG_HOME/reposync"

# Sandbox state: repos live under $code; a tiny idle threshold lets the
# just-created clones sync instead of reading as recent activity.
cat > "$XDG_CONFIG_HOME/reposync/state.json" <<EOF
{"default_location": "$code", "settings": {"idle_threshold": "1ms"}}
EOF

# seed <name>: a bare origin with one commit, cloned under $code/<name>.
seed() {
	git init -q --bare -b main "$origins/$1.git"
	git clone -q "$origins/$1.git" "$code/$1" 2>/dev/null
	git -C "$code/$1" checkout -q -B main
	echo "hello from $1" > "$code/$1/README.md"
	git -C "$code/$1" add README.md
	git -C "$code/$1" -c user.name=demo -c user.email=demo@demo.test commit -q -m "init $1"
	git -C "$code/$1" push -q origin main
	reposync repo add "$code/$1" > /dev/null
}

seed api
seed web
seed notes

# api falls behind: origin/main moves via a second clone.
git clone -q "$origins/api.git" "$work/pusher"
git -C "$work/pusher" -c user.name=demo -c user.email=demo@demo.test \
	commit -q --allow-empty -m "feat: land on main"
git -C "$work/pusher" push -q origin main

# notes holds work in progress: a dirty tree the sync must not clobber.
echo "wip" >> "$code/notes/README.md"

out="$work/demo.txt"
{
	printf '$ reposync sync\n'
	reposync sync
} > "$out"

freeze "$out" \
	--theme github-dark --background "#0d1117" --window --padding 24 \
	--font.size 28 --output docs/assets/demo.png

echo "wrote docs/assets/demo.png"
