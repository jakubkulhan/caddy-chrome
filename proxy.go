package caddy_chrome

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/chromedp/cdproto/network"
	"go.uber.org/zap"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
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

	ca           *tls.Certificate
	leafKey      *rsa.PrivateKey
	leafSPKIHash string // base64(SHA-256(SPKI)) — what chrome wants for --ignore-certificate-errors-spki-list
	leafMu       sync.Mutex
	leaves       map[string]*tls.Certificate
}

type renderEntry struct {
	navigateURL   string
	originHost    string
	fulfillHosts  []string
	continueHosts []string
	server        http.Handler
	recorder      response
	links         *links
	reqContext    context.Context
	log           *zap.Logger
}

func newRenderProxy(log *zap.Logger) (*renderProxy, error) {
	ca, err := generateMITMCA()
	if err != nil {
		return nil, err
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	spkiDER, err := x509.MarshalPKIXPublicKey(&leafKey.PublicKey)
	if err != nil {
		return nil, err
	}
	leafSPKIHash := sha256.Sum256(spkiDER)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &renderProxy{
		addr:         "http://" + l.Addr().String(),
		listener:     l,
		log:          log,
		renders:      map[string]*renderEntry{},
		ca:           ca,
		leafKey:      leafKey,
		leafSPKIHash: base64.StdEncoding.EncodeToString(leafSPKIHash[:]),
		leaves:       map[string]*tls.Certificate{},
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

// handleConnect MITMs the TLS tunnel: it accepts the CONNECT, mints a leaf
// cert for the target host signed by our in-memory CA, performs the TLS
// handshake with the browser, and then hands the decrypted connection off to
// an http.Server using the same routing logic as plain HTTP. Lightpanda
// accepts our self-signed chain when launched with
// --insecure-disable-tls-host-verification.
func (p *renderProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	leaf, err := p.leafFor(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		client.Close()
		return
	}

	tlsConn := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{*leaf}})
	if err := tlsConn.Handshake(); err != nil {
		p.log.Debug("MITM handshake failed", zap.String("host", host), zap.Error(err))
		tlsConn.Close()
		return
	}

	authority := r.Host
	ln := &singleConnListener{conn: tlsConn, done: make(chan struct{})}
	inner := &http.Server{
		ErrorLog: nil,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = authority
			id := req.Header.Get(renderHeader)
			p.mu.RLock()
			entry, ok := p.renders[id]
			p.mu.RUnlock()
			if !ok {
				p.log.Warn("MITM request without a registered render",
					zap.String("url", req.URL.String()))
				http.Error(w, "unknown render", http.StatusBadGateway)
				return
			}
			p.handleHTTP(w, req, entry)
		}),
	}
	_ = inner.Serve(ln)
}

// singleConnListener feeds a single pre-existing net.Conn into http.Server.
// After handing the conn over, Accept blocks until Close — at which point
// http.Server returns from Serve.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	used bool
	mu   sync.Mutex
	done chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, io.EOF
}

func (l *singleConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.conn.Close()
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func generateMITMCA() (*tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "caddy-chrome MITM"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}

func (p *renderProxy) leafFor(host string) (*tls.Certificate, error) {
	p.leafMu.Lock()
	defer p.leafMu.Unlock()
	if c, ok := p.leaves[host]; ok {
		return c, nil
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	// All leaves share p.leafKey so chrome can trust a single SPKI hash
	// passed via --ignore-certificate-errors-spki-list.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca.Leaf, &p.leafKey.PublicKey, p.ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{
		Certificate: [][]byte{der, p.ca.Leaf.Raw},
		PrivateKey:  p.leafKey,
		Leaf:        leaf,
	}
	p.leaves[host] = cert
	return cert, nil
}

func (p *renderProxy) handleHTTP(w http.ResponseWriter, r *http.Request, entry *renderEntry) {
	reqURL := r.URL.String()
	resType := guessResourceType(r)

	if r.URL.Host == entry.originHost {
		entry.links.AddResource(reqURL, resType)
	} else {
		entry.links.AddPreconnect(r.URL.Scheme + "://" + stripDefaultPort(r.URL.Scheme, r.URL.Host))
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

	host := r.URL.Host
	switch {
	case host == entry.originHost || slices.Contains(entry.fulfillHosts, host):
		// Same Caddy server (or an explicit fulfill host): route through
		// the server handler so the rest of the Caddyfile (file_server,
		// reverse_proxy, etc.) serves it. The bypass header short-circuits
		// this middleware on the synthetic sub-request to avoid recursion.
		sub := httptest.NewRequest(r.Method, reqURL, r.Body).WithContext(entry.reqContext)
		for name, values := range headers {
			sub.Header[name] = values
		}
		sub.Header.Set(bypassHeader, "1")
		sw := &responseWriter{header: make(http.Header)}
		entry.server.ServeHTTP(sw, sub)
		writeBuffered(w, sw)

	case slices.Contains(entry.continueHosts, host):
		// Allowlisted external host: actually fetch it.
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

	default:
		entry.log.Debug("blocked cross-origin request", zap.String("url", reqURL))
		http.Error(w, "blocked", http.StatusForbidden)
	}
}

func stripDefaultPort(scheme, host string) string {
	switch scheme {
	case "http":
		return strings.TrimSuffix(host, ":80")
	case "https":
		return strings.TrimSuffix(host, ":443")
	}
	return host
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
