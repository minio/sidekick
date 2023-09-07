// Copyright (c) 2021-2023 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"golang.org/x/term"

	"github.com/minio/cli"
	"github.com/minio/dnscache"
	"github.com/minio/pkg/v2/console"
	"github.com/minio/pkg/v2/ellipses"
	xnet "github.com/minio/pkg/v2/net"
	"github.com/minio/sidekick/reverse"
)

// Use e.g.: go build -ldflags "-X main.version=v1.0.0"
// to set the binary version.
var version = "0.0.0-dev"

const (
	slashSeparator = "/"
	healthPath     = "/v1/health"
)

var (
	globalQuietEnabled   bool
	globalDebugEnabled   bool
	globalLoggingEnabled bool
	globalTrace          string
	globalJSONEnabled    bool
	globalConsoleDisplay bool
	globalConnStats      []*ConnStats
	log2                 *logrus.Logger
)

const (
	prometheusMetricsPath = "/.prometheus/metrics"
	profilingPath         = "/.pprof"
)

var dnsCache = &dnscache.Resolver{
	Timeout: 5 * time.Second,
}

func init() {
	// Create a new instance of the logger. You can have any number of instances.
	log2 = logrus.New()
}

func logMsg(msg logMessage) error {
	if globalQuietEnabled {
		return nil
	}
	msg.Type = LogMsgType
	msg.Timestamp = time.Now().UTC()
	if !globalLoggingEnabled {
		return nil
	}
	if globalJSONEnabled {
		jsonBytes, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		console.Println(string(jsonBytes))
		return nil
	}
	console.Println(msg.String())
	return nil
}

const (
	// LogMsgType for log messages
	LogMsgType = "LOG"
	// TraceMsgType for trace messages
	TraceMsgType = "TRACE"
	// DebugMsgType for debug output
	DebugMsgType = "DEBUG"
)

type logMessage struct {
	Type string `json:"Type"`
	// Endpoint of backend
	Endpoint string `json:"Endpoint"`
	// Error message
	Error error `json:"Error,omitempty"`
	// Status of endpoint
	Status string `json:"Status,omitempty"`
	// Downtime so far
	DowntimeDuration time.Duration `json:"Downtime,omitempty"`
	Timestamp        time.Time
}

func (l logMessage) String() string {
	if l.Error == nil {
		if l.DowntimeDuration > 0 {
			return fmt.Sprintf("%s%2s: %s  %s is %s Downtime duration: %s",
				console.Colorize("LogMsgType", l.Type), "",
				l.Timestamp.Format(timeFormat),
				l.Endpoint, l.Status, l.DowntimeDuration)
		}
		return fmt.Sprintf("%s%2s: %s  %s is %s", console.Colorize("LogMsgType", l.Type), "", l.Timestamp.Format(timeFormat),
			l.Endpoint, l.Status)
	}
	return fmt.Sprintf("%s%2s: %s  %s is %s: %s", console.Colorize("LogMsgType", l.Type), "",
		l.Timestamp.Format(timeFormat), l.Endpoint, l.Status, l.Error)
}

// Backend entity to which requests gets load balanced.
type Backend struct {
	siteNumber          int
	endpoint            string
	proxy               *reverse.Proxy
	httpClient          *http.Client
	up                  int32
	healthCheckURL      string
	healthCheckDuration time.Duration
	Stats               *BackendStats
}

const (
	offline = iota
	online
)

func (b *Backend) setOffline() {
	atomic.StoreInt32(&b.up, offline)
}

func (b *Backend) setOnline() {
	atomic.StoreInt32(&b.up, online)
}

// Online returns true if backend is up
func (b *Backend) Online() bool {
	return atomic.LoadInt32(&b.up) == online
}

func (b *Backend) getServerStatus() string {
	if b.Online() {
		return "UP"
	}
	return "DOWN"
}

// BackendStats holds server stats for backend
type BackendStats struct {
	sync.Mutex
	LastDowntime    time.Duration
	CumDowntime     time.Duration
	TotCalls        int64
	TotCallFailures int64
	MinLatency      time.Duration
	MaxLatency      time.Duration
	CumLatency      time.Duration
	Rx              int64
	Tx              int64
	UpSince         time.Time
	DowntimeStart   time.Time
}

