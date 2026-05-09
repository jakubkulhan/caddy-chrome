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
	"net"
	"net/http"
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
	log           *zap.Logger
	timeout       time.Duration
	// allocCtx owns the underlying browser process (for exec) or the
	// long-lived chromedp.RemoteAllocator wrapper. Per-request work uses a
	// fresh chromedp.NewRemoteAllocator(browserURL) so every render gets its
	// own CDP WebSocket — this is what gives concurrent requests true
	// isolation.
	allocCtx     context.Context
	allocCancel  context.CancelFunc
	keepAliveCtx context.Context
	keepCancel   context.CancelFunc
	browserURL   string
	lightpanda bool
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
		// Pick a free port for chrome to listen on so we can build a stable
		// HTTP debugging URL and reuse it for per-request remote allocators.
		port, perr := pickFreePort()
		if perr != nil {
			return fmt.Errorf("pick debug port: %w", perr)
		}
		var opts []chromedp.ExecAllocatorOption
		if m.ExecBrowser.Path != "" {
			opts = append(opts, chromedp.ExecPath(m.ExecBrowser.Path))
		}
		if m.ExecBrowser.DefaultFlags {
			opts = append(opts, chromedp.DefaultExecAllocatorOptions[:]...)
		}
		for _, flag := range m.ExecBrowser.Flags {
			parts := strings.SplitN(flag, "=", 2)
			opts = append(opts, chromedp.Flag(parts[0], parts[1]))
		}
		// Override the default --remote-debugging-port=0 so we know the URL up
		// front. chromedp only sets that flag if it's not already provided.
		opts = append(opts, chromedp.Flag("remote-debugging-port", strconv.Itoa(port)))
		m.allocCtx, m.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
		// Bootstrap a long-lived CDP connection so chrome stays running. The
		// chromedp ExecAllocator kills the chrome process when its single
		// browser connection drops, so we keep this one open for the lifetime
		// of the middleware.
		m.keepAliveCtx, m.keepCancel = chromedp.NewContext(m.allocCtx)
		if err = chromedp.Run(m.keepAliveCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			c := chromedp.FromContext(ctx)
			bctx := cdp.WithExecutor(ctx, c.Browser)
			return logBrowserVersion(bctx, m.log, false)
		})); err != nil {
			return fmt.Errorf("start chrome: %w", err)
		}
		m.browserURL = fmt.Sprintf("http://127.0.0.1:%d/", port)

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
	if m.keepCancel != nil {
		// Cancelling the keep-alive context ends the chromedp managed browser
		// connection, which lets ExecAllocator's watchdog tear down chrome.
		m.keepCancel()
		m.keepCancel = nil
	}
	if m.allocCtx != nil {
		timeoutCtx, c := context.WithTimeout(m.allocCtx, 10*time.Second)
		defer c()
		_ = chromedp.Cancel(timeoutCtx)
	}
	if m.allocCancel != nil {
		m.allocCancel()
		m.allocCancel = nil
	}
	m.allocCtx = nil
	m.keepAliveCtx = nil
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
