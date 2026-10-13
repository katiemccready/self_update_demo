#!/usr/bin/env bash
# End-to-end demo on the host platform (Linux/macOS, bash).
# Ships v1.0.0 as a multi-file program, installs it into an install root, starts
# the launcher mid-job, ships newer versions, and shows how the restart is
# deferred until idle, or forced after the deferral limit, with state saved
# first either way. Every version is a directory: program plus data files.
set -euo pipefail
cd "$(dirname "$0")/.."

W=$(mktemp -d); trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$W"' EXIT
OS=$(go env GOOS); ARCH=$(go env GOARCH); PORT=${PORT:-18080}
go build -o "$W/updatectl" ./cmd/updatectl
go build -o "$W/updateserver" ./cmd/updateserver
go build -o "$W/launcher" ./cmd/launcher
"$W/updatectl" keygen -out "$W/keys" >/dev/null
PUB=$(cat "$W/keys/public.key")

prog() {
  local v=$1 d="$W/prog-$1"
  mkdir -p "$d/data" "$d/lib"
  go build -ldflags "-X main.version=$v -X main.publicKey=$PUB -X main.defaultUpdateURL=http://localhost:$PORT/releases" -o "$d/agent" ./cmd/agent
  echo "hello $v" > "$d/data/greeting"
  echo "engine $v" > "$d/lib/engine.dat"
}
publish() { # publish VERSION [extra updatectl flags...]
  local v=$1; shift
  prog "$v"
  "$W/updatectl" publish -key "$W/keys/private.key" -root "$W/releases" -version "$v" -os "$OS" -arch "$ARCH" \
    -dir "$W/prog-$v" -entry agent "$@" >/dev/null
}
# install NAME: a fresh install root holding version 1.0.0.
install_root() {
  "$W/updatectl" pack -dir "$W/prog-1.0.0" -entry agent -out "$W/v1.tar.gz" >/dev/null
  "$W/updatectl" bootstrap -root "$W/$1" -launcher "$W/launcher" -package "$W/v1.tar.gz" -version 1.0.0 -entry agent >/dev/null
}
current() { tr -d '\n' < "$W/$1/current"; }

publish 1.0.0
"$W/updateserver" -addr ":$PORT" -dir "$W/releases" 2>/dev/null &
sleep 1

# scenario DEFERRAL NEW_VERSION: the workload is a 5-step job, 1s per step.
scenario() {
  local deferral=$1 newv=$2 name="root-$1"
  install_root "$name"
  echo; echo "=== max-restart-deferral=$deferral (publishing $newv mid-job)"
  "$W/$name/launcher" -update-interval 2s -confirm-after 3s -step 1s -state "$W/state-$1.json" \
      -max-restart-deferral "$deferral" 2>&1 \
    | grep -E "update|restart|resum|complete|saved|starting|files loaded|step=" | sed -E 's/^[0-9\/]+ [0-9:]+ //' &
  local pid=$!
  sleep 2; publish "$newv"
  sleep 13
  pkill -TERM -f "$W/$name/launcher" 2>/dev/null || true; wait $pid 2>/dev/null || true
  echo "state on disk: $(cat "$W/state-$1.json")   current version: $(current "$name")"
}

scenario 0   1.1.0   # wait for the job to finish, then restart
scenario 2s  1.2.0   # force after 2s: job interrupted, saved, resumed in new version

# Prompted mode: the update installs in the background, but nothing restarts
# until the user types "restart" (simulated here by a delayed echo).
echo; echo "=== update-mode=prompt (1.3.0, user approves ~8s later)"
install_root root-prompt
publish 1.3.0 -notes "Faster sync; fixes a crash on resume."
{ sleep 9; echo restart; sleep 100; } | "$W/root-prompt/launcher" -update-interval 2s -confirm-after 3s -step 1s \
    -state "$W/state-prompt.json" -update-mode prompt -max-restart-deferral 1s 2>&1 \
  | grep -E "update|restart|Update|Type|user|starting|complete|notes" | sed -E 's/^[0-9\/]+ [0-9:]+ //' &
sleep 16
pkill -TERM -f "$W/root-prompt/launcher" 2>/dev/null || true
echo "current version: $(current root-prompt)"
