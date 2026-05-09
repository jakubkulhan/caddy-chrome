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
	"net/http"
	"strings"
	"sync"
	"time"
)

func init() {
	caddy.RegisterModule(Middleware{})
	httpcaddyfile.RegisterHandlerDirective("chrome", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("chrome", "after", "templates")
}

type Middleware struct {
	Timeout              string         `json:"timeout,omitempty"`
	MIMETypes            []string       `json:"mime_types,omitempty"`
	ExecBrowser          *ExecBrowser   `json:"exec_browser,omitempty"`
	RemoteBrowser        *RemoteBrowser `json:"remote_browser,omitempty"`
	FulfillHosts         []string       `json:"fulfill_hosts,omitempty"`
	ContinueHosts        []string       `json:"continue_hosts,omitempty"`
	Links                bool           `json:"links,omitempty"`
	ConnectionPerRequest *bool          `json:"connection_per_request,omitempty"`
	log                  *zap.Logger
	timeout              time.Duration
	chromeCtx            context.Context
	singleTarget         bool
	connPerRequest       bool
	requestMu            sync.Mutex
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

	var cancel context.CancelFunc
	if m.ExecBrowser != nil {
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
		m.chromeCtx, cancel = chromedp.NewExecAllocator(context.Background(), opts...)

	} else if m.RemoteBrowser != nil {
		m.singleTarget = detectSingleTargetBrowser(m.RemoteBrowser.URL)
		m.chromeCtx, cancel = chromedp.NewRemoteAllocator(context.Background(), m.RemoteBrowser.URL)

	} else {
		panic("unreachable")
	}

	if m.ConnectionPerRequest != nil {
		m.connPerRequest = *m.ConnectionPerRequest
	} else {
		m.connPerRequest = m.singleTarget
	}
	if m.connPerRequest && m.RemoteBrowser == nil {
		return fmt.Errorf("connection_per_request requires a remote browser url")
	}
	defer func() {
		if err != nil {
			cancel()
			m.chromeCtx = nil
		}
	}()

	// In single-target mode (e.g. Lightpanda) the browser allows only one open
	// target at a time, so we cannot keep a long-lived parent context with its
	// own target alive across requests. Use a one-shot probe instead.
	// In multi-target mode we promote m.chromeCtx to a chromedp.Context so that
	// per-request child contexts can use WithNewBrowserContext for isolation.
	probeCtx := m.chromeCtx
	var probeCancel context.CancelFunc
	if m.singleTarget {
		probeCtx, probeCancel = chromedp.NewContext(m.chromeCtx)
	} else {
		m.chromeCtx, _ = chromedp.NewContext(m.chromeCtx)
		probeCtx = m.chromeCtx
	}
	// Browser.getVersion must be issued against the browser session (not a page
	// session) because some CDP implementations (e.g. Lightpanda) only respond
	// to Browser-level commands when they are sent without a sessionId.
	err = chromedp.Run(probeCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		c := chromedp.FromContext(ctx)
		bctx := cdp.WithExecutor(ctx, c.Browser)
		protocolVersion, product, revision, userAgent, jsVersion, err := browser.GetVersion().Do(bctx)
		if err != nil {
			return err
		}
		m.log.Info("browser connected",
			zap.String("protocol_version", protocolVersion),
			zap.String("product", product),
			zap.String("revision", revision),
			zap.String("user_agent", userAgent),
			zap.String("js_version", jsVersion),
			zap.Bool("single_target", m.singleTarget),
			zap.Bool("connection_per_request", m.connPerRequest))
		return nil
	}))
	if probeCancel != nil {
		probeCancel()
	}
	if err != nil {
		return
	}

	return nil
}

// detectSingleTargetBrowser probes the remote debugging endpoint to identify
// browsers (currently Lightpanda) that only support a single target / browser
// context at a time and require serialized request handling.
func detectSingleTargetBrowser(remoteURL string) bool {
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

func (m *Middleware) Cleanup() error {
	if m.chromeCtx != nil {
		timeoutCtx, cancel := context.WithTimeout(m.chromeCtx, 10*time.Second)
		defer cancel()
		if err := chromedp.Cancel(timeoutCtx); err != nil {
			return err
		}
		m.chromeCtx = nil
	}
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
			case "connection_per_request":
				args := d.RemainingArgs()
				var v bool
				switch len(args) {
				case 0:
					v = true
				case 1:
					switch args[0] {
					case "true", "yes", "on":
						v = true
					case "false", "no", "off":
						v = false
					default:
						return d.ArgErr()
					}
				default:
					return d.ArgErr()
				}
				m.ConnectionPerRequest = &v
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
