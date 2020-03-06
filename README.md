# Sidekick
Sidekick is a load-balancer run as a sidecar.

Sidekick is meant to be run on the same server as your S3 client application. The S3 client application sends S3 requests to Sidekick which in-turn load balances the requests across different MinIO servers.

## Usage

```
USAGE:
  sidekick [FLAGS] ENDPOINT_1 ENDPOINT_2 ENDPOINT_3 ... ENDPOINT_N

FLAGS:
  --address value, -a value          listening address for sidekick (default: ":8080")
  --health-path value, -p value      health check path (default: "/minio/health/ready")
  --health-duration value, -d value  health check duration (default: 5s)
  --insecure, -i                     disable TLS certificate verification
  --logging, -l                      enable debug logging
  --help, -h                         show help
  --version, -v                      print the version
```

## Examples

1. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000)
```
$ sidekick http://minio1:9000 http://minio2:9000 http://minio3:9000 http://minio4:9000
```

2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), listen on port 8000
```
$ sidekick --address :8000 http://minio1:9000 http://minio2:9000 http://minio3:9000 http://minio4:9000
```

3. Load balance across 4 MinIO Servers using HTTPS and disable TLS certificate validation
```
$ sidekick --insecure https://minio1:9000 https://minio2:9000 https://minio3:9000 https://minio4:9000
```
