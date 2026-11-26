// Package diaglog provides bounded log rotation, log redaction and
// whitelist-only diagnostic bundle export.
package diaglog

import (
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/huangzheng2016/pvm/internal/exit"
)

var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	pemRE  = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*(?:PRIVATE KEY|CERTIFICATE)-----.*?-----END [A-Z0-9 ]*(?:PRIVATE KEY|CERTIFICATE)-----|-----BEGIN [A-Z0-9 ]*(?:PRIVATE KEY|CERTIFICATE)-----.*`)

	headerRE     = regexp.MustCompile(`(?im)^(\s*(?:Authorization|Proxy-Authorization|Cookie|Set-Cookie)\s*:\s*).*$`)
	authSchemeRE = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{6,}`)
	credURLRE    = regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/"'<>]+@`)
	kvSecretRE   = regexp.MustCompile(`(?i)(["']?(?:token|password|passwd|secret|private_key|preshared_key|api_key|apikey|access_key|secret_key|client_secret|vnc_password)["']?\s*[:=]\s*["']?)([^\s,"'&}\[\]]+)`)
	tokenRE      = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|sk-[A-Za-z0-9_-]{16,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,})\b`)

	unixHomeRE = regexp.MustCompile(`/(?:Users|home)/[^/\s:"'\\]+`)
	winHomeRE  = regexp.MustCompile(`(?i)\b[A-Z]:\\Users\\[^\\\s:"']+`)
	ipv4RE     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

var docIPv4Prefixes = []string{"192.0.2.", "198.51.100.", "203.0.113."}

// Redactor removes credentials, user paths and public IP addresses from
// diagnostic output. Private, link-local, benchmark and TEST-NET addresses
// stay readable so routing and kill-switch evidence remains useful.
// Redacting an already redacted string is a no-op.
type Redactor struct {
	mu      sync.RWMutex
	secrets map[string]string
	ordered []string
}

func NewRedactor() *Redactor {
	return &Redactor{secrets: map[string]string{}}
}

// AddSecret registers an exact credential value (token, key, password).
func (r *Redactor) AddSecret(v string) {
	if len(v) < 6 || strings.Contains(v, "[redacted") {
		return
	}
	r.add(v, "[redacted]")
}

// AddEndpoint registers an exact hostname or IP literal of a remote peer.
func (r *Redactor) AddEndpoint(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 4 || strings.Contains(v, "[") {
		return
	}
	for _, p := range docIPv4Prefixes {
		if strings.HasPrefix(v, p) {
			return
		}
	}
	r.add(v, "[host]")
}

// AddPath registers a local path prefix (such as PVM_HOME) with its label.
func (r *Redactor) AddPath(path, label string) {
	if path == "" || label == "" {
		return
	}
	r.add(path, label)
}

func (r *Redactor) add(old, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.secrets[old]; ok {
		return
	}
	r.secrets[old] = label
	r.ordered = append(r.ordered, old)
	sort.Slice(r.ordered, func(i, j int) bool { return len(r.ordered[i]) > len(r.ordered[j]) })
}

func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	for _, k := range r.ordered {
		if strings.Contains(s, k) {
			s = strings.ReplaceAll(s, k, r.secrets[k])
		}
	}
	r.mu.RUnlock()
	return Redact(s)
}

// Redact applies the built-in pattern rules without exact-match secrets.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = ansiRE.ReplaceAllString(s, "")
	s = pemRE.ReplaceAllString(s, "[redacted-pem]")
	s = headerRE.ReplaceAllString(s, "${1}[redacted]")
	s = credURLRE.ReplaceAllString(s, "${1}[redacted]@")
	s = authSchemeRE.ReplaceAllString(s, "${1} [redacted]")
	s = kvSecretRE.ReplaceAllString(s, "${1}[redacted]")
	s = tokenRE.ReplaceAllString(s, "[redacted-token]")
	s = unixHomeRE.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "/home/[") || strings.HasPrefix(m, "/Users/[") {
			return m
		}
		if strings.HasPrefix(m, "/Users/") {
			return "/Users/[user]"
		}
		return "/home/[user]"
	})
	s = winHomeRE.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.Index(strings.ToLower(m), `\users\`) + len(`\users\`)
		return m[:i] + "[user]"
	})
	s = ipv4RE.ReplaceAllStringFunc(s, func(m string) string {
		if isPublicIPv4Literal(m) {
			return "[ip]"
		}
		return m
	})
	return s
}

func isPublicIPv4Literal(s string) bool {
	for _, p := range docIPv4Prefixes {
		if strings.HasPrefix(s, p) {
			return false
		}
	}
	parts := strings.Split(s, ".")
	b := make([]byte, 4)
	for i, p := range parts {
		n := 0
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return false
		}
		b[i] = byte(n)
	}
	return !exit.IsBlockedEgressIP(b)
}

type redactWriter struct {
	w io.Writer
	r *Redactor
}

// NewRedactWriter redacts each write before passing it to w.
func NewRedactWriter(w io.Writer, r *Redactor) io.Writer {
	if r == nil {
		r = NewRedactor()
	}
	return &redactWriter{w: w, r: r}
}

func (w *redactWriter) Write(p []byte) (int, error) {
	out := w.r.Redact(string(p))
	if _, err := io.WriteString(w.w, out); err != nil {
		return 0, err
	}
	return len(p), nil
}
