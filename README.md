# Sidekick

Sidekick is a load-balancer run as a sidecar.

Sidekick should be run on the same server as your S3 client application. The S3 client application sends S3 requests to Sidekick which in-turn load balances the requests across different MinIO servers.

## Usage

```
USAGE:
  sidekick [FLAGS] ENDPOINT_1 ENDPOINT_2 ENDPOINT_3 ... ENDPOINT_N

FLAGS:
  --address value, -a value          listening address for Sidekick (default: ":8080")
  --health-path value, -p value      Health check path
  --health-duration value, -d value  Health check duration (in seconds) (default: 5)
  --insecure, -i                     Disable TLS certificate verification
  --logging, -l                      Enable logging
  --help, -h                         show help
  --version, -v                      print the version
```

## Examples

  1. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000)
  ```
     $ sidekick --health-path /minio/health/ready http://minio1:9000 http://minio2:9000 http://minio3:9000 http://minio4:9000
  ```

  2. Load balance across 4 MinIO Servers (http://minio1:9000 to http://minio4:9000), listen on port 8000
  ```
     $ sidekick --address :8000 --health-path /minio/health/ready http://minio1:9000 http://minio2:9000 http://minio3:9000 http://minio4:9000
  ```

  3. Load balance across 4 MinIO Servers using HTTPS and disable TLS certificate validation
  ```
     $ sidekick --insecure --health-path /minio/health/ready https://minio1:9000 https://minio2:9000 https://minio3:9000 https://minio4:9000
  ```
