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
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gizak/termui/v3/widgets"
	"github.com/gorilla/mux"
	"github.com/minio/cli"
	"github.com/minio/minio/pkg/console"
	"github.com/minio/minio/pkg/ellipses"
	"github.com/sirupsen/logrus"
)

const slashSeparator = "/"

var (
	globalQuietEnabled   bool
	globalDebugEnabled   bool
	globalLoggingEnabled bool
	globalTraceEnabled   bool
	globalJSONEnabled    bool
	globalConsoleDisplay bool
	globalTermTable      *widgets.Table
	globalConnStats      []*ConnStats
	timeZero             = time.Time{}

	// Create a new instance of the logger. You can have any number of instances.
	log = logrus.New()
)

const (
	prometheusMetricsPath = "/.prometheus/metrics"
)

func logMsg(msg logMessage) error {
	if globalQuietEnabled {
		return nil
	}
	msg.Type = LogMsgType
	msg.Timestamp = time.Now().UTC()
	msg.DowntimeDuration = msg.downtimeEnd.Sub(msg.downtimeStart)
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
	downtimeStart    time.Time
	downtimeEnd      time.Time
	DowntimeDuration time.Duration `json:"Downtime,omitempty"`
	Timestamp        time.Time
}

func (l logMessage) String() string {
	if l.Error == nil {
		if !l.downtimeStart.Equal(timeZero) {
			return fmt.Sprintf("%s%2s: %s  %s is %s Downtime: %s", console.Colorize("LogMsgType", l.Type), "", l.Timestamp.Format(timeFormat),
				l.Endpoint, l.Status, l.downtimeEnd.Sub(l.downtimeStart))
		}
		return fmt.Sprintf("%s%2s: %s  %s is %s", console.Colorize("LogMsgType", l.Type), "", l.Timestamp.Format(timeFormat),
			l.Endpoint, l.Status)
	}
	return fmt.Sprintf("%s%2s: %s  %s is %s: %s", console.Colorize("LogMsgType", l.Type), "", l.Timestamp.Format(timeFormat), l.Endpoint, l.Status, l.Error)
}

// Backend entity to which requests gets load balanced.
type Backend struct {
	endpoint            string
	proxy               *httputil.ReverseProxy
	httpClient          *http.Client
	up                  BackendStatus
	healthCheckPath     string
	healthCheckDuration int
	Stats               *BackendStats
	DowntimeStart       time.Time
}

// BackendStatus - if true, backend is up.
type BackendStatus bool

func (b BackendStatus) String() string {
	if BackendUP {
		return "up"
	}
	return "down"
}

const (
	// BackendUP - backend is online
	BackendUP BackendStatus = true
	// BackendDown - backend is offline
	BackendDown BackendStatus = false
)

func (b *Backend) getServerStatus() string {
	status := "UP"
	if !b.up {
		status = "DOWN"
	}
	return status
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
}

// ErrorHandler called by httputil.ReverseProxy for errors.
func (b *Backend) ErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		if globalLoggingEnabled {
			logMsg(logMessage{Endpoint: b.endpoint, Error: err})
		}
		b.up = false
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

