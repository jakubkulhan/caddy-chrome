package caddy_chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"go.uber.org/zap"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func init() {
	caddy.RegisterModule(Middleware{})
	httpcaddyfile.RegisterHandlerDirective("chrome", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("chrome", "after", "templates")
}

type Middleware struct {
	Timeout       string         `json:"timeout,omitempty"`
	MIMETypes     []string       `json:"mime_types,omitempty"`
	ExecBrowser   *ExecBrowser   `json:"exec_browser,omitempty"`
	RemoteBrowser *RemoteBrowser `json:"remote_browser,omitempty"`
	FulfillHosts  []string       `json:"fulfill_hosts,omitempty"`
	ContinueHosts []string       `json:"continue_hosts,omitempty"`
	Links         bool           `json:"links,omitempty"`
	log     *zap.Logger
	timeout time.Duration
	// In exec mode the middleware itself launches and owns the browser
	// process (cmd) and its temp user-data-dir (tempDir). Per-request work
	// always uses a fresh chromedp.NewRemoteAllocator(browserURL) so every
	// render gets its own CDP WebSocket.
	cmd        *exec.Cmd
	tempDir    string
	browserURL string
	lightpanda bool
	// proxy is started only when the middleware launched lightpanda itself
	// (exec mode). Lightpanda is told to use it via --http-proxy and the
	// per-request flow registers a render entry so the proxy can route
	// requests through the same Caddy server (or out to the network).
	proxy *renderProxy
}

type ExecBrowser struct {
	Path         string   `json:"path,omitempty"`
	DefaultFlags bool     `json:"default_flags,omitempty"`
	Flags        []string `json:"flags,omitempty"`
}

type RemoteBrowser struct {
	URL string `json:"url,omitempty"`
}

func (Middleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.chrome",
		New: func() caddy.Module { return new(Middleware) },
	}
}

func (m *Middleware) Provision(ctx caddy.Context) (err error) {
	if len(m.MIMETypes) == 0 {
		m.MIMETypes = []string{"text/html"}
	}

	if m.ExecBrowser == nil && m.RemoteBrowser == nil {
		m.ExecBrowser = &ExecBrowser{DefaultFlags: true}
	}
	if m.ExecBrowser != nil && m.RemoteBrowser != nil {
		return fmt.Errorf("cannot specify both exec and remote browser")
	}

	m.log = ctx.Logger()

	if m.Timeout != "" {
		m.timeout, err = time.ParseDuration(m.Timeout)
		if err != nil {
			return err
		}
	} else {
		m.timeout = 10 * time.Second
	}

	defer func() {
		if err != nil {
			m.cleanup()
		}
	}()

	if m.ExecBrowser != nil {
		port, perr := pickFreePort()
		if perr != nil {
			return fmt.Errorf("pick debug port: %w", perr)
		}

		execPath, kind, perr := resolveBrowser(m.ExecBrowser.Path)
		if perr != nil {
			return perr
		}
		m.lightpanda = kind == browserLightpanda

		m.proxy, perr = newRenderProxy(m.log)
		if perr != nil {
			return fmt.Errorf("start render proxy: %w", perr)
		}

		var args []string
		switch kind {
		case browserLightpanda:
			args = append(args, "serve", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
			args = append(args, "--http-proxy", m.proxy.addr)
			args = append(args, m.ExecBrowser.Flags...)
		case browserChrome:
			if m.ExecBrowser.DefaultFlags {
				args = append(args, defaultChromeFlags...)
			}
			args = append(args, m.ExecBrowser.Flags...)
			m.tempDir, perr = os.MkdirTemp("", "caddy-chrome-")
			if perr != nil {
				return fmt.Errorf("create user-data-dir: %w", perr)
			}
			args = append(args, "--user-data-dir="+m.tempDir)
			args = append(args, "--remote-debugging-port="+strconv.Itoa(port))
			args = append(args, "--proxy-server="+m.proxy.addr)
			// Chrome trusts our self-signed MITM CA only for leaf certs
			// whose SPKI matches this hash. All leaves share a single key,
			// so one hash is enough.
			args = append(args, "--ignore-certificate-errors-spki-list="+m.proxy.leafSPKIHash)
		}

		m.log.Info("starting browser",
			zap.String("path", execPath),
			zap.Bool("lightpanda", m.lightpanda),
			zap.Strings("args", args))

		m.cmd = exec.Command(execPath, args...)
		m.cmd.Stdout = io.Discard
		m.cmd.Stderr = io.Discard
		if err = m.cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", execPath, err)
		}
		m.browserURL = fmt.Sprintf("http://127.0.0.1:%d/", port)

		if err = waitForBrowser(m.browserURL, 10*time.Second); err != nil {
			return fmt.Errorf("wait for browser: %w", err)
		}

		probeAlloc, probeAllocCancel := chromedp.NewRemoteAllocator(context.Background(), m.browserURL)
		defer probeAllocCancel()
		probeCtx, probeCancel := chromedp.NewContext(probeAlloc)
		defer probeCancel()
		if err = chromedp.Run(probeCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			c := chromedp.FromContext(ctx)
			bctx := cdp.WithExecutor(ctx, c.Browser)
			return logBrowserVersion(bctx, m.log, m.lightpanda)
		})); err != nil {
			return fmt.Errorf("connect to browser: %w", err)
		}

	} else {
		m.browserURL = m.RemoteBrowser.URL
		m.lightpanda = detectLightpanda(m.browserURL)
		// Verify the remote endpoint and log its version. Use a one-shot
		// connection so we don't hold a target open between requests.
		probeAlloc, probeAllocCancel := chromedp.NewRemoteAllocator(context.Background(), m.browserURL)
		defer probeAllocCancel()
		probeCtx, probeCancel := chromedp.NewContext(probeAlloc)
		defer probeCancel()
		if err = chromedp.Run(probeCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			c := chromedp.FromContext(ctx)
			bctx := cdp.WithExecutor(ctx, c.Browser)
			return logBrowserVersion(bctx, m.log, m.lightpanda)
		})); err != nil {
			return fmt.Errorf("connect to remote browser: %w", err)
		}
	}

	return nil
}

