![sidekick](sidekick_logo.png)

# Sidekick
Sidekick is a high-performance sidecar load-balancer. By attaching a tiny load balancer as a sidecar to each of the client application processes, you can eliminate the centralized loadbalancer bottleneck and DNS failover management.  Sidekick automatically avoids sending traffic to the failed servers by checking their health via the readiness API and HTTP error returns.

## Usage

```
USAGE:
  sidekick [FLAGS] ENDPOINTs...

FLAGS:
  --adsdress value, -a value         listening address for sidekick (default: ":8080")
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
$ sidekick --health-path=/minio/health/ready http://minio1:9000 http://minio2:9000 http://minio3:9000 http://minio4:9000
```

3. Load balance across 16 MinIO Servers (http://minio1:9000 to http://minio16:9000)
```
$ sidekick --health-path=/minio/health/ready http://minio{1...16}:9000
```

## Roadmap
1. S3 Cache: Use an S3 compatible object storage for shared cache storage
