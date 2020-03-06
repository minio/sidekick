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
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/minio/cli"
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

func sidekickMain(ctx *cli.Context) error {
	healthCheckPath := ctx.GlobalString("health-path")
	healthCheckDuration := ctx.GlobalInt("health-duration")
	insecure := ctx.GlobalBool("insecure")
	addr := ctx.GlobalString("address")
	logging := ctx.GlobalBool("logging")

	if !strings.HasPrefix(healthCheckPath, "/") {
		healthCheckPath = "/" + healthCheckPath
	}

	endpoints := ctx.Args()
	if len(endpoints) == 0 {
		cli.ShowAppHelpAndExit(ctx, 1)
		return errors.New("endpoints not provided")
	}
	var backends []*Backend
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSuffix(endpoint, "/")
		target, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		if target.Scheme == "" {
			target.Scheme = "http"
		}
		if target.Scheme != "http" && target.Scheme != "https" {
			fmt.Printf("\nScheme for %s should be http or https\n\n", endpoint)
			cli.ShowAppHelpAndExit(ctx, 1)
			return nil
		}
		if target.Host == "" {
			fmt.Printf("\nParse error for %s\n\n", endpoint)
			cli.ShowAppHelpAndExit(ctx, 1)
			return nil
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		if insecure {
			proxy.Transport = http.DefaultTransport
			proxy.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
				NextProtos:         []string{"http/1.1"},
				InsecureSkipVerify: true,
			}
		}
		backend := &Backend{endpoint, proxy, false, healthCheckPath, healthCheckDuration, logging}
		go backend.healthCheck()
		proxy.ErrorHandler = backend.ErrorHandler
		backends = append(backends, backend)
	}
	lb := &LoadBalancer{
		backends: backends,
	}
	return http.ListenAndServe(addr, lb)
}

func main() {
	app := cli.NewApp()
	app.Name = "MinIO Sidekick"
	app.Author = "MinIO, Inc."
	app.Description = `Run sidekick as a sidecar on the same server as your application. Sidekick will load balance the application requests across the backends.`
	app.UsageText = "sidekick [options] ENDPOINT_1 ENDPOINT_2 ENDPOINT_3 ... ENDPOINT_N"
	app.Version = "1.0.0"
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
	app.CustomAppHelpTemplate = `NAME:
  {{.Name}}

DESCRIPTION:
  {{.Description}}

USAGE:
  sidekick [FLAGS] ENDPOINT_1 ENDPOINT_2 ENDPOINT_3 ... ENDPOINT_N

FLAGS:
  {{range .VisibleFlags}}{{.}}
  {{end}}
VERSION:
  {{.Version}}

EXAMPLES:
  1. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000)
     $ sidekick http://minio1:9000 http://minio2:9000 http://minio3:9000 http://minio4:9000

  2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), listen on port 8000
     $ sidekick --address ":8000" http://minio1:9000 http://minio2:9000 http://minio3:9000 http://minio4:9000

  3. Load balance across 4 MinIO Servers using HTTPS and disable TLS certificate validation
     $ sidekick --insecure https://minio1:9000 https://minio2:9000 https://minio3:9000 https://minio4:9000
`
	app.Before = sidekickMain
	app.RunAndExitOnError()
}