func logBrowserVersion(ctx context.Context, log *zap.Logger, lightpanda bool) error {
	protocolVersion, product, revision, userAgent, jsVersion, err := browser.GetVersion().Do(ctx)
	if err != nil {
		return err
	}
	log.Info("browser connected",
		zap.String("protocol_version", protocolVersion),
		zap.String("product", product),
		zap.String("revision", revision),
		zap.String("user_agent", userAgent),
		zap.String("js_version", jsVersion),
		zap.Bool("lightpanda", lightpanda))
	return nil
}

type browserKind int

const (
	browserChrome browserKind = iota
	browserLightpanda
)

var lightpandaBinaries = []string{"lightpanda"}

var chromeBinaries = []string{
	"google-chrome",
	"google-chrome-stable",
	"chromium",
	"chromium-browser",
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
}

var defaultChromeFlags = []string{
	"--headless",
	"--disable-gpu",
	"--no-first-run",
	"--no-default-browser-check",
	"--disable-background-networking",
	"--enable-features=NetworkService,NetworkServiceInProcess",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--disable-breakpad",
	"--disable-client-side-phishing-detection",
	"--disable-default-apps",
	"--disable-dev-shm-usage",
	"--disable-extensions",
	"--disable-hang-monitor",
	"--disable-ipc-flooding-protection",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-renderer-backgrounding",
	"--disable-sync",
	"--force-color-profile=srgb",
	"--metrics-recording-only",
	"--mute-audio",
	"--safebrowsing-disable-auto-update",
	"--enable-automation",
	"--password-store=basic",
	"--use-mock-keychain",
}

