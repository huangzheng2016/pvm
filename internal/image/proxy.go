package image

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ProxyConfig is the proxy environment used only while baking a golden
// image. Loopback addresses are rewritten for the build VM's slirp network.
type ProxyConfig struct {
	HTTPProxy  string
	HTTPSProxy string
	AllProxy   string
	NoProxy    string
}

func (p ProxyConfig) enabled() bool {
	return p.HTTPProxy != "" || p.HTTPSProxy != "" || p.AllProxy != ""
}

func (p ProxyConfig) normalized() ProxyConfig {
	p.HTTPProxy = rewriteProxyURL(p.HTTPProxy)
	p.HTTPSProxy = rewriteProxyURL(p.HTTPSProxy)
	p.AllProxy = rewriteProxyURL(p.AllProxy)
	return p
}

// ProxyConfigFromEnv reads the host's proxy settings and rewrites only
// loopback proxy hosts for the build VM (10.0.2.2 on QEMU slirp).
func ProxyConfigFromEnv() ProxyConfig {
	return ProxyConfig{
		HTTPProxy:  rewriteProxyURL(os.Getenv("HTTP_PROXY")),
		HTTPSProxy: rewriteProxyURL(os.Getenv("HTTPS_PROXY")),
		AllProxy:   rewriteProxyURL(os.Getenv("ALL_PROXY")),
		NoProxy:    os.Getenv("NO_PROXY"),
	}
}

// rewriteProxyURL preserves scheme, credentials, port and path, changing
// only loopback proxy hosts to the address visible from the build VM.
func rewriteProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	normalized := raw
	if !strings.Contains(normalized, "://") {
		normalized = "http://" + normalized
	}
	u, err := url.Parse(normalized)
	if err != nil || u.Host == "" {
		return raw
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
		host := "10.0.2.2"
		if strings.HasPrefix(u.Host, "[") {
			host = "[" + host + "]"
		}
		if u.Port() != "" {
			host += ":" + u.Port()
		}
		u.Host = host
	}
	return u.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func systemdEnv(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s)
}

func (p ProxyConfig) writeProvisionEnv(b *strings.Builder) {
	p = p.normalized()
	if !p.enabled() {
		return
	}
	b.WriteString("cat > /tmp/pvm-build-proxy.env <<'EOF'\n")
	for _, item := range [][2]string{
		{"HTTP_PROXY", p.HTTPProxy},
		{"HTTPS_PROXY", p.HTTPSProxy},
		{"ALL_PROXY", p.AllProxy},
		{"NO_PROXY", p.NoProxy},
		{"http_proxy", p.HTTPProxy},
		{"https_proxy", p.HTTPSProxy},
		{"all_proxy", p.AllProxy},
		{"no_proxy", p.NoProxy},
	} {
		fmt.Fprintf(b, "export %s=%s\n", item[0], shellQuote(item[1]))
	}
	b.WriteString("EOF\nsource /tmp/pvm-build-proxy.env\n")
	b.WriteString("mkdir -p /etc/systemd/system/docker.service.d\n")
	b.WriteString("cat > /etc/systemd/system/docker.service.d/pvm-proxy.conf <<'EOF'\n")
	b.WriteString("[Service]\n")
	fmt.Fprintf(b, "Environment=\"HTTP_PROXY=%s\" \"HTTPS_PROXY=%s\" \"ALL_PROXY=%s\" \"NO_PROXY=%s\"\n",
		systemdEnv(p.HTTPProxy), systemdEnv(p.HTTPSProxy), systemdEnv(p.AllProxy), systemdEnv(p.NoProxy))
	fmt.Fprintf(b, "Environment=\"http_proxy=%s\" \"https_proxy=%s\" \"all_proxy=%s\" \"no_proxy=%s\"\n",
		systemdEnv(p.HTTPProxy), systemdEnv(p.HTTPSProxy), systemdEnv(p.AllProxy), systemdEnv(p.NoProxy))
	b.WriteString("EOF\n")
}

func (p ProxyConfig) writeCleanup(b *strings.Builder) {
	if !p.enabled() {
		return
	}
	b.WriteString("rm -f /tmp/pvm-build-proxy.env /etc/systemd/system/docker.service.d/pvm-proxy.conf\n")
	b.WriteString("systemctl daemon-reload || true\n")
}
