if ! dpkg -s claude-code >/dev/null 2>&1; then
  apt_repo claude-code https://downloads.claude.ai/keys/claude-code.asc "$claude_key_fpr" https://downloads.claude.ai/claude-code/apt/stable
  $apt install -y --no-install-recommends claude-code
fi
claude --version
