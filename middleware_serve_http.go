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

	m.log.Debug("got response", zap.String("response", buf.String()), zap.String("content_type", recorder.Header().Get("Content-Type")))

	var scheme string
	if r.TLS == nil {
		scheme = "http"
	} else {
		scheme = "https"
	}
	navigateURL := scheme + "://" + r.Host + r.RequestURI

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

					m.log.Debug("request fulfilled", zap.String("request_url", event.Request.URL))
				}()
			case *runtime.EventExceptionThrown:
				m.log.Error("exception thrown in runtime", zap.String("exception_details", event.ExceptionDetails.Exception.Description))
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
	var serializer *domSerializer
	tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
		root, err := dom.GetDocument().WithDepth(-1).WithPierce(true).Do(ctx)
		if err != nil {
			return err
		}
		serializer = &domSerializer{root: root}
		return nil
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

	if m.Links {
		links.MakeHeaders(w.Header())
	}

	w.WriteHeader(recorder.Status())

	if err := serializer.Serialize(w); err != nil {
		return errors.Wrap(err, "failed to serialize")
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
