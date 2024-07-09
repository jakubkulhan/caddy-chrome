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
	"time"
)

func init() {
	caddy.RegisterModule(Middleware{})
	httpcaddyfile.RegisterHandlerDirective("chrome", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("chrome", "after", "templates")
}

type Middleware struct {
	MIMETypes []string `json:"mime_types,omitempty"`
	log       *zap.Logger
	chromeCtx context.Context
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

	m.log = ctx.Logger()

	var cancel context.CancelFunc
	m.chromeCtx, cancel = chromedp.NewExecAllocator(context.Background(), chromedp.DefaultExecAllocatorOptions[:]...)
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

func (m *Middleware) Validate() error {
	if len(m.MIMETypes) == 0 {
		return fmt.Errorf("mime_types must not be empty")
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
		for d.NextBlock(0) {
			switch d.Val() {
			case "mime_types":
				m.MIMETypes = d.RemainingArgs()
				if len(m.MIMETypes) == 0 {
					return d.ArgErr()
				}
			}
		}
	}
	return nil
}

var (
	_ caddy.Module          = (*Middleware)(nil)
	_ caddy.Provisioner     = (*Middleware)(nil)
	_ caddy.CleanerUpper    = (*Middleware)(nil)
	_ caddy.Validator       = (*Middleware)(nil)
	_ caddyfile.Unmarshaler = (*Middleware)(nil)
)
