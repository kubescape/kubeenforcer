# Kubeenforcer
This project aims to provide a simple way to enforce policies on Kubernetes clusters. It is based on the [cel-admission-webhook](https://github.com/kubernetes/cel-admission-webhook) project.

## How it works
Kubeenforcer is a Kubernetes admission webhook that intercepts requests to the Kubernetes API server and evaluates a [CEL](https://github.com/google/cel-spec) expression. If the expression evaluates to true, the request is allowed to proceed. If the expression evaluates to false, the request is denied/audited.
Using this set of rules, you can enforce policies on your Kubernetes cluster and monitor on suspicious activity such as a pod trying to mount a hostPath volume or an exec request to a pod.

## Alerting
Kubeenforcer can be configured to send alerts to multiple destinations. Currently, the following destinations are supported:
- Alertmanager
    - Slack
    - Email
    - Pagerduty
    - Opsgenie
    - Victorops
    - Webhook
    - Wechat
    - Discord
    - Telegram

## Installation

### Using Helm:
To install the admission controller webhook:
```bash
git clone https://github.com/kubescape/kubeenforcer.git && cd kubeenforcer
kubectl create namespace kubescape
helm install kubeenforcer -n kubescape ./charts/kubeenforcer
```

### Installing Polices
*Example polices 🚧*
```bash
kubectl apply -f https://github.com/kubescape/cel-admission-library/releases/download/v0.6/kubescape-validating-admission-policies-x-v1alpha1.yaml
```

### Installing Policy Bindings
*Example Exec Binding 🚧*
```bash
kubectl apply -f https://raw.githubusercontent.com/kubescape/kubeenforcer/main/policies-bindings/exec/binding.yaml
```

### Installing Alertmanager (Optional)
If you want to see alerts on policies violations kubeenforcer supports streaming events to alertmanager, here is an example for a setup if you don't already have an alertmanager in your enviorment, if you do you can skip to the part of enabling alertmanager in kubeenforcer.
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: alertmanager-server-armo
  namespace: kubescape
  labels:
    app: alertmanager-server-armo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: alertmanager-server-armo
  template:
    metadata:
      labels:
        app: alertmanager-server-armo
    spec:
      containers:
      - name: alertmanager
        image: quay.io/prometheus/alertmanager:latest
        imagePullPolicy: Always
        ports:
        - containerPort: 9093
        volumeMounts:
        - name: alertmanager-config
          mountPath: /etc/alertmanager
      volumes:
      - name: alertmanager-config
        configMap:
          name: alertmanager-config
---
apiVersion: v1
kind: Service
metadata:
  name: armo-alertmanager
  namespace: kubescape
spec:
  type: NodePort
  selector:
    app: alertmanager-server-armo
  ports:
    - port: 9093
      targetPort: 9093
```

And the alert manager config:
```yaml
kind: ConfigMap
apiVersion: v1
metadata:
  name: alertmanager-config
  namespace: kubescape
data:
  alertmanager.yml: |-
    global:
      smtp_smarthost: smtp-server:port   # SMTP server and port
      smtp_from: sender@email.com    # Sender email address
      smtp_auth_username: ""     # SMTP username
      smtp_auth_password: ""     # SMTP password
      smtp_require_tls: true                # Require TLS encryption

    route:
      group_by: ['alertname']
      receiver: email-notifier

    receivers:
    - name: email-notifier
      email_configs:
      - to: your@email.com
```
To enable alertmanager in kubeenforcer:
```bash
helm upgrade --install kubeenforcer charts/kubeenforcer -n kubescape --set admissionWebhook.alertmanager.enabled=true --set admissionWebhook.alertmanager.endpoint=<ALERT_MANAGER_SERVICE_ENDPOINT:PORT>
```