// ErrorHandler called by httputil.ReverseProxy for errors.
// Avoid canceled context error since it means the client disconnected.
func (b *Backend) ErrorHandler(_ http.ResponseWriter, _ *http.Request, err error) {
	if err != nil {
		offline := true
		for _, nerr := range []error{
			context.Canceled,
			io.EOF,
			io.ErrClosedPipe,
			io.ErrUnexpectedEOF,
			errors.New("http: server closed idle connection"),
		} {
			if errors.Is(err, nerr) {
				offline = false
				break
			}
			if err.Error() == nerr.Error() {
				offline = false
				break
			}
		}
		if offline {
			if globalLoggingEnabled {
				logMsg(logMessage{Endpoint: b.endpoint, Status: "down", Error: err})
			}
			b.setOffline()
		}
	}
}

// registerMetricsRouter - add handler functions for metrics.
func registerMetricsRouter(router *mux.Router) error {
	handler, err := metricsHandler()
	if err != nil {
		return err
	}
	router.Handle(prometheusMetricsPath, handler)
	return nil
}

// A blocking call to setup a new web API for pprof - the web API
// can be consumed with go tool pprof command:
//
//	e.g. go tool pprof http://localhost:6060/minio/.profile?seconds=20 for CPU profiling
func listenAndServePProf(addr string) error {
	router := mux.NewRouter()

	for _, profilerName := range []string{"goroutine", "threadcreate", "heap", "allocs", "block", "mutex"} {
		router.Handle(profilingPath+"/"+profilerName, pprof.Handler(profilerName))
	}

	router.Handle(profilingPath+"/profile", http.HandlerFunc(pprof.Profile))
	router.Handle(profilingPath+"/symbol", http.HandlerFunc(pprof.Symbol))
	router.Handle(profilingPath+"/trace", http.HandlerFunc(pprof.Trace))

	return http.ListenAndServe(addr, router)
}

const (
	portLowerLimit = 0
	portUpperLimit = 65535
)

// getHealthCheckURL - extracts the health check URL.
func getHealthCheckURL(endpoint, healthCheckPath string, healthCheckPort int) (string, error) {
	u, err := xnet.ParseHTTPURL(strings.TrimSuffix(endpoint, slashSeparator) + healthCheckPath)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q and health check path %q: %s", endpoint, healthCheckPath, err)
	}

	if healthCheckPort == 0 {
		return u.String(), nil
	}

	// Validate port range which should be in [0, 65535]
	if healthCheckPort < portLowerLimit || healthCheckPort > portUpperLimit {
		return "", fmt.Errorf("invalid health check port \"%d\": must be in [0, 65535]", healthCheckPort)
	}

	// Set healthcheck port
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(healthCheckPort))

	return u.String(), nil
}

// healthCheck - background routine which checks if a backend is up or down.
func (b *Backend) healthCheck() {
	for {
		err := b.doHealthCheck()
		if err != nil {
			console.Fatalln(err)
		}

		time.Sleep(b.healthCheckDuration)
	}
}

