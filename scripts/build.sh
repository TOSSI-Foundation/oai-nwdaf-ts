#!/bin/bash
# Build every image this deployment needs, from this repository.
#
#   ./scripts/build.sh            # everything, in dependency order
#   ./scripts/build.sh nwdaf      # the four NWDAF Go services only
#   ./scripts/build.sh nfs        # SMF, PCF, NRF  (clone upstream, patch, build)
#   ./scripts/build.sh gnbsim     # the UE simulator
#   ./scripts/build.sh fed        # oai-cn5g-fed + the deployment patch + configs
#
# WHY THIS EXISTS. The OAI network functions are deliberately NOT vendored - the
# repository ships patches against pinned upstream commits instead. That is the
# right call for licensing and for review, but it left every image tag the deploy
# scripts require to be produced by hand, from commands that existed only in one
# person's shell history. This script is that history, made executable.
#
# Env:
#   SRC_DIR   where upstream clones live        (default $HOME/oai-src)
#   FED_DIR   oai-cn5g-fed checkout             (default $HOME/oai-cn5g-fed)
#   JOBS      compile parallelism               (default nproc)
#
# TIME. The NWDAF images build in ~2 minutes total. Each C++ NF is a full
# from-source build and takes 30-120 minutes on a laptop; 'nfs' is therefore the
# slow half and prints per-NF progress so you can leave it running.
set -u

HERE=$(cd "$(dirname "$0")/.." && pwd)
SRC_DIR=${SRC_DIR:-$HOME/oai-src}
FED_DIR=${FED_DIR:-$HOME/oai-cn5g-fed}
JOBS=${JOBS:-$(nproc)}
DOCKER="sudo docker"

# Upstream repo, pinned base commit, and the image tag the deploy scripts expect.
# The commits are the ones named in the patch headers - they are the contract.
SMF_REPO=https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-smf.git
SMF_BASE=667aa8fd356d2fd569bf8aa1c610065e7be18f65
SMF_TAG=${SMF_TAG:-oai-smf:serialize}

PCF_REPO=https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-pcf.git
PCF_BASE=67aee53df01542a0deccf32874bfb90b38c547b3
PCF_TAG=${PCF_TAG:-oai-pcf:heartbeat}

NRF_REPO=https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nrf.git
NRF_BASE=b38f13d82ba7885930f899993c0257940021cf40
NRF_TAG=${NRF_TAG:-oai-nrf:nwdaf-disc-amfalias}

FED_REPO=https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-fed.git
FED_BASE=55859262fbcdaf6c89d8ccf9caed70ef432e4cb3

GNBSIM_UPSTREAM=${GNBSIM_UPSTREAM:-rohankharade/gnbsim:latest}

RC=0
say(){ echo; echo "──── $* ────"; }
ok(){  echo "  OK    $*"; }
bad(){ echo "  FAIL  $*"; RC=1; }

# Clone at a pinned commit, or reuse an existing clone. Never touches a checkout
# that already carries local work: if the tree is dirty we assume it is yours and
# build it as-is, because silently discarding a hand-edit is worse than a stale
# build you can see in the log.
prepare(){ # name repo base
  local name=$1 repo=$2 base=$3 d="$SRC_DIR/$1"
  mkdir -p "$SRC_DIR"
  if [ ! -d "$d/.git" ]; then
    echo "  cloning $name"
    git clone --quiet --recurse-submodules "$repo" "$d" || { bad "clone $name"; return 1; }
  fi
  if [ -n "$(git -C "$d" status --porcelain 2>/dev/null)" ]; then
    echo "  $name: working tree is dirty - building it AS IS, not re-patching"
    return 2
  fi
  git -C "$d" checkout --quiet "$base" 2>/dev/null || { bad "$name: base commit $base not found"; return 1; }
  git -C "$d" submodule update --quiet --init --recursive 2>/dev/null
  return 0
}

apply_patch(){ # dir patchfile
  local d=$1 p=$2
  git -C "$d" apply --check "$p" 2>/dev/null || { bad "patch does not apply: $(basename "$p")"; return 1; }
  git -C "$d" apply "$p" && ok "applied $(basename "$p")"
}

