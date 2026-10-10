#!/usr/bin/env bash
# Resolve and validate the real host platform. Environment-provided
# GOOS/GOARCH values are allowed only when they match the host; otherwise
# the caller is attempting a cross build, which is not supported.
pvm_native_target() {
  local hostos hostarch
  if command -v go >/dev/null 2>&1 && hostos=$(go env GOHOSTOS) && hostarch=$(go env GOHOSTARCH); then
    :
  else
    case "$(uname -s)" in
      Darwin) hostos=darwin ;;
      Linux) hostos=linux ;;
      *) echo "error: unsupported host OS: $(uname -s)" >&2; return 1 ;;
    esac
    case "$(uname -m)" in
      arm64|aarch64) hostarch=arm64 ;;
      x86_64|amd64) hostarch=amd64 ;;
      *) echo "error: unsupported host architecture: $(uname -m)" >&2; return 1 ;;
    esac
  fi

  if [ -n "${GOOS:-}" ] && [ "$GOOS" != "$hostos" ]; then
    echo "error: GOOS=$GOOS does not match host $hostos; cross builds are not supported" >&2
    return 1
  fi
  if [ -n "${GOARCH:-}" ] && [ "$GOARCH" != "$hostarch" ]; then
    echo "error: GOARCH=$GOARCH does not match host $hostarch; cross builds are not supported" >&2
    return 1
  fi

  case "$hostos/$hostarch" in
    darwin/arm64|linux/amd64) ;;
    *)
      echo "error: unsupported host platform: $hostos/$hostarch" >&2
      return 1
      ;;
  esac

  PVM_HOSTOS="$hostos"
  PVM_HOSTARCH="$hostarch"
  export PVM_HOSTOS PVM_HOSTARCH
}
