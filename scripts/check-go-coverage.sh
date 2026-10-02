#!/usr/bin/env sh
set -eu

minimum_coverage="${LOCALPERF_MIN_COVERAGE:-85}"
profile="${TMPDIR:-/tmp}/localperf-coverage.out"

go test -coverprofile="$profile" ./internal/runner
total="$(go tool cover -func="$profile" | awk '/^total:/ {print substr($3, 1, length($3)-1)}')"

awk -v total="$total" -v minimum="$minimum_coverage" 'BEGIN {
  if (total + 0 < minimum + 0) {
    printf("coverage %.1f%% is below %.1f%%\n", total, minimum) > "/dev/stderr"
    exit 1
  }
  printf("coverage %.1f%% meets %.1f%%\n", total, minimum)
}'