// healthCheck - background routine which checks if a backend is up or down.
func (b *Backend) healthCheck() {
	healthCheckURL := strings.TrimSuffix(b.endpoint, slashSeparator) + b.healthCheckPath
	for {
		reqTime := time.Now().UTC()
		req, err := http.NewRequest(http.MethodGet, healthCheckURL, nil)
		if err != nil {
			if globalLoggingEnabled {
				logMsg(logMessage{Endpoint: b.endpoint, Error: err})
			}
			b.up = false
			time.Sleep(time.Duration(b.healthCheckDuration) * time.Second)
			continue
		}

		resp, err := b.httpClient.Do(req)
		respTime := time.Now().UTC()
		if err != nil {
			b.httpClient.CloseIdleConnections()
			if globalLoggingEnabled && ((b.up == BackendDown) || (b.Stats.UpSince.Equal(timeZero))) {
				logMsg(logMessage{Endpoint: b.endpoint, Status: "down", Error: err})
			}
			if b.up {
				b.up = false
			}
			if b.DowntimeStart.Equal(timeZero) {
				b.DowntimeStart = time.Now().UTC()
			}
		} else {
			// Drain the connection.
			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
			var downtimeEnd time.Time
			if resp.StatusCode == http.StatusOK {
				if !b.DowntimeStart.Equal(timeZero) {
					now := time.Now().UTC()
					downtime := now.Sub(b.DowntimeStart)
					b.updateDowntime(downtime)
					downtimeEnd = now
				}
				if globalLoggingEnabled && (b.up == BackendDown) && (!b.Stats.UpSince.Equal(timeZero)) {
					logMsg(logMessage{Endpoint: b.endpoint, Status: "up", downtimeStart: b.DowntimeStart, downtimeEnd: downtimeEnd})
				}
				b.Stats.UpSince = time.Now()
				b.DowntimeStart = timeZero
			}
			b.up = resp.StatusCode == http.StatusOK
		}
		if globalTraceEnabled {
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

type loadBalancer struct {
	backends  []*Backend
	next      int // next backend the request should go to.
	listIndex int // List happens on the same backend always as S3 list is stateful for MinIO.
	sync.RWMutex
}

// Returns the next backend the request should go to.
func (lb *loadBalancer) nextProxy() (*httputil.ReverseProxy, int) {
	lb.Lock()
	defer lb.Unlock()

	tries := 0
	idx := 0
	for {
		var proxy *httputil.ReverseProxy
		if lb.backends[lb.next].up {
			proxy = lb.backends[lb.next].proxy
			idx = lb.next
		}
		lb.next++
		if lb.next == len(lb.backends) {
			lb.next = 0
		}
		if proxy != nil {
			return proxy, idx
		}
		tries++
		if tries == len(lb.backends) {
			break
		}
	}
	return nil, -1
}

// Returns the next backend the request should go to.
func (lb *loadBalancer) listProxy() (*httputil.ReverseProxy, int) {
	lb.Lock()
	defer lb.Unlock()

	tries := 0
	listIndex := lb.listIndex
	idx := lb.listIndex
	for {
		var proxy *httputil.ReverseProxy
		if lb.backends[listIndex].up {
			proxy = lb.backends[listIndex].proxy
			idx = listIndex
		}
		listIndex++
		if listIndex == len(lb.backends) {
			listIndex = 0
		}
		if proxy != nil {
			return proxy, idx
		}
		tries++
		if tries == len(lb.backends) {
			break
		}
	}
	return nil, -1
}

func trimPrefixSuffixSlash(p string) string {
	p = strings.TrimPrefix(p, slashSeparator)
	p = strings.TrimSuffix(p, slashSeparator)
	return p
}

// ServeHTTP - LoadBalancer implements http.Handler
func (lb *loadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	p = trimPrefixSuffixSlash(p)

	var proxy *httputil.ReverseProxy
	var bkIdx int
	if strings.Contains(p, "/") {
		// Object calls get distributed across the backends.
		proxy, bkIdx = lb.nextProxy()
	} else {
		// Bucket calls always go to the same backend (to accommodate ListObjects which is stateful).
		proxy, bkIdx = lb.listProxy()
	}
	if proxy == nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	handlerFn := func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}
	httpTraceHdrs(handlerFn, w, r, lb.backends[bkIdx])
}

// mustGetSystemCertPool - return system CAs or empty pool in case of error (or windows)
func mustGetSystemCertPool() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return x509.NewCertPool()
	}
	return pool
}

var rng = rand.New(rand.NewSource(time.Now().UTC().UnixNano()))

type dialContext func(ctx context.Context, network, address string) (net.Conn, error)

func newCustomDialContext(dialTimeout, dialKeepAlive time.Duration) dialContext {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
		}

		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		addrs, err := net.LookupHost(host)
		if err != nil {
			addrs = []string{host}
		}

		for i := range addrs {
			addrs[i] = net.JoinHostPort(addrs[i], port)
		}

		return dialer.DialContext(ctx, network, addrs[rng.Intn(len(addrs))])
	}
}

func clientTransport(ctx *cli.Context, enableTLS bool) http.RoundTripper {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           newCustomDialContext(5*time.Second, 5*time.Second),
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 30 * time.Second,
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
			RootCAs: mustGetSystemCertPool(),
			// Can't use SSLv3 because of POODLE and BEAST
			// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
			// Can't use TLSv1.1 because of RC4 cipher usage
			MinVersion:         tls.VersionTLS12,
			NextProtos:         []string{"http/1.1"},
			InsecureSkipVerify: ctx.GlobalBool("insecure"),
		}

		// Because we create a custom TLSClientConfig, we have to opt-in to HTTP/2.
		// See https://github.com/golang/go/issues/14275
		//
		// TODO: Enable http2.0 when upstream issues related to HTTP/2 are fixed.
		//
		// if e = http2.ConfigureTransport(tr); e != nil {
		// 	return nil, probe.NewError(e)
		// }
	}
	return tr
}

