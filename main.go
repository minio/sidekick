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
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/minio/cli"
	"github.com/minio/minio/pkg/console"
	"github.com/minio/minio/pkg/ellipses"
)

// Backend entity to which requests gets load balanced.
type Backend struct {
	endpoint            string
	proxy               *httputil.ReverseProxy
	up                  bool
	healthCheckPath     string
	healthCheckDuration int
	logging             bool
}

// ErrorHandler called by httputil.ReverseProxy for errors.
func (b *Backend) ErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		if b.logging {
			fmt.Println(b.endpoint, err)
		}
		b.up = false
	}
}

// healthCheck - background routine which checks if a backend is up or down.
func (b *Backend) healthCheck() {
	healthCheckURL := b.endpoint + b.healthCheckPath
	for {
		resp, err := http.Get(healthCheckURL)
		switch {
		case err == nil && b.healthCheckPath == "":
			fallthrough
		case err == nil && resp.StatusCode == http.StatusOK:
			if b.logging {
				fmt.Printf("%s is up\n", b.endpoint)
			}
			b.up = true
		default:
			if b.logging {
				fmt.Printf("%s is down : %s\n", b.endpoint, err.Error())
			}
			b.up = false
		}
		time.Sleep(time.Duration(b.healthCheckDuration) * time.Second)
	}
}

type LoadBalancer struct {
	backends []*Backend
	next     int // next backend the request should go to.
	sync.RWMutex
}

// Returns the next backend the request should go to.
func (lb *LoadBalancer) nextProxy() *httputil.ReverseProxy {
	lb.Lock()
	defer lb.Unlock()

	tries := 0
	for {
		var proxy *httputil.ReverseProxy
		if lb.backends[lb.next].up {
			proxy = lb.backends[lb.next].proxy
		}
		lb.next++
		if lb.next == len(lb.backends) {
			lb.next = 0
		}
		if proxy != nil {
			return proxy
		}
		tries++
		if tries == len(lb.backends) {
			break
		}
	}
	return nil
}

// ServeHTTP - LoadBalancer implements http.Handler
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proxy := lb.nextProxy()
	if proxy == nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	proxy.ServeHTTP(w, r)
}

// mustGetSystemCertPool - return system CAs or empty pool in case of error (or windows)
func mustGetSystemCertPool() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return x509.NewCertPool()
	}
	return pool
}

func clientTransport(ctx *cli.Context, enableTLS bool) http.RoundTripper {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 5 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
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

func sidekickMain(ctx *cli.Context) {
	healthCheckPath := ctx.GlobalString("health-path")
	healthCheckDuration := ctx.GlobalInt("health-duration")
	addr := ctx.GlobalString("address")
	logging := ctx.GlobalBool("logging")

	if !strings.HasPrefix(healthCheckPath, "/") {
		healthCheckPath = "/" + healthCheckPath
	}

	if !ctx.Args().Present() {
		console.Fatalln(fmt.Errorf("not arguments found, please use '%s --help'", ctx.App.Name))
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
		if len(endpoints) == 1 {
			target, err := url.Parse(endpoints[0])
			if err != nil {
				console.Fatalln(fmt.Errorf("Unable to parse input arg %s: %s", endpoints[0], err))
			}
			// Single endpoint do lookup address to get all IPs
			addrs, err := net.LookupHost(target.Hostname())
			if err != nil {
				console.Fatalln(fmt.Errorf("Unable to lookup host %s: %s", endpoints[0], err))
			}
			endpoints = make([]string, len(addrs))
			for i, addr := range addrs {
				endpoints[i] = target.Scheme + "://" + net.JoinHostPort(addr, target.Port())
			}
		}
	}

	var backends []*Backend
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSuffix(endpoint, "/")
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
		backend := &Backend{endpoint, proxy, false, healthCheckPath, healthCheckDuration, logging}
		go backend.healthCheck()
		proxy.ErrorHandler = backend.ErrorHandler
		backends = append(backends, backend)
	}
	console.Infoln("Listening on", addr)
	if err := http.ListenAndServe(addr, &LoadBalancer{
		backends: backends,
	}); err != nil {
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
			Value: "/minio/health/ready",
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
			Name:  "logging, l",
			Usage: "enable logging",
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
     $ sidekick http://minio{1...4}:9000

  2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), listen on port 8000
     $ sidekick --address ":8000" http://minio{1...4}:9000

  3. Load balance across 4 MinIO Servers using HTTPS and disable TLS certificate validation
     $ sidekick --insecure https://minio{1...4}:9000
`
	app.Action = sidekickMain
	app.Run(os.Args)
}
