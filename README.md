<h1 align="center">kubectl-count</h1>
<p align="center">
  <em>🎊 Show resources count in the cluster.</em>
</p>

kubectl-count uses the dynamic library to found server preferred resources and then leverages informer mechanism to list resources.

### 🔰 Installation

Build from source code

```shell
$ git clone https://github.com/chenjiandongx/kubectl-count.git
$ cd kubectl-count && go build -ldflags="-s -w" -o kubectl-count . && mv ./kubectl-count /usr/local/bin
$ kubectl count --help
```

Download the binary

```shell
# Refer to the link: https://github.com/chenjiandongx/kubectl-count/releases
# Download the binary and then...
$ chmod +x kubectl-count && mv kubectl-count /usr/local/bin/
$ kubectl count --help
```

### 📝 Usage

```shell
~ 🐶 kubectl count --help
Show resources count in the cluster.

Usage:
  kubectl-count <kinds> [flags]

Examples:
  # display a table of specified resources count, resources split by comma.
  kubectl count pods,ds,deploy

  # display kube-system cluster count info in yaml format.
  kubectl count -oy -n kube-system rs,ep

Flags:
  -A, --all-namespaces                 if present, resources aggregated by all namespaces
      --as string                      Username to impersonate for the operation. User could be a regular user or a service account in a namespace.
      --as-group stringArray           Group to impersonate for the operation, this flag can be repeated to specify multiple groups.
      --as-uid string                  UID to impersonate for the operation.
      --cache-dir string               Default cache directory (default "/root/.kube/cache")
      --certificate-authority string   Path to a cert file for the certificate authority
      --client-certificate string      Path to a client certificate file for TLS
      --client-key string              Path to a client key file for TLS
      --cluster string                 The name of the kubeconfig cluster to use
      --context string                 The name of the kubeconfig context to use
  -h, --help                           help for kubectl-count
      --insecure-skip-tls-verify       If true, the server's certificate will not be checked for validity. This will make your HTTPS connections insecure
      --kubeconfig string              Path to the kubeconfig file to use for CLI requests.
  -n, --namespace string               If present, the namespace scope for this CLI request
  -O, --order string                   used to sort the counts in ascending or descending order. [asc(a)|desc(d)] (default "asc")
  -o, --output-format string           output format. [json(j)|table(t)|yaml(y)] (default "table")
      --request-timeout string         The length of time to wait before giving up on a single server request. Non-zero values should contain a corresponding time unit (e.g. 1s, 2m, 3h). A value of zero means don't timeout requests. (default "0")
  -s, --server string                  The address and port of the Kubernetes API server
      --tls-server-name string         Server name to use for server certificate validation. If it is not provided, the hostname used to contact the server is used
      --token string                   Bearer token for authentication to the API server
      --user string                    The name of the kubeconfig user to use
  -v, --version                        version for kubectl-count
```

### 🔖 Glances

```shell
~ 🐶 kubectl count -n kube-system deploy,ds,rs
+-------------+------------+--------------+-------+
|  Namespace  |    Kind    | GroupVersion | Count |
+-------------+------------+--------------+-------+
| kube-system | Deployment | apps/v1      |     2 |
+             +------------+              +       +
|             | DaemonSet  |              |       |
+             +------------+              +       +
|             | ReplicaSet |              |       |
+-------------+------------+--------------+-------+

~ 🐶 kubectl count pods,ep,service -A
+-----------+------------+------------------------+-------+
| Namespace |    Kind    |      GroupVersion      | Count |
+-----------+------------+------------------------+-------+
|           | Pod        | v1                     |   712 |
+-----------+------------+------------------------+-------+
|           | PodMetrics | metrics.k8s.io/v1beta1 |   628 |
+-----------+------------+------------------------+-------+
|           | Endpoints  | v1                     |   239 |
+-----------+------------+                        +-------+
|           | Service    |                        |   237 |
+-----------+------------+------------------------+-------+

~ 🐶 kubectl count service,ds,rs -oy -A
- namespace: ""
  kind: Service
  groupVersion: v1
  count: 237
- namespace: ""
  kind: DaemonSet
  groupVersion: apps/v1
  count: 11
- namespace: ""
  kind: ReplicaSet
  groupVersion: apps/v1
  count: 1585

~ 🐶 kubectl count service,ds,rs -oj -A
[
 {
  "namespace": "",
  "kind": "Service",
  "groupVersion": "v1",
  "count": 237
 },
 {
  "namespace": "",
  "kind": "DaemonSet",
  "groupVersion": "apps/v1",
  "count": 11
 },
 {
  "namespace": "",
  "kind": "ReplicaSet",
  "groupVersion": "apps/v1",
  "count": 1585
 }
]
```

### 📃 License

MIT [©chenjiandongx](https://github.com/chenjiandongx)
