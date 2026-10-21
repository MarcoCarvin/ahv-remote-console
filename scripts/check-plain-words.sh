#!/usr/bin/env bash
# Fails when Markdown documentation uses filler or hype words.
# Usage: scripts/check-plain-words.sh [file...]   (default: every tracked *.md)
# Generated and third-party files are skipped. To allow a word in a real name,
# a quoted message or a code identifier, wrap it in backticks.
set -euo pipefail

words='seamless(ly)?|robust|leverag(e|es|ed|ing)|delve|empower|cutting-edge|state-of-the-art|powerful|magic(al)?|effortless(ly)?|intuitive|comprehensive|crucial|pivotal|holistic|synergy|streamlin(e|ed|es|ing)|elevate|unlock|harness|tapestry|testament|game-changer|best-in-class|world-class|next-generation|boasts|showcase|utiliz(e|es|ed|ing)|it is worth noting|it.s important to note|at its core|under the hood|out of the box|plays a key role|dive into|deep dive'

if [ "$#" -gt 0 ]; then
  files=("$@")
else
  mapfile -t files < <(git ls-files '*.md' | grep -v -E '^THIRD_PARTY_NOTICES\.md$')
fi

status=0
for f in "${files[@]}"; do
  # Drop fenced code blocks and inline code before matching.
  hits=$(awk '/^```/{c=!c; next} !c' "$f" | sed -E 's/`[^`]*`//g' | grep -n -i -w -E "$words" || true)
  if [ -n "$hits" ]; then
    printf '%s\n' "$hits" | sed "s|^|$f:|"
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  echo "check-plain-words: replace the words above with plain wording (see CONTRIBUTING.md)." >&2
fi
exit "$status"
