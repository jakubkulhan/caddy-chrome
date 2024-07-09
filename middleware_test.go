package caddy_chrome

import (
	"encoding/json"
	"github.com/alecthomas/assert/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"regexp"
	"testing"
)

func TestMiddleware_UnmarshalCaddyfile(t *testing.T) {
	re := regexp.MustCompile(`\s+`)
	for _, testCase := range []struct {
		caddyfile string
		json      string
	}{
		{
			caddyfile: `chrome`,
			json:      `{}`,
		},
		{
			caddyfile: `chrome {
			}`,
			json: `{}`,
		},
		{
			caddyfile: `chrome {
				mime_types text/html
			}`,
			json: `{"mime_types":["text/html"]}`,
		},
		{
			caddyfile: `chrome {
				mime_types text/html application/xhtml+xml
			}`,
			json: `{"mime_types":["text/html","application/xhtml+xml"]}`,
		},
		{
			caddyfile: `chrome {
				exec
			}`,
			json: `{"exec_browser":{"default_flags":true}}`,
		},
		{
			caddyfile: `chrome {
				exec /usr/bin/chrome
			}`,
			json: `{"exec_browser":{"path":"/usr/bin/chrome","default_flags":true}}`,
		},
		{
			caddyfile: `chrome {
				exec /usr/bin/chrome --
			}`,
			json: `{"exec_browser":{"path":"/usr/bin/chrome","default_flags":true}}`,
		},
		{
			caddyfile: `chrome {
				exec /usr/bin/chrome --headless
			}`,
			json: `{"exec_browser":{"path":"/usr/bin/chrome","default_flags":true,"flags":["--headless"]}}`,
		},
		{
			caddyfile: `chrome {
				exec /usr/bin/chrome -- --headless
			}`,
			json: `{"exec_browser":{"path":"/usr/bin/chrome","default_flags":true,"flags":["--headless"]}}`,
		},
		{
			caddyfile: `chrome {
				exec --headless
			}`,
			json: `{"exec_browser":{"default_flags":true,"flags":["--headless"]}}`,
		},
		{
			caddyfile: `chrome {
				exec_no_default_flags /usr/bin/chrome
			}`,
			json: `{"exec_browser":{"path":"/usr/bin/chrome"}}`,
		},
		{
			caddyfile: `chrome {
				exec_no_default_flags
			}`,
			json: `{"exec_browser":{}}`,
		},
		{
			caddyfile: `chrome {
				url http://localhost:9222/
			}`,
			json: `{"remote_browser":{"url":"http://localhost:9222/"}}`,
		},
		{
			caddyfile: `chrome {
				fulfill_hosts localhost
			}`,
			json: `{"fulfill_hosts":["localhost"]}`,
		},
		{
			caddyfile: `chrome {
				fulfill_hosts my.domain api.my.domain cdn.my.domain
			}`,
			json: `{"fulfill_hosts":["my.domain","api.my.domain","cdn.my.domain"]}`,
		},
		{
			caddyfile: `chrome {
				continue_hosts external-cdn.example.com
			}`,
			json: `{"continue_hosts":["external-cdn.example.com"]}`,
		},
		{
			caddyfile: `chrome {
				continue_hosts external-cdn.example.com analytics.example.com
			}`,
			json: `{"continue_hosts":["external-cdn.example.com","analytics.example.com"]}`,
		},
	} {
		t.Run(re.ReplaceAllString(testCase.caddyfile, " "), func(t *testing.T) {
			m := new(Middleware)
			err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(testCase.caddyfile))
			if err != nil {
				t.Fatal(err)
			}
			j, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, testCase.json, string(j))
		})
	}
}
