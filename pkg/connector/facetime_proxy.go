// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/matrix"
)

// FaceTime web-join proxy — eliminates the "still in the pre-call screen"
// problem that leaves peer's UI showing a propped-up-but-not-joined bridge
// participant (phantom tile).
//
// Why this exists: OpenBubbles (which runs the same rustpush protocol we
// do) gets a clean single-tile result because their Android app intercepts
// facetime.apple.com/main.js at the WebView network layer and injects two
// automations — `submitName(userDisplayName)` and a click on the
// `callcontrols-join-button-session-banner` Join button. After the webview
// fully joins, upstream rustpush auto-unprops the bridge (facetime.rs:1315
// detects the `temp:`-prefixed pseud joining and calls unprop_conv).
//
// We can't do WebView MITM from a bridge — our users open the link in a
// default browser we don't control. But we can do the equivalent at the
// HTTP origin layer: serve our own copy of facetime.apple.com/join (just
// the HTML + main.js; all other assets load from Apple's CDN directly)
// and apply the same regex patches OB applies. Browser hits our URL,
// loads our patched JS, auto-submits + auto-joins. Upstream's auto-unprop
// then fires and the phantom tile kicks itself out.
//
// The ONE undocumented risk is Apple's `wss://webcourier.push.apple.com`
// signaling WebSocket: if it validates `Origin: <bridge-host>` and rejects
// non-apple origins, the call won't establish. If it doesn't care about
// Origin, the proxy works end-to-end (media flows WebRTC direct to Apple's
// TURN servers, we're not in the bandwidth path).

const (
	// Route prefix under the bridge's appservice HTTP listener.
	// Matches the publicmedia convention (bridgev2/matrix/publicmedia.go).
	facetimeProxyPath     = "/_mautrix/facetime"
	facetimeProxyMainJS   = "/_mautrix/facetime/main.js"
	facetimeSourceOrigin  = "https://facetime.apple.com"
	facetimeSourceJoinURL = "https://facetime.apple.com/join"
	// Refresh cadence for the cached upstream HTML + JS. Apple ships new
	// builds of facetime2 periodically (each identified by a mastering
	// number like 2612Build17); a stale cache would keep serving the old
	// main.js and eventually diverge from whatever Apple's live page
	// expects. Hourly is conservative.
	facetimeCacheRefresh = 1 * time.Hour
)

type faceTimeProxy struct {
	log        zerolog.Logger
	httpClient *http.Client
	// publicBase is the bridge's externally-reachable base URL
	// (e.g. https://bridge.example.com). Empty string means no proxy —
	// buildLink returns the raw facetime.apple.com URL unchanged.
	publicBase string

	// cache holds the atomically-updated patched bodies. nil until first
	// successful fetch.
	cache atomic.Pointer[faceTimeProxyCache]

	refreshMu sync.Mutex
}

type faceTimeProxyCache struct {
	html      []byte
	mainJS    []byte
	fetchedAt time.Time
}