func drainBody(resp *http.Response) {
	if resp != nil {
		// Drain the connection.
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (b *Backend) doHealthCheck() error {
	// Set up a maximum timeout time for the healtcheck operation
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.healthCheckURL, nil)
	if err != nil {
		return err
	}

	reqTime := time.Now().UTC()
	resp, err := b.httpClient.Do(req)
	respTime := time.Now().UTC()
	drainBody(resp)
	if err != nil || (err == nil && resp.StatusCode != http.StatusOK) {
		if globalLoggingEnabled && (!b.Online() || b.Stats.UpSince.IsZero()) {
			logMsg(logMessage{Endpoint: b.endpoint, Status: "down", Error: err})
		}
		// observed an error, take the backend down.
		b.setOffline()
		if b.Stats.DowntimeStart.IsZero() {
			b.Stats.DowntimeStart = time.Now().UTC()
		}
	} else {
		var downtimeEnd time.Time
		if !b.Stats.DowntimeStart.IsZero() {
			now := time.Now().UTC()
			b.updateDowntime(now.Sub(b.Stats.DowntimeStart))
			downtimeEnd = now
		}
		if globalLoggingEnabled && !b.Online() && !b.Stats.UpSince.IsZero() {
			logMsg(logMessage{
				Endpoint:         b.endpoint,
				Status:           "up",
				DowntimeDuration: downtimeEnd.Sub(b.Stats.DowntimeStart),
			})
		}
		b.Stats.UpSince = time.Now().UTC()
		b.Stats.DowntimeStart = time.Time{}
		b.setOnline()
	}
	if globalTrace != "application" {
		if resp != nil {
			traceHealthCheckReq(req, resp, reqTime, respTime, b)
		}
	}

	return nil
}

func (b *Backend) updateDowntime(downtime time.Duration) {
	b.Stats.Lock()
	defer b.Stats.Unlock()
	b.Stats.LastDowntime = downtime
	b.Stats.CumDowntime += downtime
}

// updateCallStats updates the cumulative stats for each call to backend
func (b *Backend) updateCallStats(t shortTraceMsg) {
	b.Stats.Lock()
	defer b.Stats.Unlock()
	b.Stats.TotCalls++
	if t.StatusCode >= http.StatusBadRequest {
		b.Stats.TotCallFailures++
	}
	b.Stats.MaxLatency = time.Duration(int64(math.Max(float64(b.Stats.MaxLatency), float64(t.CallStats.Latency))))
	b.Stats.MinLatency = time.Duration(int64(math.Min(float64(b.Stats.MinLatency), float64(t.CallStats.Latency))))
	b.Stats.Rx += int64(t.CallStats.Rx)
	b.Stats.Tx += int64(t.CallStats.Tx)
	for _, c := range globalConnStats {
		if c == nil {
			continue
		}
		if c.endpoint != b.endpoint {
			continue
		}
		c.setMinLatency(b.Stats.MinLatency)
		c.setMaxLatency(b.Stats.MaxLatency)
		c.setInputBytes(b.Stats.Rx)
		c.setOutputBytes(b.Stats.Tx)
		c.setTotalCalls(b.Stats.TotCalls)
		c.setTotalCallFailures(b.Stats.TotCallFailures)
	}
}

type multisite struct {
	sites []*site
}

func (m *multisite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "SideKick") // indicate sidekick is serving the request
	for _, s := range m.sites {
		if s.Online() {
			if r.URL.Path == healthPath {
				// Health check endpoint should return success
				return
			}
			s.ServeHTTP(w, r)
			return
		}
	}
	w.WriteHeader(http.StatusBadGateway)
}

type site struct {
	backends []*Backend
}

func (s *site) Online() bool {
	for _, backend := range s.backends {
		if backend.Online() {
			return true
		}
	}
	return false
}

func (s *site) upBackends() []*Backend {
	var backends []*Backend
	for _, backend := range s.backends {
		if backend.Online() {
			backends = append(backends, backend)
		}
	}
	return backends
}

// Returns the next backend the request should go to.
func (s *site) nextProxy() *Backend {
	backends := s.upBackends()
	if len(backends) == 0 {
		return nil
	}

	idx := rand.Intn(len(backends))
	// random backend from a list of available backends.
	return backends[idx]
}

// ServeHTTP - LoadBalancer implements http.Handler
func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend := s.nextProxy()
	if backend != nil && backend.Online() {
		httpTraceHdrs(backend.proxy.ServeHTTP, w, r, backend)
		return
	}
	w.WriteHeader(http.StatusBadGateway)
}

// mustGetSystemCertPool - return system CAs or empty pool in case of error (or windows)
func mustGetSystemCertPool() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return x509.NewCertPool()
	}
	return pool
}

// getCertPool - return system CAs or load CA from file if flag specified
func getCertPool(cacert string) *x509.CertPool {
	if cacert == "" {
		return mustGetSystemCertPool()
	}

	pool := x509.NewCertPool()
	caPEM, err := ioutil.ReadFile(cacert)
	if err != nil {
		console.Fatalln(fmt.Errorf("unable to load CA certificate: %s", err))
	}
	ok := pool.AppendCertsFromPEM(caPEM)
	if !ok {
		console.Fatalln(fmt.Errorf("unable to load CA certificate: %s is not valid certificate", cacert))
	}
	return pool
}

