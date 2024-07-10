package caddy_chrome

import (
	"bytes"
	"context"
	"encoding/base64"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/chromedp/cdproto/cdp"
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
	"net/http"
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

func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
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

	m.log.Debug("got response", zap.String("response", buf.String()), zap.String("contentType", recorder.Header().Get("Content-Type")))

	var scheme string
	if r.TLS == nil {
		scheme = "http"
	} else {
		scheme = "https"
	}
	navigateURL := scheme + "://" + r.Host + r.RequestURI

	var responseHTML string

	timeoutCtx, timeoutCancel := context.WithTimeout(m.chromeCtx, m.timeout)
	defer timeoutCancel()

	browserCtx, browserCancel := chromedp.NewContext(timeoutCtx, chromedp.WithNewBrowserContext())
	defer browserCancel()

	reqContext := r.Context()
	go func() {
		<-reqContext.Done()
		browserCancel()
	}()
	server := reqContext.Value(caddyhttp.ServerCtxKey).(http.Handler)

	links := newLinks()

	var tasks chromedp.Tasks
	tasks = append(tasks, fetch.Enable())
	tasks = append(tasks, runtime.Enable())
	tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
		chromedp.ListenTarget(ctx, func(event any) {
			switch event := event.(type) {
			case *fetch.EventRequestPaused:
				go func() {
					var res response
					pausedURL, err := url.Parse(event.Request.URL)
					m.log.Debug("request paused",
						zap.String("requestUrl", event.Request.URL),
						zap.Bool("isNavigate", event.Request.URL == navigateURL),
						zap.Bool("hasPostData", event.Request.HasPostData))

					if err != nil {
						m.log.Error("failed to parse request URL", zap.String("requestUrl", event.Request.URL), zap.Error(err))
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
						subRequest, err := http.NewRequestWithContext(reqContext, event.Request.Method, event.Request.URL, body)
						if err != nil {
							m.log.Error("failed to create sub request", zap.String("requestUrl", event.Request.URL), zap.Error(err))
							browserCancel()
							return
						}
						_ = subRequest

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
							m.log.Error("failed to continue request", zap.String("requestUrl", event.Request.URL), zap.Error(err))
							browserCancel()
						}

						m.log.Debug("request continued", zap.String("requestUrl", event.Request.URL))

						return

					} else {
						if pausedURL.Host == r.Host {
							links.AddResource(event.Request.URL, event.ResourceType)
						} else {
							links.AddPreconnect(pausedURL.Scheme + "://" + pausedURL.Host)
						}

						err := fetch.FailRequest(event.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
						if err != nil {
							m.log.Error("failed to block request", zap.String("requestUrl", event.Request.URL), zap.Error(err))
							browserCancel()
						}

						m.log.Debug("request blocked", zap.String("requestUrl", event.Request.URL))

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
						m.log.Error("failed to fulfill request", zap.String("requestUrl", event.Request.URL), zap.Error(err))
						browserCancel()
						return
					}

					m.log.Debug("request fulfilled", zap.String("requestUrl", event.Request.URL))
				}()
			case *runtime.EventExceptionThrown:
				m.log.Error("exception thrown in runtime", zap.String("exceptionDetails", event.ExceptionDetails.Exception.Description))
			}
		})
		return nil
	}))
	for _, cookie := range r.Cookies() {
		tasks = append(tasks, network.SetCookie(cookie.Name, cookie.Value).WithDomain(r.Host))
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
	tasks = append(tasks, chromedp.QueryAfter("html", func(ctx context.Context, execCtx runtime.ExecutionContextID, nodes ...*cdp.Node) error {
		r, err := dom.ResolveNode().WithNodeID(nodes[0].NodeID).Do(ctx)
		if err != nil {
			return err
		}
		return chromedp.CallFunctionOn(
			getHTMLScript,
			&responseHTML,
			func(p *runtime.CallFunctionOnParams) *runtime.CallFunctionOnParams {
				return p.WithObjectID(r.ObjectID)
			},
		).Do(ctx)
	}))
	err = chromedp.Run(browserCtx, tasks)
	if err != nil {
		return errors.Wrap(err, "failed to run chrome")
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

	links.MakeHeaders(w.Header())

	w.WriteHeader(recorder.Status())

	if _, err := w.Write([]byte("<!doctype html>\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte(responseHTML)); err != nil {
		return err
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
