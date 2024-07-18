// Copyright (c) 2021-2024 MinIO, Inc.
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
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"golang.org/x/term"

	"github.com/minio/cli"
	"github.com/minio/dnscache"
	"github.com/minio/pkg/v3/certs"
	"github.com/minio/pkg/v3/console"
	"github.com/minio/pkg/v3/ellipses"
	xnet "github.com/minio/pkg/v3/net"
	"github.com/minio/sidekick/reverse"
)

// Use e.g.: go build -ldflags "-X main.version=v1.0.0"
// to set the binary version.
var version = "0.0.0-dev"

const (
	slashSeparator   = "/"
	healthPath       = "/v1/health"
	certificatesPath = "/v1/certificates"
)

var (
	globalQuietEnabled   bool
	globalDebugEnabled   bool
	globalLoggingEnabled bool
	globalTrace          string
	globalJSONEnabled    bool
	globalConsoleDisplay bool
	globalErrorsOnly     bool
	globalStatusCodes    []int
	globalConnStats      atomic.Pointer[[]*ConnStats]
	log2                 *logrus.Logger
	globalHostBalance    string
	globalTLSCert        atomic.Pointer[[]byte]
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
	healthCheckTimeout  time.Duration
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
	LastFinished atomic.Int64
	CurrentCalls atomic.Int64
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

const errMessage = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>BackendDown</Code><Message>The remote server returned an error (%v)</Message><Resource>%s</Resource></Error>`

func writeErrorResponse(w http.ResponseWriter, r *http.Request, err error) {
	// Set retry-after header to indicate user-agents to retry request after 120secs.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Retry-After
	w.Header().Set("Retry-After", "120")
	w.WriteHeader(http.StatusBadGateway)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, errMessage, err, r.URL.Path)
}

// ErrorHandler called by httputil.ReverseProxy for errors.
// Avoid canceled context error since it means the client disconnected.
func (b *Backend) ErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		panic("reverse proxy cannot call error handler without err being set")
	}

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

	writeErrorResponse(w, r, err)
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
func (b *Backend) healthCheck(ctxt context.Context) {
	ticker := time.NewTicker(b.healthCheckDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ctxt.Done():
			return
		case <-ticker.C:
			err := b.doHealthCheck()
			if err != nil {
				console.Errorln(err)
			}
		}
	}
}

func drainBody(resp *http.Response) {
	if resp != nil {
		// Drain the connection.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (b *Backend) doHealthCheck() error {
	// Set up a maximum timeout time for the healtcheck operation
	ctx, cancel := context.WithTimeout(context.Background(), b.healthCheckTimeout)
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
			traceHealthCheckReq(req, resp, reqTime, respTime, b, err)
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
	for _, c := range *globalConnStats.Load() {
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
		if t.CallStats.HealthError != nil {
			c.addHealthErrorCounts(1)
		}
	}
}

type multisite struct {
	sites          atomic.Pointer[[]*site]
	healthCanceler context.CancelFunc
}

type healthCheckOptions struct {
	healthCheckPath     string
	healthReadCheckPath string
	healthCheckPort     int
	healthCheckDuration time.Duration
	healthCheckTimeout  time.Duration
}

func (m *multisite) renewSite(ctx *cli.Context, tlsMaxVersion uint16, opts healthCheckOptions) {
	ctxt, cancel := context.WithCancel(context.Background())
	var sites []*site
	for i, siteStrs := range ctx.Args() {
		if i == len(ctx.Args())-1 {
			opts.healthCheckPath = opts.healthReadCheckPath
		}
		site := configureSite(ctxt, ctx, i+1, strings.Split(siteStrs, ","), tlsMaxVersion, opts)
		sites = append(sites, site)
	}
	m.sites.Store(&sites)
	// cancel the previous health checker
	if m.healthCanceler != nil {
		m.healthCanceler()
	}
	m.healthCanceler = cancel
}

func (m *multisite) displayUI(show bool) {
	if !show {
		return
	}
	go func() {
		// Clear screen before we start the table UI
		clearScreen()

		ticker := time.NewTicker(500 * time.Millisecond)
		for range ticker.C {
			m.populate()
		}
	}()
}

func (m *multisite) populate() {
	sites := *m.sites.Load()

	dspOrder := []col{colGreen} // Header
	for i := 0; i < len(sites); i++ {
		for range sites[i].backends {
			dspOrder = append(dspOrder, colGrey)
		}
	}
	var printColors []*color.Color
	for _, c := range dspOrder {
		printColors = append(printColors, getPrintCol(c))
	}

	tbl := console.NewTable(printColors, []bool{
		false, false, false, false, false, false,
		false, false, false, false, false,
	}, 0)

	cellText := make([][]string, len(dspOrder))
	cellText[0] = headers
	for i, site := range sites {
		for j, b := range site.backends {
			b.Stats.Lock()
			minLatency := "0s"
			maxLatency := "0s"
			if b.Stats.MaxLatency > 0 {
				minLatency = fmt.Sprintf("%2s", b.Stats.MinLatency.Round(time.Microsecond))
				maxLatency = fmt.Sprintf("%2s", b.Stats.MaxLatency.Round(time.Microsecond))
			}
			cellText[i*len(site.backends)+j+1] = []string{
				humanize.Ordinal(b.siteNumber),
				b.endpoint,
				b.getServerStatus(),
				strconv.FormatInt(b.Stats.TotCalls, 10),
				strconv.FormatInt(b.Stats.TotCallFailures, 10),
				humanize.IBytes(uint64(b.Stats.Rx)),
				humanize.IBytes(uint64(b.Stats.Tx)),
				b.Stats.CumDowntime.Round(time.Microsecond).String(),
				b.Stats.LastDowntime.Round(time.Microsecond).String(),
				minLatency,
				maxLatency,
			}
			b.Stats.Unlock()
		}
	}
	console.RewindLines(len(cellText) + 2)
	tbl.DisplayTable(cellText)
}

func (m *multisite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == certificatesPath {
		cert := globalTLSCert.Load()
		if cert != nil {
			w.Write(*cert)
		} else {
			http.Error(w, "no configured certificates found", http.StatusNotFound)
		}
		return
	}
	w.Header().Set("Server", "SideKick") // indicate sidekick is serving
	for _, s := range *m.sites.Load() {
		if s.Online() {
			switch r.URL.Path {
			case healthPath:
				// Health check endpoint should return success
				return
			default:
				s.ServeHTTP(w, r)
				return
			}
		}
	}
	writeErrorResponse(w, r, errors.New("all backend servers are offline"))
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
func (s *site) nextProxy() (*Backend, func()) {
	backends := s.upBackends()
	if len(backends) == 0 {
		return nil, func() {}
	}
	switch globalHostBalance {
	case "least":
		min := int64(math.MaxInt64)
		earliest := int64(math.MaxInt64)
		idx := 0
		// Shuffle before picking the least connection to ensure all nodes
		// are involved if the load is low to medium.
		rand.Shuffle(len(backends), func(i, j int) {
			backends[i], backends[j] = backends[j], backends[i]
		})
		for i, backend := range backends {
			currentCalls := backend.Stats.CurrentCalls.Load()
			if currentCalls < min {
				min = currentCalls
				lastFinished := backend.Stats.LastFinished.Load()
				if lastFinished < earliest {
					earliest = lastFinished
					idx = i
				}
			}
		}
		backend := backends[idx]
		backend.Stats.CurrentCalls.Add(1)
		return backend, func() {
			backend.Stats.CurrentCalls.Add(-1)
			backend.Stats.LastFinished.Store(time.Now().UnixNano())
		}
	default:
		idx := rand.Intn(len(backends))
		// random backend from a list of available backends.
		return backends[idx], func() {}
	}
}

// ServeHTTP - LoadBalancer implements http.Handler
func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend, done := s.nextProxy()
	defer done()
	if backend != nil && backend.Online() {
		httpTraceHdrs(backend.proxy.ServeHTTP, w, r, backend)
		return
	}
	writeErrorResponse(w, r, fmt.Errorf("backend %v is offline", backend.endpoint))
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
	caPEM, err := os.ReadFile(cacert)
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
	certPEM, err := os.ReadFile(cert)
	if err != nil {
		console.Fatalln(fmt.Errorf("unable to load certificate: %s", err))
	}
	keyPEM, err := os.ReadFile(key)
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
			return baseDialCtx(ctx, network, addr)
		}

		ips, err := resolver.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}

		for _, ip := range ips {
			conn, err = baseDialCtx(ctx, network, net.JoinHostPort(ip, port))
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

func clientTransport(ctx *cli.Context, tlsMaxVersion uint16, enableTLS bool, hostName string) http.RoundTripper {
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
			RootCAs:                  getCertPool(ctx.GlobalString("cacert")),
			Certificates:             getCertKeyPair(ctx.GlobalString("client-cert"), ctx.GlobalString("client-key")),
			InsecureSkipVerify:       ctx.GlobalBool("insecure"),
			MinVersion:               tls.VersionTLS12,
			MaxVersion:               tlsMaxVersion,
			PreferServerCipherSuites: true,
			ClientSessionCache:       tls.NewLRUClientSessionCache(tlsClientSessionCacheSize),
			ServerName:               hostName,
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

// sortIPs - sort ips based on higher octets.
// The logic to sort by last octet is implemented to
// prefer CIDRs with higher octets, this in-turn skips the
// localhost/loopback address to be not preferred as the
// first ip on the list. Subsequently this list helps us print
// a user friendly message with appropriate values.
func sortIPs(ipList []string) []string {
	if len(ipList) == 1 {
		return ipList
	}

	var IPs []net.IP
	var nonIPs []string
	for _, ip := range ipList {
		nip := net.ParseIP(ip)
		if nip != nil {
			IPs = append(IPs, nip)
		} else {
			nonIPs = append(nonIPs, ip)
		}
	}

	sort.Slice(IPs, func(i, j int) bool {
		// This case is needed when all ips in the list
		// have same last octets, Following just ensures that
		// 127.0.0.1 is moved to the end of the list.
		if IPs[i].IsLoopback() {
			return false
		}
		if IPs[j].IsLoopback() {
			return true
		}
		// Prefer IPv4 over IPv6
		if IPs[i].To16() == nil && IPs[j].To16() != nil {
			return true
		}
		if IPs[i].To16() != nil && IPs[j].To16() == nil {
			return false
		}
		if IPs[i].To16() != nil {
			return true // TODO: check if any IPv6 order can be preferred
		}
		// Sort IPs v4 based on higher octets.
		return []byte(IPs[i].To4())[3] > []byte(IPs[j].To4())[3]
	})

	var ips []string
	for _, ip := range IPs {
		ips = append(ips, ip.String())
	}

	return append(nonIPs, ips...)
}

func getPublicIP() string {
	var IPs []string
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if ok {
			IPs = append(IPs, ipNet.IP.String())
		}
	}
	if ips := sortIPs(IPs); len(ips) > 0 {
		return ips[0]
	}
	return "<unknown-address>"
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

func configureSite(ctxt context.Context, ctx *cli.Context, siteNum int, siteStrs []string, tlsMaxVersion uint16, opts healthCheckOptions) *site {
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
	var connStats []*ConnStats
	var hostName string
	if len(endpoints) == 1 && ctx.GlobalBool("rr-dns-mode") {
		console.Infof("RR DNS mode enabled, using %s as hostname", endpoints[0])
		// guess it is LB config address
		target, err := url.Parse(endpoints[0])
		if err != nil {
			console.Fatalln(fmt.Errorf("Unable to parse input arg %s: %s", endpoints[0], err))
		}
		hostName = target.Hostname()
		ips, err := net.LookupHost(hostName)
		if err != nil {
			console.Fatalln(fmt.Errorf("Unable to lookup host %s", hostName))
		}
		// set the new endpoints
		endpoints = []string{}
		for _, ip := range ips {
			endpoints = append(endpoints, strings.Replace(target.String(), hostName, ip, 1))
		}
	}
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
			transport = clientTransport(ctx, tlsMaxVersion, target.Scheme == "https", hostName)
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
		healthCheckURL, err := getHealthCheckURL(endpoint, opts.healthCheckPath, opts.healthCheckPort)
		if err != nil {
			console.Fatalln(err)
		}
		backend := &Backend{siteNum, endpoint, proxy, &http.Client{
			Transport: proxy.Transport,
		}, 0, healthCheckURL, opts.healthCheckDuration, opts.healthCheckTimeout, &stats}
		go backend.healthCheck(ctxt)
		proxy.ErrorHandler = backend.ErrorHandler
		backends = append(backends, backend)
		connStats = append(connStats, newConnStats(endpoint))
	}
	globalConnStats.Store(&connStats)
	return &site{
		backends: backends,
	}
}

var headers = []string{
	"SITE",
	"HOST",
	"STATUS",
	"CALLS",
	"FAILURES",
	"Rx",
	"Tx",
	"TOTAL DOWNTIME",
	"LAST DOWNTIME",
	"MIN LATENCY",
	"MAX LATENCY",
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
	healthCheckTimeout := ctx.GlobalDuration("health-timeout")
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
	globalErrorsOnly = ctx.GlobalBool("errors")
	globalStatusCodes = ctx.GlobalIntSlice("status-code")
	globalHostBalance = ctx.GlobalString("host-balance")
	if globalHostBalance == "" {
		globalHostBalance = "least"
	}

	tlsMaxVersion := uint16(tls.VersionTLS13)
	switch tlsMax := ctx.GlobalString("tls-max"); tlsMax {
	case "1.2":
		tlsMaxVersion = tls.VersionTLS12
	case "1.3":
	default:
		console.Fatalln(fmt.Errorf("invalid TLS max version specified '%s' - supported values [1.2, 1.3]", tlsMax))
	}

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

	if globalConsoleDisplay {
		console.SetColor("LogMsgType", color.New(color.FgHiMagenta))
		console.SetColor("TraceMsgType", color.New(color.FgYellow))
		console.SetColor("Stat", color.New(color.FgYellow))
		console.SetColor("Request", color.New(color.FgCyan))
		console.SetColor("Method", color.New(color.Bold, color.FgWhite))
		console.SetColor("Host", color.New(color.Bold, color.FgGreen))
		console.SetColor("ReqHeaderKey", color.New(color.Bold, color.FgWhite))
		console.SetColor("RespHeaderKey", color.New(color.Bold, color.FgCyan))
		console.SetColor("RespStatus", color.New(color.Bold, color.FgYellow))
		console.SetColor("ErrStatus", color.New(color.Bold, color.FgRed))
		console.SetColor("Response", color.New(color.FgGreen))
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

	m := &multisite{}
	m.renewSite(ctx, tlsMaxVersion, healthCheckOptions{
		healthCheckPath,
		healthReadCheckPath,
		healthCheckPort,
		healthCheckDuration,
		healthCheckTimeout,
	})
	m.displayUI(!globalConsoleDisplay)

	router.PathPrefix(slashSeparator).Handler(m)
	server := &http.Server{
		Addr:     addr,
		Handler:  router,
		ErrorLog: log.New(io.Discard, "", 0), // Turn-off random logging by Go stdlib. From MinIO server implementation.
	}
	if ctx.String("cert") != "" && ctx.String("key") != "" {
		manager, err := certs.NewManager(context.Background(), ctx.String("cert"), ctx.String("key"), tls.LoadX509KeyPair)
		if err != nil {
			console.Fatalln(err)
		}
		tlsConfig := &tls.Config{
			PreferServerCipherSuites: true,
			NextProtos:               []string{"http/1.1", "h2"},
			GetCertificate:           manager.GetCertificate,
			MinVersion:               tls.VersionTLS12,
			MaxVersion:               tlsMaxVersion,
			ClientSessionCache:       tls.NewLRUClientSessionCache(tlsClientSessionCacheSize),
		}
		server.TLSConfig = tlsConfig
	} else if ctx.String("auto-tls-host") != "" {
		cert, key, err := generateTLSCertKey(ctx.String("auto-tls-host"))
		if err != nil {
			console.Fatalln(err)
		}
		console.Printf("Generated TLS certificate for host '%s'\n", ctx.String("auto-tls-host"))
		certificates, err := tls.X509KeyPair(cert, key)
		if err != nil {
			console.Fatalln(err)
		}
		fingerprint := sha256.Sum256(certificates.Certificate[0])
		console.Printf("\nCertificate: % X", fingerprint[:len(fingerprint)/2])
		console.Printf("\n             % X", fingerprint[len(fingerprint)/2:])
		var publicKeyDER []byte
		switch privateKey := certificates.PrivateKey.(type) {
		case *ecdsa.PrivateKey:
			publicKeyDER, err = x509.MarshalPKIXPublicKey(privateKey.Public())
		default:
			console.Fatalln(fmt.Errorf("unsupported private key type %T", privateKey))
		}
		if err != nil {
			console.Fatalln(err)
		}
		publicKey := sha256.Sum256(publicKeyDER)
		console.Println("\nPublic Key:  " + base64.StdEncoding.EncodeToString(publicKey[:]))
		console.Println()
		globalTLSCert.Store(&cert)

		tlsConfig := &tls.Config{
			PreferServerCipherSuites: true,
			NextProtos:               []string{"http/1.1", "h2"},
			Certificates:             []tls.Certificate{certificates},
			MinVersion:               tls.VersionTLS12,
			MaxVersion:               tlsMaxVersion,
			ClientSessionCache:       tls.NewLRUClientSessionCache(tlsClientSessionCacheSize),
		}
		server.TLSConfig = tlsConfig
	}
	go func() {
		if err := server.ListenAndServe(); err != nil {
			console.Fatalln(err)
		}
	}()
	osSignalChannel := make(chan os.Signal, 1)
	signal.Notify(
		osSignalChannel,
		syscall.SIGTERM,
		syscall.SIGINT,
		syscall.SIGHUP,
	)
	for signal := range osSignalChannel {
		switch signal {
		case syscall.SIGHUP:
			m.renewSite(ctx, tlsMaxVersion, healthCheckOptions{
				healthCheckPath,
				healthReadCheckPath,
				healthCheckPort,
				healthCheckDuration,
				healthCheckTimeout,
			})
		default:
			console.Infof("caught signal '%s'\n", signal)
			os.Exit(1)
		}
	}
}

func main() {
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
		cli.DurationFlag{
			Name:  "health-timeout",
			Usage: "health check timeout in seconds",
			Value: 10 * time.Second,
		},
		cli.BoolFlag{
			Name:  "insecure, i",
			Usage: "disable TLS certificate verification",
		},
		cli.BoolFlag{
			Name:  "rr-dns-mode",
			Usage: "enable round-robin DNS mode",
		},
		cli.StringFlag{
			Name:  "auto-tls-host",
			Usage: "enable auto TLS mode for the specified host",
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
		cli.BoolFlag{
			Name:  "errors , e",
			Usage: "filter out any non-error responses",
		},
		cli.IntSliceFlag{
			Name:  "status-code",
			Usage: "filter by given status code",
		},
		cli.StringFlag{
			Name:  "host-balance",
			Usage: "specify the algorithm to select backend host when load balancing, supported values are 'least', 'random'",
			Value: "least",
		},
		cli.StringFlag{
			Name:   "tls-max",
			Usage:  "specify maximum supported TLS version",
			Value:  "1.3",
			Hidden: true,
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

  7. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), Set host balance as least 
     $ sidekick --host-balance=least --health-path=/minio/health/cluster http://minio{1...4}:9000
`
	app.Action = sidekickMain
	app.Run(os.Args)
}
