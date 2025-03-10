/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package app connections to applications over a reverse tunnel and forwards
// HTTP requests to them.
package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/httplib/reverseproxy"
	"github.com/gravitational/teleport/lib/reversetunnelclient"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
)

// HandlerConfig is the configuration for an application handler.
type HandlerConfig struct {
	// Clock is used to control time in tests.
	Clock clockwork.Clock
	// AuthClient is a direct client to auth.
	AuthClient auth.ClientI
	// AccessPoint is caching client to auth.
	AccessPoint auth.ProxyAccessPoint
	// ProxyClient holds connections to leaf clusters.
	ProxyClient reversetunnelclient.Tunnel
	// ProxyPublicAddrs contains web proxy public addresses.
	ProxyPublicAddrs []utils.NetAddr
	// CipherSuites is the list of TLS cipher suites that have been configured
	// for this process.
	CipherSuites []uint16
	// WebPublicAddr
	WebPublicAddr string
}

// CheckAndSetDefaults validates configuration.
func (c *HandlerConfig) CheckAndSetDefaults() error {
	if c.Clock == nil {
		c.Clock = clockwork.NewRealClock()
	}

	if c.AuthClient == nil {
		return trace.BadParameter("auth client missing")
	}
	if c.AccessPoint == nil {
		return trace.BadParameter("access point missing")
	}
	if len(c.CipherSuites) == 0 {
		return trace.BadParameter("ciphersuites missing")
	}

	return nil
}

// Handler is an application handler.
type Handler struct {
	c *HandlerConfig

	closeContext context.Context

	router *httprouter.Router

	cache *sessionCache

	clusterName string

	log *logrus.Entry
}

// NewHandler returns a new application handler.
func NewHandler(ctx context.Context, c *HandlerConfig) (*Handler, error) {
	err := c.CheckAndSetDefaults()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	h := &Handler{
		c:            c,
		closeContext: ctx,
		log: logrus.WithFields(logrus.Fields{
			trace.Component: teleport.ComponentAppProxy,
		}),
	}

	// Create a new session cache, this holds sessions that can be used to
	// forward requests.
	h.cache, err = newSessionCache(ctx, h.log)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get the name of this cluster.
	cn, err := h.c.AccessPoint.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	h.clusterName = cn.GetClusterName()

	// Create the application routes.
	h.router = httprouter.New()
	h.router.UseRawPath = true
	h.router.POST("/x-teleport-auth", makeRouterHandler(h.withCustomCORS(h.handleAuth)))
	h.router.OPTIONS("/x-teleport-auth", makeRouterHandler(h.withCustomCORS(nil)))
	h.router.GET("/teleport-logout", h.withRouterAuth(h.handleLogout))
	h.router.NotFound = h.withAuth(h.handleHttp)

	return h, nil
}

