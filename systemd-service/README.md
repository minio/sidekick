# Systemd service for Sidekick

Systemd script for Sidekick Load Balancer.

## Installation

- Systemd script is configured to run the binary from /usr/local/bin/
- Download the binary from https://github.com/minio/sidekick/releases

## Create default configuration

```sh
$ cat <<EOF >> /etc/default/sidekick
# Sidekick options
SIDEKICK_OPTIONS="--health-path=/minio/health/ready --address :8000"

# Sidekick sites
SIDEKICK_SITES="http://172.17.0.{11...18}:9000 http://172.18.0.{11...18}:9000"

EOF
```

## Systemctl

Download sidekick.service in /etc/systemd/system/

## Enable startup on boot

```
systemctl enable sidekick.service
```

## Note
Replace User=nobody and Group=nobody in minio.service file with your local setup user.
