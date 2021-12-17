---
layout: default
title: "Quick Reference"
parent: Configure
nav_order: 10
---

# Quick Reference

### Interpolation

```
$ENV_VAR
%{monitor.hostname}
```

### Config File

Following is a full Blip config file (YAML syntax).
This is only a reference to show all configuration variables.

```yaml
---
# ---------------------------------------------------------------------------
# Blip server
# ---------------------------------------------------------------------------

api:
  bind: 127.1:7090
  disable: false

http:
  proxy: <addr>

monitor-loader:
  freq: 60s
  files: [one.yaml, two.yaml]
  stop-loss: 50%
  aws:
    regions: ["auto","us-east-1"]
  local:
    disable-auto: true
    disable-auto-root: true

strict: true

# ---------------------------------------------------------------------------
# Defaults for monitors
# ---------------------------------------------------------------------------

aws-rds:
  iam-auth-token: true
  password-secret: "arn::::"
  region: "us-east-1"
  disable-auto-region: false
  disable-auto-tls: false

exporter:
  mode: dual|legacy
  flags:
    web.listen-address: :9001

heartbeat:
  freq: 1s
  table: blip.heartbeat
  create-table: try

mysql:
  mycnf: my.cnf
  username: blip
  password: blip
  password-file: ""
  timeout-connect: 5s

plans:
  files:
    - foo.yaml
    - bar.yaml
  table: blip.plans
  monitor: <monitor>
  adjust:
    offline:
      after: 1s
      plan: "" # collect nothing
    standby:
      after: 1s
      plan: "" # collect nothing
    read-only:
      after: 1s
      plan: ro-plan
    active:
      after: 1s
      plan: active-plan

sinks:
  signalfx:
    auth-token: ""
    auth-token-file: ""
    send-timeout: 2s
  log:
    # No options

tags:
  env: ${ENVIRONMENT:-dev}
  dc: ${DATACENTER:-local}
  hostname: %{monitor.hostname}

tls:
  ca: square.ca
  cert: /app/secrets/$%{monitor.hostname}.crt
  key: /app/secrets/%{monitor.hostname}.key

# ---------------------------------------------------------------------------
# MySQL instances to monitor
# ---------------------------------------------------------------------------

monitors:
  - id: host1
    hostname: host1.local
    socket: /tmp/mysql.sock
    # mysql:
    mycnf: my.cnf
    username: metrics
    password: foo
    password-file: /dev/shm/mypasswd
    timeout-connect: 5s
    aws-rds:
      password-secret: "arn::::"
      iam-auth-token: true
    exporter:
      bind: 127.0.0.1:9001
      legacy: false
    heartbeat:
      freq: 1s
      table: blip.heartbeat
      create-table: try
    ha:
      # Reserved
    plans:
      table: "blip.plans"
      #monitor: <monitor>
      adjust:
        readonly:
          after: 2s
          plan: ro.yaml
        active:
          after: 1s
          plan: rw.yaml
    sinks:
      signalfx:
        auth-token: ""
        auth-token-file: ""
        send-timeout: 2s
      log:
        # No options
      chronosphere:
        url: http://127.0.0.1:3030/openmetrics/write
    tags:
      env: staging
      monitor-id: %{monitor.id}
    tls:
      ca: my-ca
      cert: ${SECRETS}/%{monitor.hostname}.cert
      key:  ${SECRETS}/%{monitor.hostname}.key
    meta:
      source: host2.local
      canary: no
```

## Environment Variables

Most config options have a corresponding environment variable, like `SPINCYCLE_RM_CLIENT_URL` for `rm_client.url`. Exceptions are noted.

Take a config option, change `.` to `_`, upper-case everything, and add `SPINCYCLE_` prefix.