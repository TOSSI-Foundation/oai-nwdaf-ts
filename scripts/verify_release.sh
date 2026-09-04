#!/bin/bash
# Verify a checkout of this repository WITHOUT touching a running lab.
#
#   ./scripts/verify_release.sh                 # verify this working copy
#   ./scripts/verify_release.sh <git-url|path>  # clone somewhere else and verify that
#
# Everything happens in a temp directory and in throwaway containers. No NF is
# restarted, no image is retagged, no MongoDB is written. Safe to run while a
# demo is live.
#
# What this CANNOT check: that steering actually works. That needs the UPF, SMF,
# PCF and UEs, and there is one lab. See docs/MULTI-UE-STEERING.md.
set -u
SRC=${1:-}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"; sudo docker rm -f verify-smf-cc >/dev/null 2>&1' EXIT
PASS=0; FAIL=0
ok(){ echo "  PASS  $1"; PASS=$((PASS+1)); }
no(){ echo "  FAIL  $1"; FAIL=$((FAIL+1)); }

echo "=== 0. obtain the tree ==="
if [ -n "$SRC" ]; then
  git clone --quiet "$SRC" "$TMP/repo" 2>/dev/null && ok "cloned $SRC" || { no "clone $SRC"; exit 1; }
else
  git -C "$(git rev-parse --show-toplevel)" archive HEAD | tar -x -C "$TMP/repo" 2>/dev/null \
    || { mkdir -p "$TMP/repo" && git -C "$(git rev-parse --show-toplevel)" archive HEAD | tar -x -C "$TMP/repo"; }
  ok "exported HEAD ($(git rev-parse --short HEAD))"
fi
R="$TMP/repo"

echo "=== 1. Go components build and test ==="
for c in oai-nwdaf-engine oai-nwdaf-sbi oai-nwdaf-nbi-analytics oai-nwdaf-nbi-events; do
  [ -d "$R/nwdaf/$c" ] || continue
  out=$(sudo docker run --rm -v "$R/nwdaf/$c":/w -w /w -e GOFLAGS=-mod=mod golang:1.20 \
        sh -c 'go build ./... 2>&1 && go test ./... 2>&1' 2>&1)
  echo "$out" | grep -qE '^(FAIL|.*\.go:[0-9]+:)' && { no "$c"; echo "$out" | tail -5 | sed 's/^/        /'; } || ok "$c build+test"
done

echo "=== 2. SMF C++ compiles ==="
BUILDER=$(for id in $(sudo docker images -a --format '{{.ID}} {{.Size}}' | awk '$2 ~ /GB/ {print $1}'); do
  sudo docker run --rm --entrypoint sh "$id" -c 'ls /openair-smf/build/smf/build/CMakeCache.txt' >/dev/null 2>&1 && echo "$id" && break; done)
if [ -n "$BUILDER" ]; then
  sudo docker rm -f verify-smf-cc >/dev/null 2>&1
  sudo docker create --name verify-smf-cc --entrypoint sleep "$BUILDER" infinity >/dev/null
  sudo docker start verify-smf-cc >/dev/null
  for f in smf_nwdaf_consumer.cpp smf_nwdaf_consumer.hpp smf_nwdaf_steer_serialization.hpp; do
    sudo docker cp "$R/patches/smf/$f" verify-smf-cc:/openair-smf/src/smf_app/$f
  done
  sudo docker exec verify-smf-cc sh -c 'cd /openair-smf/build/smf/build && make -j6' >"$TMP/cc.log" 2>&1 \
    && ok "SMF C++ compiles and links" || { no "SMF C++"; tail -8 "$TMP/cc.log" | sed 's/^/        /'; }
else
  echo "  SKIP  no cached SMF builder image on this host"
fi

echo "=== 3. SMF unit tests ==="
if g++ -std=c++17 -Wall -Wextra -I "$R/patches/smf" \
     "$R/patches/smf/tests/test_steer_serialization.cpp" -o "$TMP/t" 2>"$TMP/g.log"; then
  "$TMP/t" >"$TMP/t.log" 2>&1 && ok "serialization tests ($(grep -c PASS "$TMP/t.log") assertions)" \
    || { no "serialization tests"; tail -5 "$TMP/t.log" | sed 's/^/        /'; }
else no "serialization tests did not compile"; tail -3 "$TMP/g.log" | sed 's/^/        /'; fi

echo "=== 4. SMF patch applies to its stated base ==="
BASE=$(grep -oP '^# Base\s+:\s+\K[0-9a-f]{40}' "$R/patches/smf/01-nwdaf-consumer-and-steering.patch")
UP=${SMF_UPSTREAM:-/home/ubuntu/oai-src/oai-cn5g-smf}
if [ -n "$BASE" ] && [ -d "$UP/.git" ]; then
  git clone --quiet --no-checkout "$UP" "$TMP/smf" 2>/dev/null
  if git -C "$TMP/smf" checkout --quiet "$BASE" 2>/dev/null; then
    git -C "$TMP/smf" apply --check "$R/patches/smf/01-nwdaf-consumer-and-steering.patch" 2>"$TMP/ap.log" \
      && ok "patch applies cleanly to ${BASE:0:12}" \
      || { no "patch does NOT apply to ${BASE:0:12}"; head -5 "$TMP/ap.log" | sed 's/^/        /'; }
  else no "base commit ${BASE:0:12} not found in $UP"; fi
else echo "  SKIP  no upstream SMF clone (set SMF_UPSTREAM)"; fi

echo "=== 5. scripts and configs parse ==="
bad=0; for f in $(find "$R/scripts" -name '*.sh'); do bash -n "$f" 2>/dev/null || { bad=1; echo "        $f"; }; done
[ $bad = 0 ] && ok "shell scripts" || no "shell scripts"
bad=0; for f in $(find "$R/scripts" -name '*.py'); do python3 -m py_compile "$f" 2>/dev/null || { bad=1; echo "        $f"; }; done
[ $bad = 0 ] && ok "python scripts" || no "python scripts"
bad=0; for f in $(find "$R/compose" "$R/configs" -name '*.yaml' 2>/dev/null); do
  python3 -c "import yaml,sys; yaml.safe_load(open('$f'))" 2>/dev/null || { bad=1; echo "        $f"; }; done
[ $bad = 0 ] && ok "YAML configs" || no "YAML configs"

echo "=== 6. no credentials committed ==="
if grep -rniE '(BEGIN [A-Z ]*PRIVATE KEY|ghp_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16})' "$R" >/dev/null 2>&1; then
  no "possible credential material found"
else ok "no private keys / tokens"; fi

echo
echo "=============================================="
echo "  $PASS passed, $FAIL failed"
echo "  NOT covered: runtime steering behaviour."
echo "  That needs the lab - see docs/MULTI-UE-STEERING.md."
echo "=============================================="
[ $FAIL -eq 0 ]
