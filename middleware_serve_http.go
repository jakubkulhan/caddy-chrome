package caddy_chrome

import (
	"bytes"
	"context"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sync"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

var skipHeaders = map[string]struct{}{
	"Accept-Ranges":  {},
	"Content-Length": {},
	"Etag":           {},
	"Last-Modified":  {},
	"Vary":           {},
}

// bypassHeader is set on every sub-request the browser makes to the same
// Caddy server while we are rendering. When this middleware sees the marker
// it passes the request through to the next handler so the browser observes
// the unmodified upstream response (no recursive rendering, no recorder).
const bypassHeader = "X-Caddy-Chrome-Bypass"

func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Header.Get(bypassHeader) != "" || r.Header.Get(renderHeader) != "" {
		return next.ServeHTTP(w, r)
	}
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	recorder := caddyhttp.NewResponseRecorder(w, buf, func(code int, header http.Header) bool {
		if len(m.MIMETypes) == 0 {
			return true
		}
		contentType := header.Get("Content-Type")
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return false
		}
		for _, mimeType := range m.MIMETypes {
			if mediaType == mimeType {
				return true
			}
		}
		return false
	})
	err := next.ServeHTTP(recorder, r)
	if err != nil {
		return err
	}
	if !recorder.Buffered() {
		return nil
	}

	m.log.Debug("got response", zap.String("response", buf.String()), zap.String("content_type", recorder.Header().Get("Content-Type")))

	var scheme string
	if r.TLS == nil {
		scheme = "http"
	} else {
		scheme = "https"
	}
	navigateURL := scheme + "://" + r.Host + r.RequestURI

	// Always open a fresh WebSocket connection to the browser per request.
	// For both Chrome (with chromedp default flags) and Lightpanda, this is
	// faster than sharing one CDP connection because every render gets its
	// own root browser session — no Target.createBrowserContext / disposal
	// dance, no serialization on a single WS reader.
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), m.timeout)
	defer timeoutCancel()
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(timeoutCtx, m.browserURL)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	reqContext := r.Context()
	go func() {
		<-reqContext.Done()
		browserCancel()
	}()
	server := reqContext.Value(caddyhttp.ServerCtxKey).(http.Handler)

	links := newLinks()

	// All browser traffic is intercepted by the in-process render proxy
	// (exec mode) or routed back through Caddy via the bypass header
	// (url mode). Either way, no CDP Fetch interception is involved.
	var tasks chromedp.Tasks
	tasks = append(tasks, network.Enable())
	if m.proxy != nil {
		id := m.proxy.register(&renderEntry{
			navigateURL:   navigateURL,
			originHost:    r.Host,
			fulfillHosts:  m.FulfillHosts,
			continueHosts: m.ContinueHosts,
			server:        server,
			recorder:      recorder,
			links:         links,
			reqContext:    reqContext,
			log:           m.log,
		})
		defer m.proxy.unregister(id)
		tasks = append(tasks, network.SetExtraHTTPHeaders(network.Headers{renderHeader: id}))
	} else {
		// External browser (url mode): no proxy. Tag every request with
		// the bypass marker so this middleware short-circuits when the
		// browser fetches the navigation / sub-resources directly from
		// the same Caddy server.
		tasks = append(tasks, network.SetExtraHTTPHeaders(network.Headers{bypassHeader: "1"}))
	}
	tasks = append(tasks, runtime.Enable())
	tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
		chromedp.ListenTarget(ctx, func(event any) {
			switch event := event.(type) {
			case *network.EventRequestWillBeSent:
				reqURL, err := url.Parse(event.Request.URL)
				if err != nil {
					return
				}
				if reqURL.Host == r.Host {
					links.AddResource(event.Request.URL, event.Type)
				} else {
					links.AddPreconnect(reqURL.Scheme + "://" + reqURL.Host)
				}
			case *runtime.EventExceptionThrown:
				m.log.Error("exception thrown in runtime", zap.String("exception_details", event.ExceptionDetails.Exception.Description))
			}
		})
		return nil
	}))
	cookieDomain := r.Host
	if h, _, err := net.SplitHostPort(cookieDomain); err == nil {
		cookieDomain = h
	}
	for _, cookie := range r.Cookies() {
		tasks = append(tasks, network.SetCookie(cookie.Name, cookie.Value).WithDomain(cookieDomain))
	}
	if ua := r.UserAgent(); ua != "" {
		tasks = append(tasks, emulation.SetUserAgentOverride(ua))
	}
	tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(onNewDocumentScript).Do(ctx)
		return err
	}))
	tasks = append(tasks, chromedp.Navigate(navigateURL))
	tasks = append(tasks, chromedp.Evaluate("window.CaddyChrome.pendingTask", nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		p.AwaitPromise = true
		return p
	}))
	var serializer *domSerializer
	var serializedHTML string
	var finalURL string
	if m.lightpanda {
		// Browsers like Lightpanda support shadow DOM in JS but do not expose
		// shadow roots through CDP DOM.getDocument. Serialize the document
		// (including shadow roots as <template shadowrootmode>) in JS instead
		// of mutating the live DOM. Also capture the document's final URL so
		// we can detect cross-origin redirects (Lightpanda follows them
		// directly because we don't intercept its requests).
		tasks = append(tasks, chromedp.Evaluate("location.href", &finalURL))
		tasks = append(tasks, chromedp.Evaluate(serializeDOMScript, &serializedHTML))
	} else {
		tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
			root, err := dom.GetDocument().WithDepth(-1).WithPierce(true).Do(ctx)
			if err != nil {
				return err
			}
			serializer = &domSerializer{root: root}
			return nil
		}))
	}
	err = chromedp.Run(browserCtx, tasks)
	if err != nil {
		m.log.Info("failed to run chrome", zap.String("url", navigateURL), zap.Error(err))
		return errors.Wrap(recorder.WriteResponse(), "failed to write original response")
	}
	if m.lightpanda && finalURL != "" {
		if loc, err := url.Parse(finalURL); err == nil && loc.Host != r.Host {
			m.log.Info("page navigated cross-origin; serving original response",
				zap.String("url", navigateURL), zap.String("final_url", finalURL))
			return errors.Wrap(recorder.WriteResponse(), "failed to write original response")
		}
	}

	headers := recorder.Header().Clone()
	for name, _ := range w.Header() {
		w.Header().Del(name)
	}
	for name, values := range headers {
		if _, exists := skipHeaders[name]; exists {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}

	if m.Links {
		links.MakeHeaders(w.Header())
	}

	w.WriteHeader(recorder.Status())

	if serializer != nil {
		if err := serializer.Serialize(w); err != nil {
			return errors.Wrap(err, "failed to serialize")
		}
	} else if _, err := io.WriteString(w, serializedHTML); err != nil {
		return errors.Wrap(err, "failed to write serialized response")
	}

	return nil
}

var (
	_ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
)
