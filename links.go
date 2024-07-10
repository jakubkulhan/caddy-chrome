package caddy_chrome

import (
	"github.com/chromedp/cdproto/network"
	"net/http"
	"sync"
)

type links struct {
	mu   sync.Mutex
	urls map[string]string
}

func newLinks() *links {
	return &links{
		urls: make(map[string]string),
	}
}

func (l *links) AddResource(url string, resourceType network.ResourceType) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch resourceType {
	case network.ResourceTypeFont:
		l.urls[url] = "font"
	case network.ResourceTypeImage:
		l.urls[url] = "image"
	case network.ResourceTypeScript:
		l.urls[url] = "script"
	case network.ResourceTypeStylesheet:
		l.urls[url] = "style"
	}
}

func (l *links) AddPreconnect(origin string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.urls[origin] = "preconnect"
}

func (l *links) MakeHeaders(header http.Header) {
	for url, relAs := range l.urls {
		if relAs == "preconnect" {
			header.Add("Link", "<"+url+">; rel=preconnect")
		} else {
			header.Add("Link", "<"+url+">; rel=preload; as="+relAs)
		}
	}
}
