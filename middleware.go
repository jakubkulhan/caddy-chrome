package caddy_chrome

import (
	"context"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"go.uber.org/zap"
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
	chromeCtx     context.Context
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
		m.chromeCtx, cancel = chromedp.NewRemoteAllocator(context.Background(), m.RemoteBrowser.URL)

	} else {
		panic("unreachable")
	}
	m.chromeCtx, _ = chromedp.NewContext(m.chromeCtx)
	defer func() {
		if err != nil {
			cancel()
			m.chromeCtx = nil
		}
	}()
	err = chromedp.Run(m.chromeCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		protocolVersion, product, revision, userAgent, jsVersion, err := browser.GetVersion().Do(ctx)
		if err != nil {
			return err
		}
		m.log.Info("browser connected",
			zap.String("protocol_version", protocolVersion),
			zap.String("product", product),
			zap.String("revision", revision),
			zap.String("user_agent", userAgent),
			zap.String("js_version", jsVersion))
		return nil
	}))
	if err != nil {
		return
	}

	return nil
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