// getCertKeyPair - load client certificate and key pair from file if specified
func getCertKeyPair(cert, key string) []tls.Certificate {
	if cert == "" && key == "" {
		return nil
	}
	if cert == "" || key == "" {
		console.Fatalln(fmt.Errorf("both --cert and --key flags must be specified"))
	}
	certPEM, err := ioutil.ReadFile(cert)
	if err != nil {
		console.Fatalln(fmt.Errorf("unable to load certificate: %s", err))
	}
	keyPEM, err := ioutil.ReadFile(key)
	if err != nil {
		console.Fatalln(fmt.Errorf("unable to load key: %s", err))
	}
	keyPair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		console.Fatalln(fmt.Errorf("%s", err))
	}
	return []tls.Certificate{keyPair}
}

// dialContextWithDNSCache is a helper function which returns `net.DialContext` function.
// It randomly fetches an IP from the DNS cache and dials it by the given dial
// function. It dials one by one and returns first connected `net.Conn`.
// If it fails to dial all IPs from cache it returns first error. If no baseDialFunc
// is given, it sets default dial function.
//
// You can use returned dial function for `http.Transport.DialContext`.
//
// In this function, it uses functions from `rand` package. To make it really random,
// you MUST call `rand.Seed` and change the value from the default in your application
func dialContextWithDNSCache(resolver *dnscache.Resolver, baseDialCtx DialContext) DialContext {
	if baseDialCtx == nil {
		// This is same as which `http.DefaultTransport` uses.
		baseDialCtx = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	return func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		if net.ParseIP(host) != nil {
			// For IP only setups there is no need for DNS lookups.
			return baseDialCtx(ctx, "tcp", addr)
		}

		ips, err := resolver.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}

		for _, ip := range ips {
			conn, err = baseDialCtx(ctx, "tcp", net.JoinHostPort(ip, port))
			if err == nil {
				break
			}
		}

		return
	}
}

// DialContext is a function to make custom Dial for internode communications
type DialContext func(ctx context.Context, network, address string) (net.Conn, error)

// newProxyDialContext setups a custom dialer for internode communication
func newProxyDialContext(dialTimeout time.Duration) DialContext {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{
			Timeout: dialTimeout,
			Control: setTCPParameters,
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

// tlsClientSessionCacheSize is the cache size for TLS client sessions.
const tlsClientSessionCacheSize = 100

func clientTransport(ctx *cli.Context, enableTLS bool) http.RoundTripper {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContextWithDNSCache(dnsCache, newProxyDialContext(10*time.Second)),
		MaxIdleConnsPerHost:   1024,
		WriteBufferSize:       32 << 10, // 32KiB moving up from 4KiB default
		ReadBufferSize:        32 << 10, // 32KiB moving up from 4KiB default
		IdleConnTimeout:       15 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 15 * time.Second,
		// Set this value so that the underlying transport round-tripper
		// doesn't try to auto decode the body of objects with
		// content-encoding set to `gzip`.
		//
		// Refer:
		//    https://golang.org/src/net/http/transport.go?h=roundTrip#L1843
		DisableCompression: true,
	}

	if enableTLS {
		// Keep TLS config.
		tr.TLSClientConfig = &tls.Config{
			RootCAs:            getCertPool(ctx.GlobalString("cacert")),
			Certificates:       getCertKeyPair(ctx.GlobalString("client-cert"), ctx.GlobalString("client-key")),
			InsecureSkipVerify: ctx.GlobalBool("insecure"),
			// Can't use SSLv3 because of POODLE and BEAST
			// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
			// Can't use TLSv1.1 because of RC4 cipher usage
			MinVersion:               tls.VersionTLS12,
			PreferServerCipherSuites: true,
			ClientSessionCache:       tls.NewLRUClientSessionCache(tlsClientSessionCacheSize),
		}
	}

	return tr
}

func checkMain(ctx *cli.Context) {
	if !ctx.Args().Present() {
		cli.ShowCommandHelpAndExit(ctx, ctx.Command.Name, 1)
	}
}

func modifyResponse() func(*http.Response) error {
	return func(resp *http.Response) error {
		resp.Header.Set("X-Proxy", "true")
		return nil
	}
}

// sortIPs - sort ips based on higher octects.
// The logic to sort by last octet is implemented to
// prefer CIDRs with higher octects, this in-turn skips the
// localhost/loopback address to be not preferred as the
// first ip on the list. Subsequently this list helps us print
// a user friendly message with appropriate values.
func sortIPs(ipList []string) []string {
	if len(ipList) == 1 {
		return ipList
	}

	var ipV4s []net.IP
	var nonIPs []string
	for _, ip := range ipList {
		nip := net.ParseIP(ip)
		if nip != nil {
			ipV4s = append(ipV4s, nip)
		} else {
			nonIPs = append(nonIPs, ip)
		}
	}

	sort.Slice(ipV4s, func(i, j int) bool {
		// This case is needed when all ips in the list
		// have same last octets, Following just ensures that
		// 127.0.0.1 is moved to the end of the list.
		if ipV4s[i].IsLoopback() {
			return false
		}
		if ipV4s[j].IsLoopback() {
			return true
		}
		return []byte(ipV4s[i].To4())[3] > []byte(ipV4s[j].To4())[3]
	})

	var ips []string
	for _, ip := range ipV4s {
		ips = append(ips, ip.String())
	}

	return append(nonIPs, ips...)
}

func getPublicIP() string {
	var IPs []string
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			IPs = append(IPs, ipNet.IP.String())
		}
	}
	return sortIPs(IPs)[0] // There would be at least one entry
}

