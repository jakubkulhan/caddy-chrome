package caddy_chrome

import (
	"context"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
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
	MIMETypes     []string       `json:"mime_types,omitempty"`
	ExecBrowser   *ExecBrowser   `json:"exec_browser,omitempty"`
	RemoteBrowser *RemoteBrowser `json:"remote_browser,omitempty"`
	log           *zap.Logger
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
	err = chromedp.Run(m.chromeCtx)
	if err != nil {
		return
	}

	return nil
}

func (m *Middleware) Cleanup() error {
	if m.chromeCtx != nil {
		tctx, tcancel := context.WithTimeout(m.chromeCtx, 10*time.Second)
		defer tcancel()
		if err := chromedp.Cancel(tctx); err != nil {
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
