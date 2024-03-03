![sidekick](https://raw.githubusercontent.com/minio/sidekick/master/sidekick_logo.png)

![build](https://github.com/minio/sidekick/workflows/Go/badge.svg) ![license](https://img.shields.io/badge/license-AGPL%20V3-blue)

![GitHub Downloads][gh-downloads]

*sidekick* is a high-performance sidecar load-balancer. By attaching a tiny load balancer as a sidecar to each of the client application processes, you can eliminate the centralized loadbalancer bottleneck and DNS failover management. *sidekick* automatically avoids sending traffic to the failed servers by checking their health via the readiness API and HTTP error returns.

# Architecture
![architecture](https://raw.githubusercontent.com/minio/sidekick/master/arch_sidekick.png)

**Demo** ![sidekick-demo](https://raw.githubusercontent.com/minio/sidekick/master/sidekick-demo.gif)

# Install

## Binary Releases

| OS      | ARCH    | Binary                                                                                                 |
|:-------:|:-------:|:------------------------------------------------------------------------------------------------------:|
| Linux   | amd64   | [linux-amd64](https://github.com/minio/sidekick/releases/latest/download/sidekick-linux-amd64)         |
| Linux   | arm64   | [linux-arm64](https://github.com/minio/sidekick/releases/latest/download/sidekick-linux-arm64)         |
| Linux   | ppc64le | [linux-ppc64le](https://github.com/minio/sidekick/releases/latest/download/sidekick-linux-ppc64le)     |
| Linux   | s390x   | [linux-s390x](https://github.com/minio/sidekick/releases/latest/download/sidekick-linux-s390x)         |
| Apple   | amd64   | [darwin-amd64](https://github.com/minio/sidekick/releases/latest/download/sidekick-darwin-amd64)       |
| Windows | amd64   | [windows-amd64](https://github.com/minio/sidekick/releases/latest/download/sidekick-windows-amd64.exe) |

You can also verify the binary with [minisign](https://jedisct1.github.io/minisign/) by downloading the corresponding [`.minisig`](https://github.com/minio/sidekick/releases/latest) signature file. Then run:
```
minisign -Vm sidekick-<OS>-<ARCH> -P RWTx5Zr1tiHQLwG9keckT0c45M3AGeHD6IvimQHpyRywVWGbP1aVSGav
```

## Docker

Pull the latest release via:
```
docker pull quay.io/minio/sidekick:v4.0.6
```

## Build from source

```
go install -v github.com/minio/sidekick@latest
```

> You will need a working Go environment. Therefore, please follow [How to install Go](https://golang.org/doc/install).
> Minimum version required is go1.17

# Usage

```
NAME:
  sidekick - High-Performance sidecar load-balancer

USAGE:
  sidekick COMMAND [COMMAND FLAGS | -h] [ARGUMENTS...]

COMMANDS:
  help, h  Shows a list of commands or help for one command
  
FLAGS:
  --address value, -a value           listening address for sidekick (default: ":8080")
  --health-path value, -p value       health check path
  --read-health-path value, -r value  health check path for read access - valid only for failover site
  --health-port value                 health check port (default: 0)
  --health-duration value, -d value   health check duration in seconds (default: 5s)
  --insecure, -i                      disable TLS certificate verification
  --log, -l                           enable logging
  --trace value, -t value             enable request tracing - valid values are [all,application,minio] (default: "all")
  --quiet, -q                         disable console messages
  --json                              output sidekick logs and trace in json format
  --debug                             output verbose trace
  --cacert value                      CA certificate to verify peer against
  --client-cert value                 client certificate file
  --client-key value                  client private key file
  --cert value                        server certificate file
  --key value                         server private key file
  --help, -h                          show help
  --version, -v                       print the version
```

## Examples

### Load balance across a web service using DNS provided IPs.
```
$ sidekick --health-path=/ready http://myapp.myorg.dom
```

### Load balance across 4 MinIO Servers.
http://minio1:9000 to http://minio4:9000
```
$ sidekick --health-path=/minio/health/ready --address :8000 http://minio{1...4}:9000
```

### Load balance across 2 sites with 4 servers each
```
$ sidekick --health-path=/minio/health/ready http://site1-minio{1...4}:9000 http://site2-minio{1...4}:9000
```

## Realworld Example with spark-operator

With spark as *driver* and sidecars as *executor*, first install spark-operator and MinIO on your kubernetes cluster.

### Configure *spark-operator*

This guide uses the maintained spark operator by GCP at https://github.com/GoogleCloudPlatform/spark-on-k8s-operator.

```
helm repo add spark-operator https://googlecloudplatform.github.io/spark-on-k8s-operator
helm --namespace spark-operator install spark-operator spark-operator/spark-operator --create-namespace --set sparkJobNamespace=spark-operator --set enableWebhook=true
```

### Install *MinIO*. 

Ensure that the `standard` storage class was previously installed.
Note that TLS is disabled for this test. Note also that the minio tenant created is called `myminio`.

```
helm repo add minio-operator https://operator.min.io/
helm install operator minio-operator/operator --namespace minio-operator --create-namespace
  
helm install myminio minio-operator/tenant --namespace tenant-sidekick --create-namespace && \
kubectl --namespace tenant-sidekick patch tenant myminio --type='merge' -p '{"spec":{"requestAutoCert":false}}'
```

Once the tenant pods are running, port-forward the minio headless service to access it locally.
```
kubectl --namespace tenant-sidekick port-forward svc/myminio-hl 9000 &
```

Configure [`mc`](https://github.com/minio/mc) and upload some data. Use `mybucket` as the s3 bucket name.
Create bucket named `mybucket` and upload some text data for spark word count sample.
```
mc alias set myminio http://localhost:9000 minio minio123
mc mb myminio/mybucket
mc cp /etc/hosts myminio/mybucket/mydata.txt
```

### Run the spark job in k8s

Obtain the ip address and port of the `minio` service. Use them as input to `fs.s3a.endpoint` the below SparkApplication. e.g. http://10.43.141.149:80
```
kubectl --namespace tenant-sidekick get svc/minio
```

Create the `spark-minio-app` yml
```
cat << EOF > spark-job.yaml
apiVersion: "sparkoperator.k8s.io/v1beta2"
kind: SparkApplication
metadata:
  name: spark-minio-app
  namespace: spark-operator
spec:
  sparkConf:
    spark.kubernetes.allocation.batch.size: "50"
  hadoopConf:
    "fs.s3a.endpoint": "http://10.43.141.149:80"
    "fs.s3a.access.key": "minio"
    "fs.s3a.secret.key": "minio123"
    "fs.s3a.path.style.access": "true"
    "fs.s3a.impl": "org.apache.hadoop.fs.s3a.S3AFileSystem"
  type: Scala
  sparkVersion: 2.4.5
  mode: cluster
  image: minio/spark:v2.4.5-hadoop-3.1
  imagePullPolicy: Always
  restartPolicy:
      type: OnFailure
      onFailureRetries: 3
      onFailureRetryInterval: 10
      onSubmissionFailureRetries: 5
      onSubmissionFailureRetryInterval: 20
  mainClass: org.apache.spark.examples.JavaWordCount
  mainApplicationFile: "local:///opt/spark/examples/target/original-spark-examples_2.11-2.4.6-SNAPSHOT.jar"
  arguments:
  - "s3a://mybucket/mydata.txt"
  driver:
    cores: 1
    memory: "512m"
    labels:
      version: 2.4.5
    sidecars:
    - name: minio-lb
      image: "quay.io/minio/sidekick:v4.0.3"
      imagePullPolicy: Always
      args: ["--health-path", "/minio/health/ready", "--address", ":8080", "http://myminio-pool-0-{0...3}.myminio-hl.tenant-sidekick.svc.cluster.local:9000"]
      ports:
        - containerPort: 9000
          protocol: http
  executor:
    cores: 2
    instances: 4
    memory: "1024m"
    labels:
      version: 2.4.5
    sidecars:
    - name: minio-lb
      image: "quay.io/minio/sidekick:v4.0.3"
      imagePullPolicy: Always
      args: ["--health-path", "/minio/health/ready", "--address", ":8080", "http://myminio-pool-0-{0...3}.myminio-hl.tenant-sidekick.svc.cluster.local:9000"]
      ports:
        - containerPort: 9000
          protocol: http
EOF
```

Grant permissions to access resources to the service account
```
kubectl create clusterrolebinding spark-role --clusterrole=edit --serviceaccount=spark-operator:default --namespace=spark-operator
kubectl create -f spark-job.yaml
kubectl --namespace spark-operator logs -f spark-minio-app-driver
```

#### Monitor

The above SparkApplication will not complete until the Health check returns "200 OK"; in this case when there is MinIO read quorum. The Health check is provided at the path "/v1/health". It returns "200 OK" even if any one of the sites is reachable, else it returns "502 Bad Gateway" error.

[gh-downloads]: https://img.shields.io/github/downloads/minio/sidekick/total?color=pink&label=GitHub%20Downloads
