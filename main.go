// Copyright (c) 2020 MinIO, Inc.
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
// along with this program.  If not, see <http://www.gnu.org/licenses/>.package main

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/minio/cli"
	xhttp "github.com/minio/minio/cmd/http"
	"github.com/minio/minio/cmd/rest"
	"github.com/minio/minio/pkg/console"
	"github.com/minio/minio/pkg/ellipses"
	"github.com/minio/sidekick/pkg"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/http2"
)

const slashSeparator = "/"

var (
	globalQuietEnabled   bool
	globalDebugEnabled   bool
	globalLoggingEnabled bool
	globalTrace          string
	globalJSONEnabled    bool
	globalConsoleDisplay bool
	globalConnStats      []*ConnStats
	globalDNSCache       *xhttp.DNSCache
	log                  *logrus.Logger
)

const (
	prometheusMetricsPath = "/.prometheus/metrics"
)

func init() {
	globalDNSCache = xhttp.NewDNSCache(3*time.Second, 10*time.Second)

	// Create a new instance of the logger. You can have any number of instances.
	log = logrus.New()
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
	proxy               *httputil.ReverseProxy
	httpClient          *http.Client
	up                  int32
	healthCheckPath     string
	healthCheckPort     int
	healthCheckDuration int
	Stats               *BackendStats
	cacheClient         *S3CacheClient
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
func (b *Backend) ErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		if globalLoggingEnabled {
			logMsg(logMessage{Endpoint: b.endpoint, Error: err})
		}
		b.setOffline()
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

const (
	portLowerLimit = 0
	portUpperLimit = 65535
)

// getHealthCheckURL - extracts the health check URL.
func getHealthCheckURL(endpoint, healthCheckPath string, healthCheckPort int) (string, error) {
	healthCheckURL, err := url.ParseRequestURI(strings.TrimSuffix(endpoint, slashSeparator) + healthCheckPath)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q and health check path %q: %s", endpoint, healthCheckPath, err)
	}

	if healthCheckPort == 0 {
		return healthCheckURL.String(), nil
	}

	// Validate port range which should be in [0, 65535]
	if healthCheckPort < portLowerLimit || healthCheckPort > portUpperLimit {
		return "", fmt.Errorf("invalid health check port \"%d\": must be in [0, 65535]", healthCheckPort)
	}

	// Set health check port
	healthCheckURL.Host = net.JoinHostPort(healthCheckURL.Hostname(), strconv.Itoa(healthCheckPort))

	return healthCheckURL.String(), nil
}

// healthCheck - background routine which checks if a backend is up or down.
func (b *Backend) healthCheck() {
	healthCheckURL, err := getHealthCheckURL(b.endpoint, b.healthCheckPath, b.healthCheckPort)
	if err != nil {
		console.Fatalln(err)
	}

	for {
		reqTime := time.Now().UTC()
		req, err := http.NewRequest(http.MethodGet, healthCheckURL, nil)
		if err != nil {
			if globalLoggingEnabled {
				logMsg(logMessage{Endpoint: b.endpoint, Error: err})
			}
			b.setOffline()
			time.Sleep(time.Duration(b.healthCheckDuration) * time.Second)
			continue
		}

		resp, err := b.httpClient.Do(req)
		respTime := time.Now().UTC()
		if err == nil {
			// Drain the connection.
			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
		}
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
		if globalTrace == "all" || globalTrace == "minio" {
			if resp != nil {
				httpInternalTrace(req, resp, reqTime, respTime, b)
			}
		}
		time.Sleep(time.Duration(b.healthCheckDuration) * time.Second)
	}
}

func (b *Backend) updateDowntime(downtime time.Duration) {
	b.Stats.Lock()
	defer b.Stats.Unlock()
	b.Stats.LastDowntime = downtime
	b.Stats.CumDowntime = b.Stats.CumDowntime + downtime
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
		c.incInputBytes(b.Stats.Rx)
		c.incOutputBytes(b.Stats.Tx)
		c.incTotalCalls()
		if t.StatusCode >= http.StatusBadRequest {
			c.incTotalCallFailures()
		}
	}
}

type multisite struct {
	sites []*site
}

func (m *multisite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "SideKick/"+pkg.ReleaseTag) // indicate sidekick is serving the request
	for _, s := range m.sites {
		if s.Online() {
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
	if backend != nil {
		cacheHandlerFn := func(w http.ResponseWriter, r *http.Request) {
			if backend.cacheClient != nil {
				cacheHandler(w, r, backend)(w, r)
			} else {
				backend.proxy.ServeHTTP(w, r)
			}
		}

		httpTraceHdrs(cacheHandlerFn, w, r, backend)
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
	ok := pool.AppendCertsFromPEM([]byte(caPEM))
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
	keyPair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		console.Fatalln(fmt.Errorf("%s", err))
	}
	return []tls.Certificate{keyPair}
}

func clientTransport(ctx *cli.Context, enableTLS bool) http.RoundTripper {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           xhttp.DialContextWithDNSCache(globalDNSCache, xhttp.NewInternodeDialContext(rest.DefaultTimeout)),
		MaxIdleConnsPerHost:   1024,
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
			NextProtos:         []string{"h2", "http/1.1"},
			RootCAs:            getCertPool(ctx.GlobalString("cacert")),
			Certificates:       getCertKeyPair(ctx.GlobalString("client-cert"), ctx.GlobalString("client-key")),
			InsecureSkipVerify: ctx.GlobalBool("insecure"),
			// Can't use SSLv3 because of POODLE and BEAST
			// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
			// Can't use TLSv1.1 because of RC4 cipher usage
			MinVersion:               tls.VersionTLS12,
			PreferServerCipherSuites: true,
		}
	}

	if tr.TLSClientConfig != nil {
		http2.ConfigureTransport(tr)
	}

	return tr
}