# ---------------------------------------------------------------- nwdaf ------
build_nwdaf(){
  say "NWDAF services (Go)"
  # tag = exactly what scripts/deploy/start_nwdaf.sh and demo_reset_multi.sh pin.
  # Do not "tidy" these into :latest - the deploy scripts check for these names
  # and the tags encode which behaviour the image has.
  local specs="oai-nwdaf-engine:docker/Dockerfile.engine:oai-nwdaf-engine:pathhealth
oai-nwdaf-nbi-analytics:docker/Dockerfile.nbi-analytics:oai-nwdaf-nbi-analytics:pathhealth
oai-nwdaf-nbi-events:docker/Dockerfile.nbi-events:oai-nwdaf-nbi-events:final
oai-nwdaf-sbi:docker/Dockerfile.sbi:oai-nwdaf-sbi:qosmon-retain"
  local c df tag
  while IFS= read -r spec; do
    [ -n "$spec" ] || continue
    c=${spec%%:*}; spec=${spec#*:}
    df=${spec%%:*}; tag=${spec#*:}
    printf "  %-26s -> %-38s " "$c" "$tag"
    if $DOCKER build -q -f "$HERE/nwdaf/$c/$df" -t "$tag" "$HERE/nwdaf/$c" >/dev/null 2>"$HERE/.build-$c.log"; then
      echo "built"
    else
      echo "FAILED"; RC=1; tail -8 "$HERE/.build-$c.log" | sed 's/^/        /'
    fi
  done <<<"$specs"
  # The SBI is started twice during a bring-up under two different tags - see the
  # header of start_nwdaf.sh. Give it the second one so neither path is missing.
  # Never move a tag the user already has - it would silently swap the image
  # under a running deployment. Only create the alias if it is absent.
  if $DOCKER image inspect oai-nwdaf-sbi:nwdaf-hardening >/dev/null 2>&1; then
    ok "oai-nwdaf-sbi:nwdaf-hardening already exists - left alone"
  else
    $DOCKER tag oai-nwdaf-sbi:qosmon-retain oai-nwdaf-sbi:nwdaf-hardening 2>/dev/null \
      && ok "oai-nwdaf-sbi:nwdaf-hardening (alias of :qosmon-retain)"
  fi
}

# ------------------------------------------------------------------ nfs ------
build_one_nf(){ # name repo base tag dockerfile patch...
  local name=$1 repo=$2 base=$3 tag=$4 dockerfile=$5; shift 5
  say "$name  ->  $tag"
  local d="$SRC_DIR/$name" st
  prepare "$name" "$repo" "$base"; st=$?
  [ $st -eq 1 ] && return 1
  if [ $st -eq 0 ]; then
    for p in "$@"; do apply_patch "$d" "$p" || return 1; done
  fi
  echo "  building (this is a full C++ build - expect 30-120 min)"
  if $DOCKER build --network host -f "$d/$dockerfile" -t "$tag" "$d" >"$HERE/.build-$name.log" 2>&1; then
    ok "$tag"
  else
    bad "$tag  (see .build-$name.log)"; tail -12 "$HERE/.build-$name.log" | sed 's/^/        /'
  fi
}

build_nfs(){
  # SMF: the patch does not carry the three NEW files, by design - a git diff
  # cannot add an untracked file. Copy them in, then the patch adds the
  # CMakeLists entry that compiles them.
  local d="$SRC_DIR/oai-cn5g-smf" st
  say "oai-cn5g-smf  ->  $SMF_TAG"
  prepare oai-cn5g-smf "$SMF_REPO" "$SMF_BASE"; st=$?
  if [ $st -eq 0 ]; then
    apply_patch "$d" "$HERE/patches/smf/01-nwdaf-consumer-and-steering.patch" || return 1
    cp "$HERE"/patches/smf/smf_nwdaf_consumer.cpp \
       "$HERE"/patches/smf/smf_nwdaf_consumer.hpp \
       "$HERE"/patches/smf/smf_nwdaf_steer_serialization.hpp "$d/src/smf_app/" \
      && ok "copied the 3 new smf_nwdaf_* sources"
  fi
  [ $st -ne 1 ] && {
    echo "  building (full C++ build - expect 30-120 min)"
    $DOCKER build --network host -f "$d/docker/Dockerfile.smf.ubuntu" -t "$SMF_TAG" "$d" \
      >"$HERE/.build-smf.log" 2>&1 && ok "$SMF_TAG" \
      || { bad "$SMF_TAG (see .build-smf.log)"; tail -12 "$HERE/.build-smf.log" | sed 's/^/        /'; }; }

  # PCF: BOTH patches. 02 is not optional - without it the PCF vanishes from the
  # NRF ~50 s after every start and nothing is ever authorized to steer.
  build_one_nf oai-cn5g-pcf "$PCF_REPO" "$PCF_BASE" "$PCF_TAG" docker/Dockerfile.pcf.ubuntu \
    "$HERE/patches/pcf/01-dnai-authorization.patch" \
    "$HERE/patches/pcf/02-nrf-heartbeat.patch"

  # NRF: patch 02 applies inside the src/common-src SUBMODULE, not the NRF repo.
  say "oai-cn5g-nrf  ->  $NRF_TAG"
  d="$SRC_DIR/oai-cn5g-nrf"
  prepare oai-cn5g-nrf "$NRF_REPO" "$NRF_BASE"; st=$?
  if [ $st -eq 0 ]; then
    git -C "$d/src/common-src" apply "$HERE/patches/nrf/02-namf-communication-alias.patch" \
      && ok "applied 02-namf-communication-alias.patch (in src/common-src)" \
      || bad "02-namf-communication-alias.patch"
    # Patch 01 records the submodule pointer going dirty; apply only its source hunk.
    git -C "$d" apply --include='src/common/api_conversions.cpp' \
      "$HERE/patches/nrf/01-nwdaf-discovery.patch" \
      && ok "applied 01-nwdaf-discovery.patch" || bad "01-nwdaf-discovery.patch"
  fi
  [ $st -ne 1 ] && {
    echo "  building (full C++ build - expect 30-120 min)"
    $DOCKER build --network host -f "$d/docker/Dockerfile.nrf.ubuntu" -t "$NRF_TAG" "$d" \
      >"$HERE/.build-nrf.log" 2>&1 && ok "$NRF_TAG" \
      || { bad "$NRF_TAG (see .build-nrf.log)"; tail -12 "$HERE/.build-nrf.log" | sed 's/^/        /'; }; }
}

# --------------------------------------------------------------- gnbsim ------
build_gnbsim(){
  say "gnbsim (UE simulator)"
  # Not built from source and not an OAI component: this deployment uses the
  # prebuilt image, retagged. recreate_gnbsim.sh and demo_reset_multi.sh both
  # reference the bare name 'gnbsim:latest'.
  if $DOCKER image inspect gnbsim:latest >/dev/null 2>&1; then
    ok "gnbsim:latest already present"
  elif $DOCKER pull "$GNBSIM_UPSTREAM" >/dev/null 2>&1 \
       && $DOCKER tag "$GNBSIM_UPSTREAM" gnbsim:latest; then
    ok "gnbsim:latest (pulled $GNBSIM_UPSTREAM)"
  else
    bad "could not obtain $GNBSIM_UPSTREAM"
  fi
}

# ------------------------------------------------------------------ fed ------
build_fed(){
  say "oai-cn5g-fed deployment tree"
  # The compose file needs database/oai_db2.sql and healthscripts/, which live in
  # oai-cn5g-fed and are NOT duplicated here. The repo ships the compose file and
  # the configs; this puts them where the compose file expects to find them.
  if [ ! -d "$FED_DIR/.git" ]; then
    echo "  cloning oai-cn5g-fed"
    git clone --quiet "$FED_REPO" "$FED_DIR" || { bad "clone oai-cn5g-fed"; return 1; }
    git -C "$FED_DIR" checkout --quiet "$FED_BASE" || bad "fed base commit"
  fi
  local F="$FED_DIR/docker-compose"
  [ -f "$F/database/oai_db2.sql" ] && ok "database/oai_db2.sql present" \
    || bad "database/oai_db2.sql MISSING - the UEs cannot authenticate without it"
  [ -f "$F/healthscripts/mysql-healthcheck2.sh" ] && ok "healthscripts present" \
    || bad "healthscripts/mysql-healthcheck2.sh MISSING"
  mkdir -p "$F/conf" "$F/policies/steering"
  cp "$HERE"/compose/docker-compose-basic-vpp-pcf-steering.yaml \
     "$HERE"/compose/docker-compose-gnbsim-vpp-additional.yaml "$F/"   && ok "compose files installed"
  cp "$HERE"/configs/nf/ulcl_config.yaml "$F/conf/"                    && ok "ulcl_config.yaml installed"
  cp -r "$HERE"/configs/pcf-policies/. "$F/policies/steering/"         && ok "PCF steering policies installed"
  echo "  FED_DIR=$FED_DIR"
}

case "${1:-all}" in
  nwdaf)  build_nwdaf ;;
  nfs)    build_nfs ;;
  gnbsim) build_gnbsim ;;
  fed)    build_fed ;;
  all)    build_nwdaf; build_gnbsim; build_fed; build_nfs ;;
  *)      echo "usage: $0 [all|nwdaf|nfs|gnbsim|fed]"; exit 2 ;;
esac

echo
if [ $RC -eq 0 ]; then echo "build.sh: OK"; else echo "build.sh: THERE WERE FAILURES (see above)"; fi
exit $RC