// IsLoopback - returns true if given IP is a loopback
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}

func configureSite(ctx *cli.Context, siteNum int, siteStrs []string, healthCheckPath string, healthCheckPort int, healthCheckDuration time.Duration) *site {
	var endpoints []string

	if ellipses.HasEllipses(siteStrs...) {
		argPatterns := make([]ellipses.ArgPattern, len(siteStrs))
		for i, arg := range siteStrs {
			patterns, err := ellipses.FindEllipsesPatterns(arg)
			if err != nil {
				console.Fatalln(fmt.Errorf("Unable to parse input arg %s: %s", arg, err))
			}
			argPatterns[i] = patterns
		}
		for _, argPattern := range argPatterns {
			for _, lbls := range argPattern.Expand() {
				endpoints = append(endpoints, strings.Join(lbls, ""))
			}
		}
	} else {
		endpoints = siteStrs
	}

	var backends []*Backend
	var prevScheme string
	var transport http.RoundTripper
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSuffix(endpoint, slashSeparator)
		target, err := url.Parse(endpoint)
		if err != nil {
			console.Fatalln(fmt.Errorf("Unable to parse input arg %s: %s", endpoint, err))
		}
		if target.Scheme == "" {
			target.Scheme = "http"
		}
		if target.Scheme != "http" && target.Scheme != "https" {
			console.Fatalln("Unexpected scheme %s, should be http or https, please use '%s --help'",
				endpoint, ctx.App.Name)
		}
		if target.Host == "" {
			console.Fatalln(fmt.Errorf("Missing host address %s, please use '%s --help'",
				endpoint, ctx.App.Name))
		}
		if prevScheme == "" {
			prevScheme = target.Scheme
		}
		if prevScheme != target.Scheme {
			console.Fatalln(fmt.Errorf("Unexpected scheme %s, please use 'http' or 'http's for all backend endpoints '%s --help'",
				endpoint, ctx.App.Name))
		}
		if transport == nil {
			transport = clientTransport(ctx, target.Scheme == "https")
		}
		// this is only used if r.RemoteAddr is localhost which means that
		// sidekick endpoint being accessed is 127.0.0.x
		realIP := getPublicIP()
		proxy := &reverse.Proxy{
			Director: func(r *http.Request) {
				r.Header.Add("X-Forwarded-Host", r.Host)
				host := realIP
				if !IsLoopback(r.RemoteAddr) {
					host, _, _ = net.SplitHostPort(r.RemoteAddr)
				}
				r.Header.Add("X-Real-IP", host)
				r.Header.Add("X-Forwarded-For", host)
				r.URL.Scheme = target.Scheme
				r.URL.Host = target.Host
			},
			Transport:      transport,
			ModifyResponse: modifyResponse(),
		}
		stats := BackendStats{MinLatency: 24 * time.Hour, MaxLatency: 0}
		healthCheckURL, err := getHealthCheckURL(endpoint, healthCheckPath, healthCheckPort)
		if err != nil {
			console.Fatalln(err)
		}
		backend := &Backend{siteNum, endpoint, proxy, &http.Client{
			Transport: proxy.Transport,
		}, 0, healthCheckURL, healthCheckDuration, &stats}
		go backend.healthCheck()
		proxy.ErrorHandler = backend.ErrorHandler
		backends = append(backends, backend)
		globalConnStats = append(globalConnStats, newConnStats(endpoint))
	}

	return &site{
		backends: backends,
	}
}

