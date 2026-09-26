#!/bin/sh
# One-shot: authlib -> core. Deleted in Task 6.
#
# perl -pi rather than sed -i '': sed here is GNU sed 4.9, which reads a
# separate '' as the script and every filename as garbage.
set -eu
cd "$(git rev-parse --show-toplevel)"

git mv authlib core

# Module path.
(cd core && go mod edit -module github.com/rossoctl/cortex/core)

# Import paths, and the LEFT side of replace directives, in Go files and go.mod.
find . -path ./.git -prune -o \( -name '*.go' -o -name 'go.mod' \) -print0 \
  | xargs -0 perl -pi -e 's{github\.com/rossoctl/cortex/authlib}{github.com/rossoctl/cortex/core}g'

# RIGHT side of replace directives: ../../authlib -> ../../core.
# Capture the prefix WHOLE. A starred group keeps only its last repetition,
# which turns ../../authlib into ../core -- wrong depth, silently.
find . -path ./.git -prune -o -name go.mod -print0 \
  | xargs -0 perl -pi -e 's{(=>\s*[./]*)authlib}{$1core}g'

# Path and prose references in non-Go files.
#  - '*Makefile' / '*Dockerfile*', not 'Makefile' / 'Dockerfile*': a pathspec with
#    no glob matches at the ROOT ONLY, hiding 4 nested Dockerfiles that COPY authlib/
#  - '*.py' included: its absence broke pytest at collection in #1134
#  - no trailing slash required: that omission let 16 spellings survive #1134
#  - docs/superpowers/ is a dated archive; install.sh is excluded entirely
git ls-files -z -- '*.yaml' '*.yml' '*.md' '*.sh' '*.py' '*Makefile' '*.toml' '*Dockerfile*' 'go.work' \
  | grep -zv '^docs/superpowers/' | grep -zv '^install' \
  | xargs -0 perl -pi -e '
      s{github\.com/rossoctl/cortex/authlib}{github.com/rossoctl/cortex/core}g;
      s{\bauthlib/}{core/}g;
      s{\bcd authlib\b}{cd core}g;
      s{-C authlib\b}{-C core}g;
      s{working-directory: authlib\b}{working-directory: core}g;
      s{\./authlib\b}{./core}g;
      s{directory: /authlib\b}{directory: /core}g;
      s{\bauthlib\b}{core}g;
    '

for m in $(find . -path ./.git -prune -o -name go.mod -print); do
  (cd "$(dirname "$m")" && GOWORK=off go mod tidy)
done
