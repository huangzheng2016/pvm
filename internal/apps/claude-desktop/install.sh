if ! dpkg -s claude-desktop >/dev/null 2>&1; then
  apt_repo claude-desktop https://downloads.claude.ai/claude-desktop/key.asc "$claude_key_fpr" https://downloads.claude.ai/claude-desktop/apt/stable
  # the recommended packages are QEMU for Cowork; the keyring keeps the sign-in
  $apt install -y --no-install-recommends claude-desktop gnome-keyring
fi
dpkg -s claude-desktop | grep '^Version:'