// resolveBrowser picks which browser binary to launch. If execPath is set the
// caller's choice wins and the kind is inferred from the basename. Otherwise
// PATH is searched: lightpanda first, then chrome variants.
func resolveBrowser(execPath string) (string, browserKind, error) {
	if execPath != "" {
		base := strings.ToLower(filepath.Base(execPath))
		if strings.Contains(base, "lightpanda") {
			return execPath, browserLightpanda, nil
		}
		return execPath, browserChrome, nil
	}
	for _, name := range lightpandaBinaries {
		if p, err := exec.LookPath(name); err == nil {
			return p, browserLightpanda, nil
		}
	}
	for _, name := range chromeBinaries {
		if filepath.IsAbs(name) {
			if _, err := os.Stat(name); err == nil {
				return name, browserChrome, nil
			}
			continue
		}
		if p, err := exec.LookPath(name); err == nil {
			return p, browserChrome, nil
		}
	}
	return "", 0, fmt.Errorf("no browser found in PATH (tried lightpanda, chrome variants); set exec <path>")
}

// waitForBrowser polls /json/version on the launched browser until it answers
// or the deadline expires.
func waitForBrowser(remoteURL string, timeout time.Duration) error {
	versionURL := strings.TrimSuffix(remoteURL, "/") + "/json/version"
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(versionURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("timeout waiting for %s: %w", versionURL, lastErr)
	}
	return fmt.Errorf("timeout waiting for %s", versionURL)
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// detectLightpanda probes the remote debugging endpoint to identify
// Lightpanda, whose CDP has quirks the middleware works around (no shadow
// DOM via DOM.getDocument, Fetch interception deadlocks on sync module
// loads, no CSS/image/font fetches).
func detectLightpanda(remoteURL string) bool {
	u := strings.TrimSuffix(remoteURL, "/") + "/json/version"
	if strings.HasPrefix(u, "ws://") {
		u = "http://" + strings.TrimPrefix(u, "ws://")
	} else if strings.HasPrefix(u, "wss://") {
		u = "https://" + strings.TrimPrefix(u, "wss://")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var info struct {
		Browser string `json:"Browser"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return false
	}
	return strings.HasPrefix(info.Browser, "Lightpanda")
}

func (m *Middleware) cleanup() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- m.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = m.cmd.Process.Kill()
			<-done
		}
		m.cmd = nil
	}
	if m.tempDir != "" {
		_ = os.RemoveAll(m.tempDir)
		m.tempDir = ""
	}
	if m.proxy != nil {
		_ = m.proxy.close()
		m.proxy = nil
	}
}

func (m *Middleware) Cleanup() error {
	m.cleanup()
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := &Middleware{}
	err := m.UnmarshalCaddyfile(h.Dispenser)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Middleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			defaultFlags := true
			switch d.Val() {
			case "timeout":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Timeout = d.Val()
			case "mime_types":
				m.MIMETypes = d.RemainingArgs()
				if len(m.MIMETypes) == 0 {
					return d.ArgErr()
				}
			case "exec_no_default_flags":
				defaultFlags = false
				fallthrough
			case "exec":
				m.ExecBrowser = &ExecBrowser{
					DefaultFlags: defaultFlags,
				}
				flags := false
				for d.NextArg() {
					if strings.HasPrefix(d.Val(), "--") {
						flags = true
					}
					if d.Val() == "--" {
						continue
					} else if flags {
						m.ExecBrowser.Flags = append(m.ExecBrowser.Flags, d.Val())
					} else {
						m.ExecBrowser.Path = d.Val()
					}
				}
			case "url":
				m.RemoteBrowser = &RemoteBrowser{}
				if d.CountRemainingArgs() != 1 {
					return d.ArgErr()
				}
				d.NextArg()
				m.RemoteBrowser.URL = d.Val()
			case "fulfill_hosts":
				m.FulfillHosts = append(m.FulfillHosts, d.RemainingArgs()...)
			case "continue_hosts":
				m.ContinueHosts = append(m.ContinueHosts, d.RemainingArgs()...)
			case "links":
				m.Links = true
				if d.CountRemainingArgs() != 0 {
					return d.ArgErr()
				}
			default:
				return d.ArgErr()
			}
		}
	}
	return nil
}

var (
	_ caddy.Module          = (*Middleware)(nil)
	_ caddy.Provisioner     = (*Middleware)(nil)
	_ caddy.CleanerUpper    = (*Middleware)(nil)
	_ caddyfile.Unmarshaler = (*Middleware)(nil)
)