func newFaceTimeProxy(log zerolog.Logger) *faceTimeProxy {
	return &faceTimeProxy{
		log: log.With().Str("component", "facetime-proxy").Logger(),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// registerOnAS attaches the proxy's routes to the appservice HTTP router.
// The caller is responsible for setting publicBase separately.
// Bridgev2-hosted setups (including Beeper, self-hosted Matrix 2, etc.)
// expose this router at the bridge's public_address — same mechanism
// publicmedia uses.
func (p *faceTimeProxy) registerOnAS(as *matrix.Connector) {
	as.AS.Router.HandleFunc("GET "+facetimeProxyPath, p.serveHTML)
	as.AS.Router.HandleFunc("GET "+facetimeProxyMainJS, p.serveMainJS)
}

// buildLink wraps a raw facetime.apple.com/join URL into a bridge-hosted
// equivalent, preserving the fragment (pseud + key + optional &n=name)
// verbatim. Returns the input unchanged if publicBase is empty or the
// input doesn't look like a facetime.apple.com/join URL — caller falls
// back to the raw link in that case.
func (p *faceTimeProxy) buildLink(sourceLink string) string {
	if p == nil || p.publicBase == "" {
		return sourceLink
	}
	fragIdx := strings.Index(sourceLink, "#")
	if fragIdx == -1 {
		return sourceLink
	}
	if !strings.HasPrefix(sourceLink, facetimeSourceOrigin+"/") &&
		!strings.HasPrefix(sourceLink, "https://www.facetime.apple.com/") {
		return sourceLink
	}
	fragment := sourceLink[fragIdx:]
	return p.publicBase + facetimeProxyPath + fragment
}

func (p *faceTimeProxy) getCache() *faceTimeProxyCache {
	return p.cache.Load()
}

func (p *faceTimeProxy) ensureFresh() (*faceTimeProxyCache, error) {
	if cached := p.cache.Load(); cached != nil && time.Since(cached.fetchedAt) < facetimeCacheRefresh {
		return cached, nil
	}
	return p.refresh()
}

func (p *faceTimeProxy) refresh() (*faceTimeProxyCache, error) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	// Re-check inside the lock so concurrent callers collapse to one fetch.
	if cached := p.cache.Load(); cached != nil && time.Since(cached.fetchedAt) < facetimeCacheRefresh {
		return cached, nil
	}

	htmlBody, err := p.fetchBody(facetimeSourceJoinURL)
	if err != nil {
		return nil, fmt.Errorf("fetch join HTML: %w", err)
	}

	// Extract the main.js URL from the HTML — Apple ships it under a
	// versioned path like /applications/facetime2/2612Build17/en-us/main.js.
	mainJSURL, err := extractMainJSURL(string(htmlBody))
	if err != nil {
		return nil, fmt.Errorf("locate main.js in HTML: %w", err)
	}

	// main.js is on the static CDN, not on facetime.apple.com directly.
	// Build the absolute URL from the base (<base href="https://web-static.cdn-apple.com/">).
	mainJSAbs := "https://web-static.cdn-apple.com" + mainJSURL

	mainJSBody, err := p.fetchBody(mainJSAbs)
	if err != nil {
		return nil, fmt.Errorf("fetch main.js: %w", err)
	}

	patchedHTML := patchFaceTimeHTML(string(htmlBody), mainJSURL)
	patchedJS := patchFaceTimeMainJS(string(mainJSBody))

	cache := &faceTimeProxyCache{
		html:      []byte(patchedHTML),
		mainJS:    []byte(patchedJS),
		fetchedAt: time.Now(),
	}
	p.cache.Store(cache)
	p.log.Info().
		Str("main_js_url", mainJSAbs).
		Int("html_bytes", len(cache.html)).
		Int("js_bytes", len(cache.mainJS)).
		Msg("Refreshed FaceTime proxy cache")
	return cache, nil
}

func (p *faceTimeProxy) fetchBody(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Use a browsery user agent so Apple's CDN returns the real content,
	// not a bot/empty response. Mirrors what a real user request would
	// look like.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d %s", resp.StatusCode, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// appleUARE matches iOS/iPadOS/macOS user agents. When the requester is
// on an Apple platform, we redirect to the raw facetime.apple.com URL so
// the OS's Universal Link handling opens the native FaceTime app — a
// strictly better experience than our patched web join page. Non-Apple
// users (Android, Windows, Linux) get the patched page.
var appleUARE = regexp.MustCompile(`(?i)iPhone|iPad|iPod|Macintosh|Mac OS X`)

func (p *faceTimeProxy) serveHTML(w http.ResponseWriter, r *http.Request) {
	// iOS/macOS → native FaceTime. The browser preserves the URL fragment
	// across the 302, so pseud+key+name flow through to the Universal
	// Link handler unchanged.
	if appleUARE.MatchString(r.Header.Get("User-Agent")) {
		http.Redirect(w, r, facetimeSourceJoinURL, http.StatusFound)
		return
	}

	cache, err := p.ensureFresh()
	if err != nil {
		p.log.Warn().Err(err).Msg("Failed to ensure fresh FaceTime cache; serving stale if available")
		cache = p.getCache()
		if cache == nil {
			http.Error(w, "FaceTime proxy upstream unavailable", http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(cache.html)
}

func (p *faceTimeProxy) serveMainJS(w http.ResponseWriter, r *http.Request) {
	cache, err := p.ensureFresh()
	if err != nil {
		p.log.Warn().Err(err).Msg("Failed to ensure fresh FaceTime cache; serving stale if available")
		cache = p.getCache()
		if cache == nil {
			http.Error(w, "FaceTime proxy upstream unavailable", http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(cache.mainJS)
}

// mainJSHrefRE captures the versioned main.js path in Apple's HTML, e.g.
//   <link rel="preload" as="script" href="/applications/facetime2/2612Build17/en-us/main.js">
var mainJSHrefRE = regexp.MustCompile(`(href|src)="(/applications/facetime2/[^"]+/main\.js)"`)

// cspMetaRE matches Apple's CSP <meta> tag. We strip it — our page origin
// differs from Apple's and the hash-based inline-script whitelist would
// block Apple's bootstrap scripts otherwise. Security lost: the page no
// longer has CSP protection from our origin. Acceptable for this flow
// (single-purpose page, no user-supplied inputs in our proxied bytes).
var cspMetaRE = regexp.MustCompile(`(?i)<meta[^>]+http-equiv=["']Content-Security-Policy["'][^>]*>`)

// extractMainJSURL finds the path to Apple's versioned main.js in the
// join page HTML.
func extractMainJSURL(html string) (string, error) {
	m := mainJSHrefRE.FindStringSubmatch(html)
	if m == nil {
		return "", fmt.Errorf("no main.js href found")
	}
	return m[2], nil
}

// patchFaceTimeHTML rewrites Apple's join-page HTML so it loads our
// patched main.js from the bridge instead of Apple's CDN, and strips the
// CSP meta so our cross-origin script load is permitted.
func patchFaceTimeHTML(html string, upstreamMainJSPath string) string {
	// Strip CSP — incompatible with our origin change.
	out := cspMetaRE.ReplaceAllString(html, "")

	// Redirect <script src="..main.js"> and <link rel="preload" href="..main.js">
	// to our proxy endpoint. Absolute path starts with our proxy path; the
	// browser resolves it against the HTML's origin (our bridge).
	out = mainJSHrefRE.ReplaceAllString(out, fmt.Sprintf(`$1="%s"`, facetimeProxyMainJS))

	return out
}

// submitNameInjectRE finds the `submitName: FN` property in Apple's minified
// main.js module, matching OB's regex (CachedWebview.kt getScriptData).
// Captures: group 1 = the full `submitName: FN;` chunk including trailing
// semicolon, group 2 = the bare function identifier FN. The patch re-emits
// the original chunk and appends an invocation that kicks off the join.
var submitNameInjectRE = regexp.MustCompile(`(submitName: *([a-zA-Z]+?)[ a-zA-Z,}=:]*?;)`)

// patchFaceTimeMainJS injects auto-submit-name + auto-click-Join after the
// ParticipantLandingPage module registers its submitName callback. We
// don't know the user's display name at proxy-fetch time (the name is in
// the URL fragment the browser sees), so the injected code reads &n= from
// location.hash, base64url-decodes it, and falls back to a non-empty
// default so Apple's page accepts the submit (the page refuses empty
// names).
//
// After submitName resolves, a MutationObserver watches for the
// session-banner Join button to appear and clicks it. OB does this from
// native Kotlin via webView.loadUrl("javascript:...click()") after a
// Native.mirrored() callback; we have to inline it because we can't
// callback into the bridge from inside the browser.
func patchFaceTimeMainJS(js string) string {
	// Inject before submitName is called. The injected arrow function
	// reads &n= from the URL, runs submitName, then chains the
	// Join-button click via MutationObserver.
	const autoJoinSnippet = `;(function(){try{function getName(){try{var h=window.location.hash||"";if(h[0]==="#")h=h.substring(1);var parts=h.split("&");for(var i=0;i<parts.length;i++){var kv=parts[i].split("=");if(kv[0]==="n"&&kv[1]){var b=kv[1].replace(/-/g,"+").replace(/_/g,"/");while(b.length%4)b+="=";try{return decodeURIComponent(escape(atob(b)))}catch(e){return atob(b)}}}}catch(e){}return null}var name=getName()||"Guest";function watchJoin(){try{var btn=document.getElementById("callcontrols-join-button-session-banner");if(btn){btn.click();return}}catch(e){}var obs=new MutationObserver(function(){var b=document.getElementById("callcontrols-join-button-session-banner");if(b){obs.disconnect();b.click()}});obs.observe(document.body,{childList:true,subtree:true});setTimeout(function(){obs.disconnect()},60000)}window.__bridgeAutoJoin={name:name,watchJoin:watchJoin}}catch(e){}})();`

	// Inject the helper at the top of the bundle so the globals are set
	// before any module runs.
	out := autoJoinSnippet + js

	// Patch the submitName registration site to call itself with the
	// resolved name and chain watchJoin once the submit resolves.
	out = submitNameInjectRE.ReplaceAllString(out,
		`$1 try{$2(window.__bridgeAutoJoin.name).then(function(){try{window.__bridgeAutoJoin.watchJoin()}catch(e){}})}catch(e){}`)

	return out
}
