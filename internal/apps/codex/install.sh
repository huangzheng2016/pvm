# The vendor installer is per user: it puts codex under the VM user's home.
home=$(getent passwd "$PVM_USER" | cut -d: -f6)
if [ ! -x "$home/.local/bin/codex" ]; then
  script=$(mktemp)
  chmod 644 "$script"
  curl -fsSL --retry 3 --connect-timeout 20 --max-time 120 https://chatgpt.com/codex/install.sh -o "$script"
  runuser -u "$PVM_USER" -- env HOME="$home" CODEX_NON_INTERACTIVE=1 sh "$script"
  rm -f "$script"
fi
ln -sfn "$home/.local/bin/codex" /usr/local/bin/codex
runuser -u "$PVM_USER" -- env HOME="$home" codex --version
