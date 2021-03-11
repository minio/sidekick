# How to monitor Sidekick loadbalancer with Prometheus [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io)

[Prometheus](https://prometheus.io) is a cloud-native monitoring platform, built originally at SoundCloud. Prometheus offers a multi-dimensional data model with time series data identified by metric name and key/value pairs. The data collection happens via a pull model over HTTP/HTTPS. Targets to pull data from are discovered via service discovery or static configuration.

Sidekick exports Prometheus compatible data by default as an authorized endpoint at `/.prometheus/metrics`. Users looking to monitor their MinIO instances can point Prometheus configuration to scrape data from this endpoint.

This document explains how to setup Prometheus and configure it to scrape data from Sidekick.

**Table of Contents**

- [Prerequisites](#prerequisites)
    - [1. Download Prometheus](#1-download-prometheus)
    - [2. Configure authentication type for Prometheus metrics](#2-configure-authentication-type-for-prometheus-metrics)
    - [3. Configuring Prometheus](#3-configuring-prometheus)
        - [3.1 Authenticated Prometheus config](#31-authenticated-prometheus-config)
        - [3.2 Public Prometheus config](#32-public-prometheus-config)
    - [4. Update `scrape_configs` section in prometheus.yml](#4-update-scrapeconfigs-section-in-prometheusyml)
    - [5. Start Prometheus](#5-start-prometheus)
- [List of metrics exposed by Sidekick](#list-of-metrics-exposed-by-sidekick)

### 1. Download Prometheus

[Download the latest release](https://prometheus.io/download) of Prometheus for your platform, then extract it

```sh
tar xvfz prometheus-*.tar.gz
cd prometheus-*
```

Prometheus server is a single binary called `prometheus` (or `prometheus.exe` on Microsoft Windows). Run the binary and pass `--help` flag to see available options

```sh
./prometheus --help
usage: prometheus [<flags>]

The Prometheus monitoring server

. . .

```

Refer [Prometheus documentation](https://prometheus.io/docs/introduction/first_steps/) for more details.

### 2. Configure authentication type for Prometheus metrics

Sidekick supports `public` authentication mode for Prometheus.

```
$ sidekick --health-path=/ready http://myapp.myorg.dom
```

### 3. Configuring Prometheus

Following prometheus config is sufficient to start scraping metrics data from Sidekick.

```yaml
scrape_configs:
- job_name: sidekick-job
  metrics_path: /.prometheus/metrics
  scheme: http
  static_configs:
  - targets: ['localhost:8080']
```

### 5. Start Prometheus

Start (or) Restart Prometheus service by running

```sh
./prometheus --config.file=prometheus.yml
```

Here `prometheus.yml` is the name of configuration file. You can now see Sidekick metrics in Prometheus dashboard. By default Prometheus dashboard is accessible at `http://localhost:9090`.

## List of metrics exposed by Sidekick

Sidekick loadbalancer exposes the following metrics on `/.prometheus/metrics` endpoint. All of these can be accessed via Prometheus dashboard.

| Metrics Name              | Description                                                      |
|:-------------------------:|:----------------------------------------------------------------:|
| `sidekick_requests_total` | Total number of requests in current SideKick instance.              |
| `sidekick_errors_total`   | Total number of errors in requests in current SideKick instance.    |
| `sidekick_rx_bytes_total` | Total number of bytes received by current SideKick server instance. |
| `sidekick_tx_bytes_total` | Total number of bytes sent to current SideKick server instance.     |
