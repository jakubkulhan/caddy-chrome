package caddy_chrome

import (
	"fmt"
	"github.com/caddyserver/caddy/v2/caddytest"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// BenchmarkRender measures end-to-end rendering latency through the chrome
// middleware for a handful of representative pages. By default it benchmarks
// against a locally-exec'd Chrome; set CADDY_CHROME_TEST_BROWSER_URL to point
// at a remote browser (e.g. Lightpanda) to compare. The "Browser" label in the
// benchmark name reflects which backend was used.
//
// Run all:
//
//	go test -bench=BenchmarkRender -benchtime=10x -run=^$ ./...
//
// Compare Chrome vs Lightpanda:
//
//	go test -bench=BenchmarkRender -benchtime=20x -run=^$ -count=3 ./... | tee chrome.txt
//	CADDY_CHROME_TEST_BROWSER_URL=http://127.0.0.1:9223/ \
//	  go test -bench=BenchmarkRender -benchtime=20x -run=^$ -count=3 ./... | tee lightpanda.txt
//	benchstat chrome.txt lightpanda.txt
func BenchmarkRender(b *testing.B) {
	caddytest.Default.LoadRequestTimeout = 30 * time.Second
	tester := caddytest.NewTester(b)
	tester.Client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	browserConfig := "exec"
	browserLabel := "chrome_exec"
	if u := os.Getenv("CADDY_CHROME_TEST_BROWSER_URL"); u != "" {
		browserConfig = "url " + u
		if l := os.Getenv("CADDY_CHROME_TEST_BROWSER_LABEL"); l != "" {
			browserLabel = l
		} else {
			browserLabel = "remote"
		}
	}
	tester.InitServer(fmt.Sprintf(`
		{
			skip_install_trust
			admin localhost:2999
			http_port 9080
			https_port 9443
			log default {
				output discard
			}
		}
		http://localhost:9080 {
			chrome {
				%s
			}
			root ./testdata
			file_server
		}`, browserConfig), "caddyfile")

	pages := []struct {
		name string
		path string
	}{
		{"static_html", "/html.html"},
		{"javascript_module", "/javascript_module.html"},
		{"shadow_dom", "/shadow_dom.html"},
		{"fetch_get", "/fetch_get.html"},
		{"pending_task", "/pending_task.html"},
	}

	// One warm-up per page so the first measured request doesn't pay
	// connection / process startup costs.
	for _, p := range pages {
		req, _ := http.NewRequest("GET", "http://localhost:9080"+p.path, nil)
		res, err := tester.Client.Do(req)
		if err != nil {
			b.Fatalf("warmup %s: %v", p.path, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}

	for _, p := range pages {
		b.Run(browserLabel+"/"+p.name, func(b *testing.B) {
			url := "http://localhost:9080" + p.path
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req, err := http.NewRequest("GET", url, nil)
				if err != nil {
					b.Fatal(err)
				}
				res, err := tester.Client.Do(req)
				if err != nil {
					b.Fatalf("%s: %v", p.path, err)
				}
				if res.StatusCode != 200 {
					res.Body.Close()
					b.Fatalf("%s: status %d", p.path, res.StatusCode)
				}
				_, err = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRenderParallel measures throughput with concurrent requests. This
// is the metric where Lightpanda's connection-per-request mode (auto-enabled)
// should pull ahead of the serialized single-target path: every goroutine
// gets its own CDP connection and therefore its own browser.
//
//	go test -bench=BenchmarkRenderParallel -benchtime=20x -run=^$ -cpu=4 ./...
func BenchmarkRenderParallel(b *testing.B) {
	caddytest.Default.LoadRequestTimeout = 30 * time.Second
	tester := caddytest.NewTester(b)
	tester.Client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	browserConfig := "exec"
	browserLabel := "chrome_exec"
	if u := os.Getenv("CADDY_CHROME_TEST_BROWSER_URL"); u != "" {
		browserConfig = "url " + u
		if l := os.Getenv("CADDY_CHROME_TEST_BROWSER_LABEL"); l != "" {
			browserLabel = l
		} else {
			browserLabel = "remote"
		}
	}
	tester.InitServer(fmt.Sprintf(`
		{
			skip_install_trust
			admin localhost:2999
			http_port 9080
			https_port 9443
			log default {
				output discard
			}
		}
		http://localhost:9080 {
			chrome {
				%s
			}
			root ./testdata
			file_server
		}`, browserConfig), "caddyfile")

	url := "http://localhost:9080/javascript_module.html"
	// Warm up.
	req, _ := http.NewRequest("GET", url, nil)
	res, err := tester.Client.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()

	b.Run(browserLabel+"/javascript_module", func(b *testing.B) {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			client := &http.Client{Timeout: 30 * time.Second}
			for pb.Next() {
				req, _ := http.NewRequest("GET", url, nil)
				res, err := client.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}
		})
	})
}
