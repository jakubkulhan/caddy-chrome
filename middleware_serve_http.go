package caddy_chrome

import (
	"bytes"
	"context"
	"encoding/base64"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
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
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
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
	if r.Header.Get(bypassHeader) != "" {
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

	if m.singleTarget && !m.connPerRequest {
		m.requestMu.Lock()
		defer m.requestMu.Unlock()
	}

	var browserCtx context.Context
	var browserCancel context.CancelFunc
	if m.connPerRequest {
		// Open a fresh WebSocket to the remote browser for this request.
		// Lightpanda 0.2.4+ gives every CDP connection its own browser, so
		// concurrent requests get full isolation without needing a mutex or
		// WithNewBrowserContext.
		timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), m.timeout)
		defer timeoutCancel()
		allocCtx, allocCancel := chromedp.NewRemoteAllocator(timeoutCtx, m.RemoteBrowser.URL)
		defer allocCancel()
		var bcancel context.CancelFunc
		browserCtx, bcancel = chromedp.NewContext(allocCtx)
		browserCancel = bcancel
	} else {
		timeoutCtx, timeoutCancel := context.WithTimeout(m.chromeCtx, m.timeout)
		defer timeoutCancel()
		var ctxOpts []chromedp.ContextOption
		if !m.singleTarget {
			ctxOpts = append(ctxOpts, chromedp.WithNewBrowserContext())
		}
		browserCtx, browserCancel = chromedp.NewContext(timeoutCtx, ctxOpts...)
	}
	defer browserCancel()

	reqContext := r.Context()
	go func() {
		<-reqContext.Done()
		browserCancel()
	}()
	server := reqContext.Value(caddyhttp.ServerCtxKey).(http.Handler)

	links := newLinks()

	var tasks chromedp.Tasks
	if m.singleTarget {
		// Lightpanda's CDP processes commands serially per session and dead-
		// locks if we issue Fetch.fulfillRequest/continueRequest for sub-
		// resources while the navigate fulfillment is still being parsed
		// (e.g. a <script type=module> with an inline `import`). Skip Fetch
		// interception entirely; instead, ask the browser to tag every
		// outgoing request with a marker header so this middleware can pass
		// the request straight through to the next handler — Lightpanda will
		// fetch the navigation and every sub-resource directly from the same
		// Caddy server.
		tasks = append(tasks, network.Enable())
		tasks = append(tasks, network.SetExtraHTTPHeaders(network.Headers{bypassHeader: "1"}))
	} else {
		tasks = append(tasks, fetch.Enable())
	}
	tasks = append(tasks, runtime.Enable())
	if m.singleTarget {
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
	} else {
		tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
			chromedp.ListenTarget(ctx, func(event any) {
				switch event := event.(type) {
				case *fetch.EventRequestPaused:
					go func() {
						var res response
						pausedURL, err := url.Parse(event.Request.URL)
						m.log.Debug("request paused",
							zap.String("request_url", event.Request.URL),
							zap.Bool("is_navigate", event.Request.URL == navigateURL),
							zap.Bool("has_post_data", event.Request.HasPostData))

						if err != nil {
							m.log.Error("failed to parse request URL", zap.String("request_url", event.Request.URL), zap.Error(err))
							browserCancel()
							return
						}

						if event.Request.URL == navigateURL {
							res = recorder

						} else if shouldHandleResourceType(event.ResourceType) && (pausedURL.Host == r.Host || slices.Contains(m.FulfillHosts, pausedURL.Host)) {
							if pausedURL.Host == r.Host {
								links.AddResource(event.Request.URL, event.ResourceType)
							} else {
								links.AddPreconnect(pausedURL.Scheme + "://" + pausedURL.Host)
							}

							var body io.Reader
							if event.Request.HasPostData {
								body = strings.NewReader(event.Request.PostData)
							}
							subRequest := httptest.NewRequest(event.Request.Method, event.Request.URL, body).WithContext(reqContext)
							for name, value := range event.Request.Headers {
								subRequest.Header.Add(name, value.(string))
							}

							subResponse := &responseWriter{header: make(http.Header)}

							server.ServeHTTP(subResponse, subRequest)

							res = subResponse

						} else if shouldHandleResourceType(event.ResourceType) && slices.Contains(m.ContinueHosts, pausedURL.Host) {
							links.AddPreconnect(pausedURL.Scheme + "://" + pausedURL.Host)

							err = fetch.ContinueRequest(event.RequestID).Do(ctx)
							if err != nil {
								m.log.Error("failed to continue request", zap.String("request_url", event.Request.URL), zap.Error(err))
								browserCancel()
							}

							m.log.Debug("request continued", zap.String("request_url", event.Request.URL))

							return

						} else {
							if pausedURL.Host == r.Host {
								links.AddResource(event.Request.URL, event.ResourceType)
							} else {
								links.AddPreconnect(pausedURL.Scheme + "://" + pausedURL.Host)
							}

							err := fetch.FailRequest(event.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
							if err != nil {
								m.log.Error("failed to block request", zap.String("request_url", event.Request.URL), zap.Error(err))
								browserCancel()
							}

							m.log.Debug("request blocked", zap.String("request_url", event.Request.URL))

							return
						}

						fulfill := fetch.FulfillRequest(event.RequestID, int64(res.Status()))
						fulfill.ResponseHeaders = make([]*fetch.HeaderEntry, 0, len(res.Header()))
						for name, values := range res.Header() {
							for _, value := range values {
								fulfill.ResponseHeaders = append(fulfill.ResponseHeaders, &fetch.HeaderEntry{name, value})
							}
						}
						fulfill.Body = base64.StdEncoding.EncodeToString(res.Buffer().Bytes())
						err = fulfill.Do(ctx)
						if err != nil {
							m.log.Error("failed to fulfill request", zap.String("request_url", event.Request.URL), zap.Error(err))
							browserCancel()
							return
						}

						m.log.Debug("request fulfilled", zap.String("request_url", event.Request.URL), zap.Int("status", res.Status()))
					}()
				case *runtime.EventExceptionThrown:
					m.log.Error("exception thrown in runtime", zap.String("exception_details", event.ExceptionDetails.Exception.Description))
				}
			})
			return nil
		}))
	}
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
	if m.singleTarget {
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
	if m.singleTarget && finalURL != "" {
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

func shouldHandleResourceType(resourceType network.ResourceType) bool {
	switch resourceType {
	case network.ResourceTypeScript:
		fallthrough
	case network.ResourceTypeXHR:
		fallthrough
	case network.ResourceTypeFetch:
		return true
	default:
		return false
	}
}

var (
	_ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
)