func checkMain(ctx *cli.Context) {
	if !ctx.Args().Present() {
		console.Fatalln(fmt.Errorf("not arguments found, please check documentation '%s --help'", ctx.App.Name))
	}
}

func configureSite(ctx *cli.Context, siteNum int, siteStrs []string, healthCheckPath string, healthCheckPort int, healthCheckDuration int) *site {
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

	cacheCfg := newCacheConfig()
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

		proxy := &httputil.ReverseProxy{
			Director: func(r *http.Request) {
				r.Header.Add("X-Forwarded-Host", r.Host)
				r.Header.Add("X-Real-IP", r.RemoteAddr)

				if target.Scheme == "https" {
					r.URL.Scheme = "https"
				} else {
					r.URL.Scheme = "http"
				}
				r.URL.Host = target.Host
			},
			Transport: transport,
		}

		stats := BackendStats{MinLatency: time.Duration(24 * time.Hour), MaxLatency: time.Duration(0)}
		backend := &Backend{siteNum, endpoint, proxy, &http.Client{
			Transport: proxy.Transport,
		}, 0, healthCheckPath, healthCheckPort, healthCheckDuration, &stats, newCacheClient(ctx, cacheCfg)}
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
	defer globalDNSCache.Stop()

	checkMain(ctx)

	log.SetFormatter(&logrus.TextFormatter{
		DisableColors: true,
		FullTimestamp: true,
	})
	log.SetReportCaller(true)

	healthCheckPath := ctx.GlobalString("health-path")
	healthCheckPort := ctx.GlobalInt("health-port")
	healthCheckDuration := ctx.GlobalInt("health-duration")
	addr := ctx.GlobalString("address")
	globalLoggingEnabled = ctx.GlobalBool("log")
	globalTrace = ctx.GlobalString("trace")
	globalJSONEnabled = ctx.GlobalBool("json")
	globalQuietEnabled = ctx.GlobalBool("quiet")
	globalConsoleDisplay = globalLoggingEnabled || ctx.IsSet("trace") || !terminal.IsTerminal(int(os.Stdout.Fd()))
	globalDebugEnabled = ctx.GlobalBool("debug")

	if !strings.HasPrefix(healthCheckPath, slashSeparator) {
		healthCheckPath = slashSeparator + healthCheckPath
	}

	var sites []*site
	for i, siteStrs := range ctx.Args() {
		site := configureSite(ctx, i+1, strings.Split(siteStrs, ","), healthCheckPath, healthCheckPort, healthCheckDuration)
		sites = append(sites, site)
	}

	m := &multisite{sites}
	initUI(m)

	if globalConsoleDisplay {
		console.Infof("listening on '%s'\n", addr)
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
	app.Version = pkg.Version + " - " + pkg.ShortCommitID
	app.Copyright = "(c) 2020 MinIO, Inc."
	app.Compiled, _ = time.Parse(time.RFC3339, pkg.ReleaseTime)
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
		cli.IntFlag{
			Name:  "health-port",
			Usage: "health check port",
		},
		cli.IntFlag{
			Name:  "health-duration, d",
			Usage: "health check duration in seconds",
			Value: 5,
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
	}
	app.CustomAppHelpTemplate = `NAME:
  {{.Name}} - {{.Description}}

USAGE:
  {{.Name}} - {{.UsageText}}

FLAGS:
  {{range .VisibleFlags}}{{.}}
  {{end}}
SITE:
  Each SITE is a comma separated list of zones of that site: http://172.17.0.{2...5},http://172.17.0.{6...9}.
  If all servers in SITE1 are down, then the traffic is routed to the next site - SITE2.

EXAMPLES:
  1. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000)
     $ sidekick --health-path "/minio/health/cluster" http://minio{1...4}:9000

  2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), listen on port 8000
     $ sidekick --health-path "/minio/health/cluster" --address ":8000" http://minio{1...4}:9000

  3. Load balance across 4 MinIO Servers using HTTPS and disable TLS certificate validation
     $ sidekick --health-path "/minio/health/cluster" --insecure https://minio{1...4}:9000

  4. Two sites, each site having two zones, each zone having 4 servers:
     $ sidekick --health-path=/minio/health/cluster http://site1-minio{1...4}:9000,http://site1-minio{5...8}:9000 \
               http://site2-minio{1...4}:9000,http://site2-minio{5...8}:9000

  5. Sidekick as TLS terminator:
     $ sidekick --cert public.crt --key private.key --health-path=/minio/health/cluster http://site1-minio{1...4}:9000
`
	app.Action = sidekickMain
	app.Run(os.Args)
}