func checkMain(ctx *cli.Context) {
	if !ctx.Args().Present() {
		console.Fatalln(fmt.Errorf("not arguments found, please check documentation '%s --help'", ctx.App.Name))
	}
}

func sidekickMain(ctx *cli.Context) {
	checkMain(ctx)

	log.SetFormatter(&logrus.TextFormatter{
		DisableColors: true,
		FullTimestamp: true,
	})
	log.SetReportCaller(true)

	healthCheckPath := ctx.GlobalString("health-path")
	healthCheckDuration := ctx.GlobalInt("health-duration")
	addr := ctx.GlobalString("address")
	globalLoggingEnabled = ctx.GlobalBool("log")
	globalTraceEnabled = ctx.GlobalBool("trace")
	globalJSONEnabled = ctx.GlobalBool("json")
	globalQuietEnabled = ctx.GlobalBool("quiet")
	globalConsoleDisplay = globalLoggingEnabled || globalTraceEnabled
	globalDebugEnabled = ctx.GlobalBool("debug")

	if !strings.HasPrefix(healthCheckPath, slashSeparator) {
		healthCheckPath = slashSeparator + healthCheckPath
	}

	var endpoints []string
	if ellipses.HasEllipses(ctx.Args()...) {
		argPatterns := make([]ellipses.ArgPattern, len(ctx.Args()))
		for i, arg := range ctx.Args() {
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
		endpoints = ctx.Args()
	}

	var backends []*Backend
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
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Transport = clientTransport(ctx, target.Scheme == "https")
		stats := BackendStats{MinLatency: time.Duration(24 * time.Hour), MaxLatency: time.Duration(0)}
		backend := &Backend{endpoint, proxy, &http.Client{
			Transport: proxy.Transport,
		}, false, healthCheckPath, healthCheckDuration, &stats, timeZero}
		go backend.healthCheck()
		proxy.ErrorHandler = backend.ErrorHandler
		backends = append(backends, backend)
		globalConnStats = append(globalConnStats, newConnStats(endpoint))
	}
	initUI(backends)
	if globalConsoleDisplay {
		console.Infoln("Listening on", addr)
	}

	nBig, err := crand.Int(crand.Reader, big.NewInt(int64(len(backends))))
	if err != nil {
		panic(err)
	}
	randInt := int(nBig.Int64())
	router := mux.NewRouter().SkipClean(true).UseEncodedPath()
	if err := registerMetricsRouter(router); err != nil {
		console.Fatalln(err)
	}
	router.PathPrefix("/").Handler(&loadBalancer{
		backends:  backends,
		next:      randInt,
		listIndex: randInt,
	})
	if err := http.ListenAndServe(addr, router); err != nil {
		console.Fatalln(err)
	}
}

func main() {
	app := cli.NewApp()
	app.Name = "sidekick"
	app.Author = "MinIO, Inc."
	app.Description = `sidekick is a high-performance sidecar load-balancer`
	app.UsageText = "sidekick [options] ENDPOINTs..."
	app.Version = Version
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

		cli.BoolFlag{
			Name:  "trace, t",
			Usage: "enable request tracing",
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
	}
	app.CustomAppHelpTemplate = `DESCRIPTION:
  {{.Description}}

USAGE:
  sidekick [FLAGS] ENDPOINTs...
  sidekick [FLAGS] ENDPOINT{1...N}

FLAGS:
  {{range .VisibleFlags}}{{.}}
  {{end}}
VERSION:
  {{.Version}}

EXAMPLES:
  1. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000)
     $ sidekick --health-path "/minio/health/ready" http://minio{1...4}:9000

  2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), listen on port 8000
     $ sidekick --health-path "/minio/health/ready" --address ":8000" http://minio{1...4}:9000

  3. Load balance across 4 MinIO Servers using HTTPS and disable TLS certificate validation
     $ sidekick --health-path "/minio/health/ready" --insecure https://minio{1...4}:9000
`
	app.Action = sidekickMain
	app.Run(os.Args)
}