func sidekickMain(ctx *cli.Context) {
	checkMain(ctx)

	log2.SetFormatter(&logrus.TextFormatter{
		DisableColors: true,
		FullTimestamp: true,
	})
	log2.SetReportCaller(true)

	healthCheckPath := ctx.GlobalString("health-path")
	healthReadCheckPath := ctx.GlobalString("read-health-path")
	healthCheckPort := ctx.GlobalInt("health-port")
	healthCheckDuration := ctx.GlobalDuration("health-duration")
	addr := ctx.GlobalString("address")

	// Validate port range which should be in [0, 65535]
	if healthCheckPort < portLowerLimit || healthCheckPort > portUpperLimit {
		console.Fatalln(fmt.Errorf("invalid health check port \"%d\": must be in [0, 65535]", healthCheckPort))
	}

	globalLoggingEnabled = ctx.GlobalBool("log")
	globalTrace = ctx.GlobalString("trace")
	globalJSONEnabled = ctx.GlobalBool("json")
	globalQuietEnabled = ctx.GlobalBool("quiet")
	globalConsoleDisplay = globalLoggingEnabled || ctx.IsSet("trace") || !term.IsTerminal(int(os.Stdout.Fd()))
	globalDebugEnabled = ctx.GlobalBool("debug")

	go func() {
		t := time.NewTicker(ctx.GlobalDuration("dns-ttl"))
		defer t.Stop()

		for range t.C {
			dnsCache.Refresh()
		}
	}()

	if !strings.HasPrefix(healthCheckPath, slashSeparator) {
		healthCheckPath = slashSeparator + healthCheckPath
	}

	if healthReadCheckPath == "" {
		healthReadCheckPath = healthCheckPath
	}
	if !strings.HasPrefix(healthReadCheckPath, slashSeparator) {
		healthReadCheckPath = slashSeparator + healthReadCheckPath
	}

	var sites []*site
	for i, siteStrs := range ctx.Args() {
		if i == len(ctx.Args())-1 {
			healthCheckPath = healthReadCheckPath
		}

		site := configureSite(ctx, i+1, strings.Split(siteStrs, ","), healthCheckPath, healthCheckPort, healthCheckDuration)
		sites = append(sites, site)
	}

	m := &multisite{sites}
	initUI(m)

	if globalConsoleDisplay {
		console.Infof("listening on '%s'\n", addr)
	}

	if pprofAddr := ctx.String("pprof"); pprofAddr != "" {
		go func() {
			if err := listenAndServePProf(pprofAddr); err != nil {
				console.Fatalln(err)
			}
		}()
	}

	router := mux.NewRouter().SkipClean(true).UseEncodedPath()
	if err := registerMetricsRouter(router); err != nil {
		console.Fatalln(err)
	}

	router.PathPrefix(slashSeparator).Handler(m)
	if ctx.String("cert") != "" && ctx.String("key") != "" {
		if err := http.ListenAndServeTLS(addr, ctx.String("cert"), ctx.String("key"), router); err != nil {
			console.Fatalln(err)
		}
	} else {
		if err := http.ListenAndServe(addr, router); err != nil {
			console.Fatalln(err)
		}
	}
}

