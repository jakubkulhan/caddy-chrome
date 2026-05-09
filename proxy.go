package caddy_chrome

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/chromedp/cdproto/network"
	"go.uber.org/zap"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
)

// renderHeader carries a short opaque ID identifying which in-flight render
// a proxied request belongs to. Lightpanda is told to add it to every
// outgoing HTTP request via Network.setExtraHTTPHeaders.
const renderHeader = "X-Caddy-Chrome-Render"

// renderProxy is a minimal HTTP proxy started by the middleware. Lightpanda
// is launched with --http-proxy pointing at it, so every HTTP request the
// browser makes flows through here. The proxy looks up the in-flight render
// by the renderHeader marker, then either fulfills from the buffered upstream
// response (for the navigation), routes through the same Caddy server (for
// same-origin sub-resources), or makes an outbound HTTP request (for
// cross-origin). HTTPS is tunneled with CONNECT — we don't MITM.
type renderProxy struct {
	addr     string
	listener net.Listener
	server   *http.Server
	log      *zap.Logger
	mu       sync.RWMutex
	renders  map[string]*renderEntry
}

type renderEntry struct {
	navigateURL string
	originHost  string
	server      http.Handler
	recorder    response
	links       *links
	reqContext  context.Context
	log         *zap.Logger
}

func newRenderProxy(log *zap.Logger) (*renderProxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &renderProxy{
		addr:     "http://" + l.Addr().String(),
		listener: l,
		log:      log,
		renders:  map[string]*renderEntry{},
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve)}
	go func() {
		if err := p.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("proxy server stopped", zap.Error(err))
		}
	}()
	log.Info("render proxy listening", zap.String("addr", p.addr))
	return p, nil
}

func (p *renderProxy) close() error {
	return p.server.Close()
}

func (p *renderProxy) register(e *renderEntry) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	p.mu.Lock()
	p.renders[id] = e
	p.mu.Unlock()
	return id
}

func (p *renderProxy) unregister(id string) {
	p.mu.Lock()
	delete(p.renders, id)
	p.mu.Unlock()
}

func (p *renderProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	id := r.Header.Get(renderHeader)
	p.mu.RLock()
	entry, ok := p.renders[id]
	p.mu.RUnlock()
	if !ok {
		p.log.Warn("proxied request without a registered render", zap.String("url", r.URL.String()))
		http.Error(w, "unknown render", http.StatusBadGateway)
		return
	}
	p.handleHTTP(w, r, entry)
}

func (p *renderProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		target.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		target.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		client.Close()
		target.Close()
		return
	}
	go func() {
		defer target.Close()
		defer client.Close()
		_, _ = io.Copy(target, client)
	}()
	go func() {
		defer client.Close()
		defer target.Close()
		_, _ = io.Copy(client, target)
	}()
}

func (p *renderProxy) handleHTTP(w http.ResponseWriter, r *http.Request, entry *renderEntry) {
	reqURL := r.URL.String()
	resType := guessResourceType(r)

	if r.URL.Host == entry.originHost {
		entry.links.AddResource(reqURL, resType)
	} else {
		entry.links.AddPreconnect(r.URL.Scheme + "://" + r.URL.Host)
	}

	headers := r.Header.Clone()
	headers.Del(renderHeader)
	headers.Del("Proxy-Connection")
	headers.Del("Proxy-Authorization")

	// Navigation: serve the buffered upstream response we already captured,
	// avoiding the second upstream hit the bypass-header path incurred.
	if reqURL == entry.navigateURL && entry.recorder != nil {
		writeBuffered(w, entry.recorder)
		return
	}

	if r.URL.Host == entry.originHost {
		// Sub-resource on the same Caddy server: route through the server
		// handler so the rest of the Caddyfile (file_server, reverse_proxy,
		// etc.) serves it. The bypass header short-circuits this middleware
		// to avoid recursion.
		sub := httptest.NewRequest(r.Method, reqURL, r.Body).WithContext(entry.reqContext)
		for name, values := range headers {
			sub.Header[name] = values
		}
		sub.Header.Set(bypassHeader, "1")
		sw := &responseWriter{header: make(http.Header)}
		entry.server.ServeHTTP(sw, sub)
		writeBuffered(w, sw)
		return
	}

	// Cross-origin: relay outbound. We don't validate against fulfill_hosts /
	// continue_hosts here — those exist for chrome's Fetch interception and
	// don't apply to lightpanda.
	outReq, err := http.NewRequestWithContext(entry.reqContext, r.Method, reqURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	outReq.Header = headers
	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		entry.log.Debug("upstream fetch failed", zap.String("url", reqURL), zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeBuffered(w http.ResponseWriter, src response) {
	for name, values := range src.Header() {
		w.Header()[name] = values
	}
	status := src.Status()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(src.Buffer().Bytes())
}

// guessResourceType maps a proxied HTTP request to chromedp's ResourceType
// enum so links can build sensible preload hints. Lightpanda only ever
// fetches document, xhr, script and fetch — but we don't see that label
// directly here, so we pattern-match Sec-Fetch-Dest / Accept.
func guessResourceType(r *http.Request) network.ResourceType {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe":
		return network.ResourceTypeDocument
	case "script":
		return network.ResourceTypeScript
	case "empty":
		if r.Header.Get("Sec-Fetch-Mode") == "cors" {
			return network.ResourceTypeFetch
		}
		return network.ResourceTypeXHR
	}
	return network.ResourceTypeOther
}