// ServeHTTP hands the request to the request router.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// HandleConnection handles connections from plain TCP applications.
func (h *Handler) HandleConnection(ctx context.Context, clientConn net.Conn) error {
	tlsConn, ok := clientConn.(utils.TLSConn)
	if !ok {
		return trace.BadParameter("expected *tls.Conn, got: %T", clientConn)
	}

	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) != 1 {
		return trace.BadParameter("expected 1 client certificate: %+v", tlsConn.ConnectionState())
	}

	identity, err := tlsca.FromSubject(certs[0].Subject, certs[0].NotAfter)
	if err != nil {
		return trace.Wrap(err)
	}

	ws, err := h.c.AccessPoint.GetAppSession(ctx, types.GetAppSessionRequest{
		SessionID: identity.RouteToApp.SessionID,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	if ws.GetUser() != identity.Username {
		err := trace.AccessDenied("session owner %q does not match caller %q", ws.GetUser(), identity.Username)

		userMeta := identity.GetUserMetadata()
		userMeta.Login = ws.GetUser()
		h.c.AuthClient.EmitAuditEvent(h.closeContext, &apievents.AuthAttempt{
			Metadata: apievents.Metadata{
				Type: events.AuthAttemptEvent,
				Code: events.AuthAttemptFailureCode,
			},
			UserMetadata: userMeta,
			ConnectionMetadata: apievents.ConnectionMetadata{
				LocalAddr:  clientConn.LocalAddr().String(),
				RemoteAddr: clientConn.RemoteAddr().String(),
			},
			Status: apievents.Status{
				Success: false,
				Error:   err.Error(),
			},
		})
		return err
	}

	session, err := h.getSession(ctx, ws)
	if err != nil {
		return trace.Wrap(err)
	}

	serverConn, err := session.tr.DialContext(ctx, "", "")
	if err != nil {
		return trace.Wrap(err)
	}
	defer serverConn.Close()

	serverConn = tls.Client(serverConn, session.tr.clientTLSConfig)

	err = utils.ProxyConn(ctx, clientConn, serverConn)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// HealthCheckAppServer establishes a connection to a AppServer that can handle
// application requests. Can be used to ensure the proxy can handle application
// requests before they arrive.
func (h *Handler) HealthCheckAppServer(ctx context.Context, publicAddr string, clusterName string) error {
	clusterClient, err := h.c.ProxyClient.GetSite(clusterName)
	if err != nil {
		return trace.Wrap(err)
	}
	accessPoint, err := clusterClient.CachingAccessPoint()
	if err != nil {
		return trace.Wrap(err)
	}

	// At least one AppServer needs to be present to serve the requests. Using
	// MatchOne can reduce the amount of work required by the app matcher by not
	// dialing every AppServer.
	_, err = MatchOne(ctx, accessPoint, appServerMatcher(h.c.ProxyClient, publicAddr, clusterName))
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// handleHttp forwards the request to the application service or redirects
// to the application directly.
func (h *Handler) handleHttp(w http.ResponseWriter, r *http.Request, session *session) error {
	var redirectURI string
	session.tr.servers.Range(func(serverID, appServerInterface any) bool {
		appServer, ok := appServerInterface.(types.AppServer)
		if !ok {
			h.log.Warnf("Failed to load AppServer, invalid type %T", appServerInterface)
			return true
		}

		// If encounter an app server that is to be redirected to, stop iterating.
		if redirectInsteadOfForward(appServer) {
			redirectURI = appServer.GetApp().GetURI()
			return false
		}

		return true
	})

	if redirectURI != "" {
		http.Redirect(w, r, redirectURI, http.StatusFound)
		return nil
	}

	if r.Body != nil {
		// We replace the request body with a NopCloser so that the request body can
		// be closed multiple times. This is needed because Go's HTTP RoundTripper
		// will close the request body once called, even if the request
		// fails. This is a problem because fwd can retry the request if no app servers
		// are available. This retry process happens in `handleForwardError`.
		// If the request body is closed, the retry will fail.
		// Because the request body is closed after the first request, we need to
		// replace the request body with a NopCloser so that the closed body can be
		// used again and finally closed when the request handling finishes.
		// Teleport only retries requests if DialContext returns a trace.ConnectionProblem.
		// Because of this,  we can safely assume that the request body is never read
		// under those conditions so the retry attempt will start reading from the beginning
		// of the request body.
		originalBody := r.Body
		defer originalBody.Close()
		r.Body = io.NopCloser(originalBody)
	}
	session.fwd.ServeHTTP(w, r)
	return nil
}

// handleForwardError when the forwarder has an error during the `ServeHTTP` it
// will call this function. This handler will then renew the session in order
// to get "fresh" app servers, and then will forwad the request to the newly
// created session.
func (h *Handler) handleForwardError(w http.ResponseWriter, req *http.Request, err error) {
	// if it is not an agent connection problem, return without creating a new
	// session.
	if !trace.IsConnectionProblem(err) {
		reverseproxy.DefaultHandler.ServeHTTP(w, req, err)
		return
	}

	// If renewing the session fails, we should do the same for when the
	// request authentication fails (defined in the "withAuth" middle). This is
	// done to have a consistent UX to when launching an application.
	session, err := h.renewSession(req)
	if err != nil {
		if redirectErr := h.redirectToLauncher(w, req); redirectErr == nil {
			return
		}

		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(http.StatusText(http.StatusInternalServerError)))
		return
	}
	// NOTE: This handler is called by the forwarder when it encounters an error
	// during the `ServeHTTP` call. This means that the request forwarding was not successful.
	// Since the request was not forwarded, and above we ignore all errors that are not
	// connection problems, we can safely assume that the request body was not read.
	// This happens because the connection problem is returned by the DialContext
	// function, which is the HTTP transport requires before reading the request body.
	// Although the request body is not read, the request body is closed by the
	// HTTP Transport but we replace the request body in (*Handler).handleForward
	// with a NopCloser so that the request body can be closed multiple times without
	// impacting the request forwarding.
	// If in the future we decide to retry requests that fail for other reasons,
	// we need to support body rewinding with `req.GetBody` together with a
	// `io.TeeReader` to read the request body and then rewind it.
	session.fwd.ServeHTTP(w, req)
}

// authenticate will check if request carries a session cookie matching a
// session in the backend.
func (h *Handler) authenticate(ctx context.Context, r *http.Request) (*session, error) {
	ws, err := h.getAppSession(r)
	if err != nil {
		h.log.Warnf("Failed to fetch application session: %v.", err)
		return nil, trace.AccessDenied("invalid session")
	}

	// Fetch a cached session or create one if this is the first request this
	// process has seen.
	session, err := h.getSession(ctx, ws)
	if err != nil {
		h.log.Warnf("Failed to get session: %v.", err)
		return nil, trace.AccessDenied("invalid session")
	}

	return session, nil
}

// renewSession based on the request removes the session from cache (if present)
// and generates a new one using the `getSession` flow (same as in
// `authenticate`).
func (h *Handler) renewSession(r *http.Request) (*session, error) {
	ws, err := h.getAppSession(r)
	if err != nil {
		h.log.Debugf("Failed to fetch application session: not found.")
		return nil, trace.AccessDenied("invalid session")
	}

	// Remove the session from the cache, this will force a new session to be
	// generated and cached.
	h.cache.remove(ws.GetName())

	// Fetches a new session using the same flow as `authenticate`.
	session, err := h.getSession(r.Context(), ws)
	if err != nil {
		h.log.Warnf("Failed to get session: %v.", err)
		return nil, trace.AccessDenied("invalid session")
	}

	return session, nil
}

// getAppSession retrieves the `types.WebSession` using the provided
// `http.Request`.
func (h *Handler) getAppSession(r *http.Request) (ws types.WebSession, err error) {
	// We have a client certificate with encoded session id in application
	// access CLI flow i.e. when users log in using "tsh apps login" and
	// then connect to the apps with the issued certs.
	if HasClientCert(r) {
		ws, err = h.getAppSessionFromCert(r)
	} else {
		ws, err = h.getAppSessionFromCookie(r)
	}
	if err != nil {
		h.log.Warnf("Failed to get session: %v.", err)
		return nil, trace.AccessDenied("invalid session")
	}
	return ws, nil
}

func (h *Handler) getAppSessionFromCert(r *http.Request) (types.WebSession, error) {
	if !HasClientCert(r) {
		return nil, trace.BadParameter("request missing client certificate")
	}
	certificate := r.TLS.PeerCertificates[0]
	identity, err := tlsca.FromSubject(certificate.Subject, certificate.NotAfter)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Check that the session exists in the backend cache. This allows the user
	// to logout and invalidate their application session immediately. This
	// lookup should also be fast because it's in the local cache.
	ws, err := h.c.AccessPoint.GetAppSession(r.Context(), types.GetAppSessionRequest{
		SessionID: identity.RouteToApp.SessionID,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if ws.GetUser() != identity.Username {
		err := trace.AccessDenied("session owner %q does not match caller %q",
			ws.GetUser(), identity.Username)

		userMeta := identity.GetUserMetadata()
		userMeta.Login = ws.GetUser()
		h.c.AuthClient.EmitAuditEvent(h.closeContext, &apievents.AuthAttempt{
			Metadata: apievents.Metadata{
				Type: events.AuthAttemptEvent,
				Code: events.AuthAttemptFailureCode,
			},
			UserMetadata: userMeta,
			ConnectionMetadata: apievents.ConnectionMetadata{
				LocalAddr:  r.Host,
				RemoteAddr: r.RemoteAddr,
			},
			Status: apievents.Status{
				Success: false,
				Error:   err.Error(),
			},
		})
		return nil, err
	}
	return ws, nil
}

func (h *Handler) getAppSessionFromCookie(r *http.Request) (types.WebSession, error) {
	subjectValue, err := extractCookie(r, SubjectCookieName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sessionID, err := extractCookie(r, CookieName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Check that the session exists in the backend cache. This allows the user
	// to logout and invalidate their application session immediately. This
	// lookup should also be fast because it's in the local cache.
	ws, err := h.c.AccessPoint.GetAppSession(r.Context(), types.GetAppSessionRequest{
		SessionID: sessionID,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if ws.GetBearerToken() != subjectValue {
		err := trace.AccessDenied("subject session token does not match")
		h.c.AuthClient.EmitAuditEvent(h.closeContext, &apievents.AuthAttempt{
			Metadata: apievents.Metadata{
				Type: events.AuthAttemptEvent,
				Code: events.AuthAttemptFailureCode,
			},
			UserMetadata: apievents.UserMetadata{
				Login: ws.GetUser(),
				User:  "unknown", // we don't have client's username, since this came from an http request with cookies.
			},
			ConnectionMetadata: apievents.ConnectionMetadata{
				LocalAddr:  r.Host,
				RemoteAddr: r.RemoteAddr,
			},
			Status: apievents.Status{
				Success: false,
				Error:   err.Error(),
			},
		})
		return nil, err
	}
	return ws, nil
}

// getSession returns a request session used to proxy the request to the
// application service. Always checks if the session is valid first and if so,
// will return a cached session, otherwise will create one.
func (h *Handler) getSession(ctx context.Context, ws types.WebSession) (*session, error) {
	// If a cached session exists, return it right away.
	session, err := h.cache.get(ws.GetName())
	if err == nil {
		return session, nil
	}

	// Create a new session with a forwarder in it.
	session, err = h.newSession(ctx, ws)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Put the session in the cache so the next request can use it.
	err = h.cache.set(ws.GetName(), session, ws.Expiry().Sub(h.c.Clock.Now()))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return session, nil
}

// extractCookie extracts the cookie from the *http.Request.
func extractCookie(r *http.Request, cookieName string) (string, error) {
	rawCookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", trace.Wrap(err)
	}
	if rawCookie != nil && rawCookie.Value == "" {
		return "", trace.BadParameter("cookie missing")
	}

	return rawCookie.Value, nil
}

// HasFragment checks if the request is coming to the fragment authentication
// endpoint.
func HasFragment(r *http.Request) bool {
	return r.URL.Path == "/x-teleport-auth"
}

// HasSession checks if an application specific cookie exists.
func HasSession(r *http.Request) bool {
	_, err := r.Cookie(CookieName)
	return err == nil
}

// HasClientCert checks if the request has a client certificate.
func HasClientCert(r *http.Request) bool {
	return r.TLS != nil && len(r.TLS.PeerCertificates) > 0
}

// HasName checks if the client is attempting to connect to a
// host that is different than the public address of the proxy. If it is, it
// redirects back to the application launcher in the Web UI.
func HasName(r *http.Request, proxyPublicAddrs []utils.NetAddr) (string, bool) {
	raddr, err := utils.ParseAddr(r.Host)
	if err != nil {
		return "", false
	}
	for _, paddr := range proxyPublicAddrs {
		// The following requests can not be for an application:
		//
		//  * The request is for localhost or loopback.
		//  * The request is for an IP address.
		//  * The request is for the public address of the proxy.
		if utils.IsLocalhost(raddr.Host()) {
			return "", false
		}
		if net.ParseIP(raddr.Host()) != nil {
			return "", false
		}
		if raddr.Host() == paddr.Host() {
			return "", false
		}
	}
	if len(proxyPublicAddrs) == 0 {
		return "", false
	}
	// At this point, it is assumed the caller is requesting an application and
	// not the proxy, redirect the caller to the application launcher.

	urlString := makeAppRedirectURL(r, proxyPublicAddrs[0].String(), raddr.Host())
	return urlString, true
}

const (
	// CookieName is the name of the application session cookie.
	CookieName = "__Host-grv_app_session"

	// SubjectCookieName is the name of the application session subject cookie.
	SubjectCookieName = "__Host-grv_app_session_subject"
)

// makeAppRedirectURL constructs a URL that will redirect the user to the
// application launcher route in the web UI.
//
// Given app URL example: some-domain.com/arbitrary/path?foo=bar&baz=qux
// The original requested URL will be separated into three parts:
//   - hostname (or fqdn): some-domain.com
//   - path (the URL parts after the app's hostname): arbitrary/path
//   - query: foo=bar&baz=qux
//
// which will be constructed into a redirect URL using this form:
//   - /web/launch/<fqdn>?path=<encoded path>&query=<encoded query>
//
// where the final result for the example URL will be:
//   - /web/launch/some-domain.com?path=%2Farbitrary%2Fpath&query=foo%3Dbar%26baz%3Dqux
//
// The URL is formed this way to help isolate the `fqdn` param
// from the rest of the URL.
//
// The original path and query cannot be formed as `web/launch/<original URL>`
// because `web/launch` route can differ depending on how the user hits the app
// endpoint:
//  1. /web/launch/:fqdn/:clusterID/:publicAddr?/:arn?
//     This route is formed when user clicks on the web UI's app launcher
//     button from the application listing screen. The app can be directly
//     resolved since we are able to determine the app's cluster name,
//     public address, and AWS role name (if defined).
//  2. /web/launch/<fqdn>?path=<encoded path>&query=<encoded query>
//     This route is formed when a user hits the app endpoint outside of
//     the web UI (clicking from a link or copy/pasta link), and the app will
//     have to be resolved by the fqdn.
//
// Isolating the `fqdn` prevents confusing the rest of the param reserved for
// clusterId, publicAddr, and arn (where the non-query param values are used to
// create app session). The `web/launcher` will reconstruct the original
// app URL when ready to redirect the user to the requested endpoint.
func makeAppRedirectURL(r *http.Request, proxyPublicAddr, hostname string) string {
	// Note that r.URL.Path field is stored in decoded form where:
	//  - `/%47%6f%2f` becomes `/Go/`
	//  - `siema%20elo` becomes `siema elo`
	// And QueryEscape() will encode spaces as `+`
	//
	// QueryEscape is used on the `r.URL.Path` since it is being placed
	// into the query part of the URL.
	query := fmt.Sprintf("path=%s", url.QueryEscape(r.URL.Path))
	if len(r.URL.RawQuery) > 0 {
		query = fmt.Sprintf("%s&query=%s", query, url.QueryEscape(r.URL.RawQuery))
	}

	u := url.URL{
		Scheme:   "https",
		Host:     proxyPublicAddr,
		Path:     fmt.Sprintf("/web/launch/%s", hostname),
		RawQuery: query,
	}

	return u.String()
}

// redirectInsteadOfForward returns true if an application shouldn't be forwarded, but
// should be redirected directly to the public address instead.
func redirectInsteadOfForward(appServer types.AppServer) bool {
	return appServer.GetApp().Origin() == types.OriginOkta
}