func main() {
	// Set-up rand seed and use global rand to avoid concurrency issues.
	rand.Seed(time.Now().UTC().UnixNano())

	// Set system resources to maximum.
	setMaxResources()

	app := cli.NewApp()
	app.Name = os.Args[0]
	app.Author = "MinIO, Inc."
	app.Description = `High-Performance sidecar load-balancer`
	app.UsageText = "[FLAGS] SITE1 [SITE2..]"
	app.Version = version
	app.Copyright = "(c) 2020-2023 MinIO, Inc."
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "address, a",
			Usage: "listening address for sidekick",
			Value: ":8080",
		},
		cli.StringFlag{
			Name:  "health-path, p",
			Usage: "health check path",
		},
		cli.StringFlag{
			Name:  "read-health-path, r",
			Usage: "health check path for read access - valid only for failover site",
		},
		cli.IntFlag{
			Name:  "health-port",
			Usage: "health check port",
		},
		cli.DurationFlag{
			Name:  "health-duration, d",
			Usage: "health check duration in seconds",
			Value: 5 * time.Second,
		},
		cli.BoolFlag{
			Name:  "insecure, i",
			Usage: "disable TLS certificate verification",
		},
		cli.BoolFlag{
			Name:  "log, l",
			Usage: "enable logging",
		},
		cli.StringFlag{
			Name:  "trace, t",
			Usage: "enable request tracing - valid values are [all,application,minio]",
			Value: "all",
		},
		cli.BoolFlag{
			Name:  "quiet, q",
			Usage: "disable console messages",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "output sidekick logs and trace in json format",
		},
		cli.BoolFlag{
			Name:  "debug",
			Usage: "output verbose trace",
		},
		cli.StringFlag{
			Name:  "cacert",
			Usage: "CA certificate to verify peer against",
		},
		cli.StringFlag{
			Name:  "client-cert",
			Usage: "client certificate file",
		},
		cli.StringFlag{
			Name:  "client-key",
			Usage: "client private key file",
		},
		cli.StringFlag{
			Name:  "cert",
			Usage: "server certificate file",
		},
		cli.StringFlag{
			Name:  "key",
			Usage: "server private key file",
		},
		cli.StringFlag{
			Name:  "pprof",
			Usage: "start and listen for profiling on the specified address (e.g. `:1337`)",
		},
		cli.DurationFlag{
			Name:  "dns-ttl",
			Usage: "choose custom DNS TTL value for DNS refreshes for load balanced endpoints",
			Value: 10 * time.Minute,
		},
	}
	app.CustomAppHelpTemplate = `NAME:
  {{.Name}} - {{.Description}}

USAGE:
  {{.Name}} - {{.UsageText}}

FLAGS:
  {{range .VisibleFlags}}{{.}}
  {{end}}
SITE:
  Each SITE is a comma separated list of pools of that site: http://172.17.0.{2...5},http://172.17.0.{6...9}.
  If all servers in SITE1 are down, then the traffic is routed to the next site - SITE2.

EXAMPLES:
  1. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000)
     $ sidekick --health-path "/minio/health/cluster" http://minio{1...4}:9000

  2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), listen on port 8000
     $ sidekick --health-path "/minio/health/cluster" --address ":8000" http://minio{1...4}:9000

  3. Load balance across 4 MinIO Servers using HTTPS and disable TLS certificate validation
     $ sidekick --health-path "/minio/health/cluster" --insecure https://minio{1...4}:9000

  4. Two sites, each site having two pools, each pool having 4 servers:
     $ sidekick --health-path=/minio/health/cluster http://site1-minio{1...4}:9000,http://site1-minio{5...8}:9000 \
               http://site2-minio{1...4}:9000,http://site2-minio{5...8}:9000

  5. Two sites, each site having two pools, each pool having 4 servers. After failover, allow read requests to site2 even if it has just read quorum:
     $ sidekick --health-path=/minio/health/cluster --read-health-path=/minio/health/cluster/read  http://site1-minio{1...4}:9000,http://site1-minio{5...8}:9000 \
               http://site2-minio{1...4}:9000,http://site2-minio{5...8}:9000

  6. Sidekick as TLS terminator:
     $ sidekick --cert public.crt --key private.key --health-path=/minio/health/cluster http://site1-minio{1...4}:9000
`
	app.Action = sidekickMain
	app.Run(os.Args)
}
