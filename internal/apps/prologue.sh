set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
id -u "${PVM_USER:?}" >/dev/null
apt="apt-get -q -o DPkg::Lock::Timeout=300"
command -v curl >/dev/null && command -v gpg >/dev/null || {
  $apt update
  $apt install -y --no-install-recommends ca-certificates curl gpg
}

# Anthropic signs the claude-code and claude-desktop repositories with it.
claude_key_fpr=31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE

# apt_repo NAME KEY_URL FINGERPRINT URI adds a signed APT repository. It
# fails unless the downloaded key has exactly one primary key with the
# pinned fingerprint.
apt_repo() {
  local key fpr
  key=$(mktemp)
  curl -fsSL --retry 3 --connect-timeout 20 --max-time 120 "$2" -o "$key"
  fpr=$(GNUPGHOME=$(mktemp -d) gpg --show-keys --with-colons "$key" | awk -F: '$1=="pub"{n++} $1=="fpr"&&f==""{f=$10} END{if(n==1)print f}')
  if [ "$fpr" != "$3" ]; then
    echo "$1 signing key fingerprint mismatch: got '$fpr'" >&2
    exit 1
  fi
  install -d -m 0755 /etc/apt/keyrings
  install -m 0644 "$key" "/etc/apt/keyrings/$1.asc"
  rm -f "$key"
  cat > "/etc/apt/sources.list.d/$1.sources" <<EOF
Types: deb
URIs: $4
Suites: stable
Components: main
Signed-By: /etc/apt/keyrings/$1.asc
EOF
  $apt update
}
