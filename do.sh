#!/usr/bin/env sh
# Do - The Simplest Build Tool on Earth.
# Documentation and examples see https://github.com/8gears/do

set -e -u # -e "Automatic exit from bash shell script on error"  -u "Treat unset variables and parameters as errors"


build() {
  CGO_ENABLED=0 go build -o .bin/proxy .
}

dockerbuild() {
  docker buildx build --platform linux/amd64 --push -t registry.gitlab.com/moebius-labs/images/ocip .
}

deploy() {
   helm upgrade -i --namespace ocip-staging --create-namespace ocip-staging ./chart
}

run() {
  USE_TLS=1 DEBUG=1 .bin/proxy registry serve
}

tests() {
  go test -v ./...
}
"$@" # <- execute the task

[ "$#" -gt 0 ] || printf "Usage:\n\t./do.sh %s\n" "($(compgen -A function | grep '^[^_]' | paste -sd '|' -))"
