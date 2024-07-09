package caddy_chrome

import (
	"github.com/alecthomas/assert/v2"
	"github.com/caddyserver/caddy/v2/caddytest"
	"io"
	"net/http"
	"testing"
)

func TestMiddleware_ServeHTTP(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
		{
			debug
			skip_install_trust
			admin localhost:2999
			http_port 9080
			https_port 9443
		}
		http://localhost:9080, https://localhost:9443 {
			@fetch_post {
				method POST
				path /fetch_post.json
			}
			handle @fetch_post {
				respond {http.request.body}
			}

			chrome
			root ./testdata
			file_server
		}`, "caddyfile")

	for _, testCase := range []struct {
		url              string
		verifier         func(*testing.T, *http.Response, string)
		configureRequest func(*http.Request) error
	}{
		{
			url: "http://localhost:9080/html.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `<h1>Hello from HTML</h1>`)
			},
		},
		{
			url: "https://localhost:9443/html.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `<h1>Hello from HTML</h1>`)
			},
		},
		{
			url: "http://localhost:9080/html_class.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html class="test">`)
			},
		},
		{
			url: "http://localhost:9080/javascript_inline.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `<h1>Hello from inline Javascript</h1>`)
			},
		},
		{
			url: "http://localhost:9080/javascript_external.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `<h1>Hello from external Javascript</h1>`)
			},
		},
		{
			url: "http://localhost:9080/javascript_module.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `<h1>Hello from Javascript module</h1>`)
			},
		},
		{
			url: "http://localhost:9080/shadow_dom.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `<template shadowrootmode="open"`)
				assert.Contains(t, body, `<h1>Hello from Web Component</h1>`)
			},
		},
		{
			url: "http://localhost:9080/cookie.html",
			configureRequest: func(req *http.Request) error {
				req.AddCookie(&http.Cookie{Name: "test", Value: "cookie"})
				return nil
			},
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `document.cookie is [test=cookie]`)
			},
		},
		{
			url: "http://localhost:9080/user_agent.html",
			configureRequest: func(req *http.Request) error {
				req.Header.Set("User-Agent", "test user agent")
				return nil
			},
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `navigator.userAgent is [test user agent]`)
			},
		},
		{
			url: "http://localhost:9080/fetch_get.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `Hello from fetch GET component!`)
			},
		},
		{
			url: "http://localhost:9080/fetch_post.html",
			verifier: func(t *testing.T, res *http.Response, body string) {
				assert.Contains(t, body, `<html>`)
				assert.Contains(t, body, `Hello from fetch POST component!`)
			},
		},
	} {
		t.Run(testCase.url, func(t *testing.T) {
			req, err := http.NewRequest("GET", testCase.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.configureRequest != nil {
				if err := testCase.configureRequest(req); err != nil {
					t.Fatal(err)
				}
			}
			res := tester.AssertResponseCode(req, 200)
			defer res.Body.Close()
			bodyBytes, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			testCase.verifier(t, res, string(bodyBytes))
		})
	}
}
