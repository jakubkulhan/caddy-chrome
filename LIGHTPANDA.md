# Running caddy-chrome against Lightpanda

This file collects what we learned while making caddy-chrome work with both
headless Chrome and [Lightpanda](https://lightpanda.io/) — a CDP-compatible,
headless-only browser written in Zig with no rendering or layout pipeline.

## TL;DR

- Lightpanda is auto-detected by probing `/json/version` (`"Browser":"Lightpanda/..."`).
- When detected, the middleware switches to **single-target mode**: per-request work is
  serialized through one browser target, `Fetch` interception is replaced with a
  bypass-header strategy, and DOM serialization runs in JS to include shadow roots.
- Result on the test suite: **22/23 pass** on Lightpanda; only `links.html` is
  skipped because Lightpanda does not fetch stylesheet/image/font resources.

## Differences vs. Chrome and how they were addressed

### 1. `Browser.*` commands are dropped on a page session

Lightpanda silently ignores Browser-level CDP commands when they are sent on a
target/page session ID. `chromedp.Run(ctx, browser.GetVersion())` therefore
hangs forever because the action is dispatched against `c.Target`.

**Fix.** In `Provision`, `Browser.getVersion` is now called via
`cdp.WithExecutor(ctx, c.Browser)`. Works on both Chrome and Lightpanda.

### 2. Only one browser context / target *per CDP connection*

- `Target.createBrowserContext` returns
  `Cannot have more than one browser context at a time (-32000)` after the first
  call on the same connection.
- A second `Target.createTarget` while the first target is still open returns
  `TargetAlreadyLoaded (-31998)`.

Since Lightpanda 0.2.4 the limit is **per CDP WebSocket connection**, not per
process — every connection gets its own full browser
([release notes](https://github.com/lightpanda-io/browser/releases/tag/v0.2.4)).

**Fix.** Every render opens a fresh `chromedp.NewRemoteAllocator` +
`chromedp.NewContext` — one new WS per request — so each request gets its own
browser. Concurrent requests don't share state and no mutex is needed. The
same code path is used for Chrome: it's also faster there because each render
gets its own root browser session instead of paying for
`Target.createBrowserContext` / disposal on a shared WS reader.

For `exec` mode the middleware itself launches the browser via `os/exec`
(not chromedp's allocator), so both chrome and lightpanda are launchable.
PATH is searched lightpanda-first, falling back to chrome variants;
explicit `exec <path>` wins. For lightpanda the command is
`lightpanda serve --host 127.0.0.1 --port <picked>` plus user flags; for
chrome it's the default flag set + `--user-data-dir=<temp>` +
`--remote-debugging-port=<picked>`. The middleware polls `/json/version`
until ready, then serves every render through its own fresh WS to the
same browser. On `Cleanup` the process gets SIGINT (5 s grace, then SIGKILL)
and the temp user-data-dir is removed.

`Provision` for `url` mode uses a one-shot WS to log the browser version, then
discards it.

### 3. `Network.setCookie` rejects domains containing a port

`network.SetCookie(...).WithDomain("localhost:9080")` returns
`InvalidDomain (-31998)`.

**Fix.** Strip the port from `r.Host` (`net.SplitHostPort`) before passing it
to `WithDomain`. Also a bug-fix for Chrome.

### 4. Shadow DOM is supported in JS but not exposed by `DOM.getDocument`

Lightpanda implements shadow DOM in its JavaScript engine —
`element.attachShadow`, `el.shadowRoot.innerHTML`, declarative
`<template shadowrootmode>` (parsed) all work — but
`DOM.getDocument({pierce:true})` does **not** return shadow root nodes, and
`Element.getHTML({serializableShadowRoots:true})` is not implemented.
`outerHTML` likewise omits shadow content.

**Fix.** In single-target mode the existing Go-side `cdp.Node` serializer is
bypassed: a JS function ([js/serialize_dom.js](js/serialize_dom.js)) walks the
live `document`, emits

- `<!DOCTYPE html>`,
- void elements as `<x />`,
- `<template shadowrootmode="...">…</template>` for both real `el.shadowRoot`
  and unparsed declarative-DSD templates (via `template.content`),
- HTML-escaped attributes / text (with `<script>`/`<style>` exempt),
- comments,

then returns the result via `Runtime.evaluate`. **No DOM mutation** — the page
is read-only from the script's perspective.

### 5. `Fetch` interception deadlocks on sync module loads

This is the most subtle one and the reason the original "intercept everything"
strategy could not be reused.

**What we observed:**

- `fetch.Enable` succeeds; `Fetch.requestPaused` does fire for the document
  navigation and for `<script>` requests.
- For `fetch_get.html` — which has an inline `<script type="module">` with an
  `import "./pending_task.js"` — `fulfillRequest` for the navigation is sent
  but Lightpanda never replies. The test client times out at 5 s.

**Why.** Lightpanda's CDP session processes commands serially on a single
WebSocket reader. While the navigate fulfillment is being parsed, parsing
synchronously triggers the module fetch (`pending_task.js`) and emits another
`requestPaused`. Our handler responds with another fulfill — but the WS reader
is already blocked inside the navigate fulfillment's parse, so the second
command never gets read. Deadlock.

This is the same class of bug as
[lightpanda-io/browser#2391](https://github.com/lightpanda-io/browser/issues/2391)
("`Fetch.failRequest` during synchronous script load crashes Lightpanda with
SIGABRT"). The reporter's isolation matrix shows that **`continueRequest`
works**, but only for pages without sync module imports — for pages with them
it deadlocks the same way `fulfillRequest` does. (We confirmed: switching
sub-resource handling to `continueRequest` did not unblock `fetch_get.html`.)

It is also worth noting that Lightpanda does not honour `urlPattern`,
`resourceType`, or `requestStage` filters on `Fetch.enable` — anything other
than the default `*` silently no-ops, so we cannot ask Lightpanda to intercept
"only the navigation" via patterns.

**Fix.** We **do not call `fetch.Enable` at all**. There are two routing paths,
chosen by whether the middleware launched Lightpanda itself:

- **`exec` mode (preferred): HTTP proxy.** The middleware starts a small HTTP
  proxy ([proxy.go](proxy.go)) on a free port and launches Lightpanda with
  `--http-proxy http://127.0.0.1:<port>`. Per-request, it registers a
  `renderEntry` keyed by a random ID and tags every outgoing browser request
  with `X-Caddy-Chrome-Render: <id>` via `Network.setExtraHTTPHeaders`. The
  proxy uses that ID to look up the in-flight render and:
    - serves the navigation directly from the buffered upstream response —
      **no second upstream hit**;
    - routes same-origin sub-resources through `caddyhttp.Server.ServeHTTP`,
      with the bypass header set on the synthetic sub-request to short-circuit
      this middleware and avoid recursion;
    - relays cross-origin requests outbound via `http.DefaultTransport`;
    - tunnels HTTPS via `CONNECT` (no MITM — for HTTPS sub-resources this is
      effectively the same as the bypass-header path).

- **`url` mode (external Lightpanda): bypass header.** We can't add
  `--http-proxy` to a process we didn't launch, so we fall back to the older
  approach: `Network.setExtraHTTPHeaders({"X-Caddy-Chrome-Bypass": "1"})` tags
  every outgoing request, and `Middleware.ServeHTTP` short-circuits when it
  sees that header. Lightpanda fetches the navigation and sub-resources
  directly from the same Caddy server; this hits the upstream a second time.

`Middleware.ServeHTTP` short-circuits on either header (`bypassHeader` or
`renderHeader`) so HTTPS sub-resources tunneled via the proxy's CONNECT also
don't re-enter rendering.

### 6. Lightpanda follows redirects we used to block

In Chrome mode, the listener calls `Fetch.failRequest(BlockedByClient)` for
hosts not listed in `fulfill_hosts` / `continue_hosts`, so `chromedp.Run`
errors out and the middleware falls back to writing the buffered original
response (`recorder.WriteResponse()`). The `error.html` test relies on this:
it returns `302 Location: https://www.example.com/`, the body is the original
HTML, the test asserts the body is preserved.

In single-target mode there is no Fetch listener, so Lightpanda happily
follows the 302 to `example.com` and renders that page instead.

**Fix.** After `chromedp.Run`, evaluate `location.href`; if its host differs
from `r.Host`, write `recorder.WriteResponse()` (same fallback the
`Run`-failed path uses).

### 7. Lightpanda does not load stylesheets, images, fonts

Lightpanda's `HttpClient.RequestParams.ResourceType` enum has exactly four
variants — `document, xhr, script, fetch`. CSS / images / fonts /
media are never requested over HTTP at all (`Image.zig:114` even has a
comment: "Since we never fetch images, they are in the 'broken' state").

**Consequence.** No `requestPaused` (in Chrome mode) and no
`Network.requestWillBeSent` (in single-target mode) fires for them, so the
`links` feature cannot emit `<…>; rel=preload; as=style` /  `as=image` link
headers. The `links.html` test is skipped on Lightpanda for this reason.

There is no documented workaround. `--http-proxy` would still only see the
four resource types Lightpanda actually loads.

### 8. HTTPS to self-signed hosts

Because Lightpanda fetches resources directly (not via our Fulfill/Continue
detour through `httptest`), it sees the test server's self-signed cert.

**Fix.** Start it with `--insecure-disable-tls-host-verification`. The CI
workflow does this; the README documents it.

## Code map

- **Auto-detection** —
  [middleware.go](middleware.go) `detectLightpanda`, setting `m.lightpanda`
  when `/json/version` says `"Browser":"Lightpanda/..."`.
- **Provision** — [middleware.go](middleware.go): for `exec` mode picks a
  free port, resolves a binary via `resolveBrowser` (lightpanda first, then
  chrome), launches it with `exec.Command`, polls `/json/version` until
  ready, then runs a one-shot remote probe to log the browser version. For
  `url` mode it stores the configured URL and runs the same one-shot probe.
- **Per-request flow** — [middleware_serve_http.go](middleware_serve_http.go):
  - bypass-header short-circuit at the top of `ServeHTTP`,
  - one fresh `chromedp.NewRemoteAllocator` + `chromedp.NewContext` per
    request, regardless of backend,
  - separate listener bodies for `network.EventRequestWillBeSent`
    (single-target) vs. `fetch.EventRequestPaused` (multi-target),
  - cookie domain port strip,
  - `location.href` check after `Run` for cross-origin fallback,
  - serialization branch: JS string in single-target, Go `domSerializer` in
    multi-target.
- **JS serializer** — [js/serialize_dom.js](js/serialize_dom.js).
- **CI** — [.github/workflows/main.yaml](.github/workflows/main.yaml) splits
  into `test-chrome` and `test-lightpanda` jobs. The Lightpanda job downloads
  the official linux binary, starts `lightpanda serve
  --insecure-disable-tls-host-verification`, and runs the suite with
  `CADDY_CHROME_TEST_BROWSER_URL=http://127.0.0.1:9223/`.

## Test status

| URL                          | Chrome | Lightpanda |
| ---                          | ---    | ---        |
| `html.html`                  | pass   | pass       |
| `https://…/html.html`        | pass   | pass       |
| `html_class.html`            | pass   | pass       |
| `javascript_inline.html`     | pass   | pass       |
| `javascript_external.html`   | pass   | pass       |
| `javascript_module.html`     | pass   | pass       |
| `shadow_dom.html`            | pass   | pass       |
| `shadow_dom_nested.html`     | pass   | pass       |
| `shadow_dom_server.html`     | pass   | pass       |
| `cookie.html`                | pass   | pass       |
| `user_agent.html`            | pass   | pass       |
| `fetch_get.html`             | pass   | pass       |
| `fetch_post.html`            | pass   | pass       |
| `links.html`                 | pass   | **skip**: stylesheet/image/font fetches not implemented in Lightpanda |
| `attribute_*` (3)            | pass   | pass       |
| `text_*` (3)                 | pass   | pass       |
| `comment.html`               | pass   | pass       |
| `error.html` (302)           | pass   | pass       |
| `pending_task.html`          | pass   | pass       |

## Open upstream issues

- [lightpanda-io/browser#2391](https://github.com/lightpanda-io/browser/issues/2391)
  — Fetch domain commands during a synchronous script load (deadlock for
  `fulfillRequest`/`continueRequest`, SIGABRT for `failRequest`). Resolving
  this would let us re-enable real Fetch interception and drop the
  bypass-header workaround.
- No tracked issue for stylesheet/image/font fetches; this is by design for
  the current renderer-less architecture.
