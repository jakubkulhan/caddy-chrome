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

	browserCtx, browserCancel := chromedp.NewContext(m.chromeCtx, chromedp.WithNewBrowserContext())
	defer browserCancel()

	reqContext := r.Context()
	go func() {
		<-reqContext.Done()
		browserCancel()
	}()
	server := reqContext.Value(caddyhttp.ServerCtxKey).(http.Handler)

	var tasks chromedp.Tasks
	tasks = append(tasks, fetch.Enable())
	tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
		chromedp.ListenTarget(ctx, func(event any) {
			requestPaused, ok := event.(*fetch.EventRequestPaused)
			if !ok {
				return
			}

			go func() {
				var res response
				pausedURL, err := url.Parse(requestPaused.Request.URL)
				m.log.Debug("request paused",
					zap.String("requestUrl", requestPaused.Request.URL),
					zap.String("host", pausedURL.Host),
					zap.Bool("isNavigate", requestPaused.Request.URL == navigateURL),
					zap.Bool("hasPostData", requestPaused.Request.HasPostData))

				if err != nil {
					m.log.Error("failed to parse request URL", zap.String("requestUrl", requestPaused.Request.URL), zap.Error(err))
					panic(err)
				}

				if requestPaused.Request.URL == navigateURL {
					res = recorder

				} else if shouldHandleResourceType(requestPaused.ResourceType) && (pausedURL.Host == r.Host || slices.Contains(m.FulfillHosts, pausedURL.Host)) {
					var body io.Reader
					if requestPaused.Request.HasPostData {
						body = strings.NewReader(requestPaused.Request.PostData)
					}
					subRequest, err := http.NewRequestWithContext(reqContext, requestPaused.Request.Method, requestPaused.Request.URL, body)
					if err != nil {
						m.log.Error("failed to create sub request", zap.String("requestUrl", requestPaused.Request.URL), zap.Error(err))
						panic(err)
					}
					_ = subRequest

					for name, value := range requestPaused.Request.Headers {
						subRequest.Header.Add(name, value.(string))
					}

					subResponse := &responseWriter{header: make(http.Header)}

					server.ServeHTTP(subResponse, subRequest)

					res = subResponse

				} else if shouldHandleResourceType(requestPaused.ResourceType) && slices.Contains(m.ContinueHosts, pausedURL.Host) {
					err = fetch.ContinueRequest(requestPaused.RequestID).Do(ctx)
					if err != nil {
						m.log.Error("failed to continue request", zap.String("requestUrl", requestPaused.Request.URL), zap.Error(err))
						panic(err)
					}
					return

				} else {
					err := fetch.FailRequest(requestPaused.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
					if err != nil {
						m.log.Error("failed to fail request", zap.String("requestUrl", requestPaused.Request.URL), zap.Error(err))
						panic(err)
					}
					return
				}

				fulfill := fetch.FulfillRequest(requestPaused.RequestID, int64(res.Status()))
				fulfill.ResponseHeaders = make([]*fetch.HeaderEntry, 0, len(res.Header()))
				for name, values := range res.Header() {
					for _, value := range values {
						fulfill.ResponseHeaders = append(fulfill.ResponseHeaders, &fetch.HeaderEntry{name, value})
					}
				}
				fulfill.Body = base64.StdEncoding.EncodeToString(res.Buffer().Bytes())
				err = fulfill.Do(ctx)
				if err != nil {
					m.log.Error("failed to fulfill request", zap.String("requestUrl", requestPaused.Request.URL), zap.Error(err))
					panic(err)
				}

				m.log.Debug("request fulfilled", zap.String("requestUrl", requestPaused.Request.URL))
			}()
		})
		return nil
	}))
	for _, cookie := range r.Cookies() {
		tasks = append(tasks, network.SetCookie(cookie.Name, cookie.Value).WithDomain(r.Host))
	}
	if ua := r.UserAgent(); ua != "" {
		tasks = append(tasks, emulation.SetUserAgentOverride(ua))
	}
	tasks = append(tasks, chromedp.Navigate(navigateURL))
	tasks = append(tasks, chromedp.QueryAfter("html", func(ctx context.Context, execCtx runtime.ExecutionContextID, nodes ...*cdp.Node) error {
		r, err := dom.ResolveNode().WithNodeID(nodes[0].NodeID).Do(ctx)
		if err != nil {
			return err
		}
		return chromedp.CallFunctionOn(
			getHTMLFunction,
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
