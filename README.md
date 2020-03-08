![sidekick](sidekick_logo.png)

*sidekick* is a high-performance sidecar load-balancer. By attaching a tiny load balancer as a sidecar to each of the client application processes, you can eliminate the centralized loadbalancer bottleneck and DNS failover management. *sidekick* automatically avoids sending traffic to the failed servers by checking their health via the readiness API and HTTP error returns.

# Download
[Download Binary Releases](https://github.com/minio/sidekick/releases) for various platforms.

# Usage

```
USAGE:
  sidekick [FLAGS] ENDPOINTs...
  sidekick [FLAGS] ENDPOINT{1...N}

FLAGS:
  --address value, -a value          listening address for sidekick (default: ":8080")
  --health-path value, -p value      health check path (default: "/health/ready")
  --health-duration value, -d value  health check duration (default: 5s)
  --insecure, -i                     disable TLS certificate verification
  --log , -l                         enable logging
  --trace, -t                        enable HTTP tracing
  --help, -h                         show help
  --version, -v                      print the version
```

## Examples

1. Load balance across a web service using DNS provided IPs.
```
$ sidekick http://myapp.myorg.dom
```

2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000)
```
$ sidekick --address :8000 http://minio{1...4}:9000
```

3. Load balance across 16 MinIO Servers (http://minio1:9000 to http://minio16:9000)
```
$ sidekick --health-path=/minio/health/ready http://minio{1...16}:9000
```

## Roadmap
1. S3 Cache: Use an S3 compatible object storage for shared cache storage
