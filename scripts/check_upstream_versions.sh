#!/usr/bin/env bash
# Compare the vendor client versions pinned in this repo with the vendors'
# public releases. Prints a markdown table of components that are behind;
# exits 0 either way (a failed fetch exits non-zero so the run is visible).
#
# Tracked (public, unauthenticated source exists):
#   ZCode  - zcode.z.ai lists releases/<ver>/ with an electron-updater latest.yml
#   Qoder  - qoder.com/changelog lists desktop app versions
# Not tracked (no public source found): WorkBuddy desktop/CLI, CodeArts plugin.
set -euo pipefail
cd "$(dirname "$0")/.."
UA='Mozilla/5.0'

behind() { # behind <have> <new>: true when <new> is a higher version than <have>
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$2" ]
}
need() { [ -n "$2" ] || { echo "could not read $1" >&2; exit 1; }; }

z_have=$(grep -oP 'const APP_VERSION = "\K[0-9.]+' integrations/magpie-plugins/zcode/index.mjs || true)
z_new=$(curl -fsSL -A "$UA" https://zcode.z.ai | grep -oE 'releases/[0-9]+\.[0-9]+\.[0-9]+/' | cut -d/ -f2 | sort -uV | tail -1 || true)
need "pinned ZCode version" "$z_have"; need "latest ZCode version" "$z_new"
# A version only counts once its updater manifest is published.
curl -fsSI -A "$UA" "https://cdn-zcode.z.ai/zcode/electron/releases/$z_new/windows-x64/latest.yml" >/dev/null

# The Qoder constant is the worker runtime of a desktop app; the comment in
# index.mjs records which app version it was taken from ("Taken from app X").
q_have=$(grep -oP 'Taken from app \K[0-9.]+' integrations/magpie-plugins/qoder/index.mjs || true)
# ponytail: assumes Qoder desktop stays 0.x; widen the pattern on a 1.0 release
q_new=$(curl -fsSL -A "$UA" https://qoder.com/changelog | sed -E 's/<[^>]+>/ /g' | grep -oE '\b0\.[0-9]+\.[0-9]+\b' | sort -uV | tail -1 || true)
need "pinned Qoder app version" "$q_have"; need "latest Qoder app version" "$q_new"

out=""
behind "$z_have" "$z_new" && out+="| ZCode | \`APP_VERSION\` in \`integrations/magpie-plugins/zcode/index.mjs\` | $z_have | $z_new |"$'\n'
behind "$q_have" "$q_new" && out+="| Qoder | \`COSY_VERSION\` (taken from app $q_have) in \`integrations/magpie-plugins/qoder/index.mjs\` | app $q_have | app $q_new |"$'\n'
printf '%s' "$out"
echo "zcode: pinned=$z_have official=$z_new | qoder: pinned-app=$q_have official-app=$q_new" >&2
