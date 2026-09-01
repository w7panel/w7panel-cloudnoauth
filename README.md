# w7panel-cloudnoauth

`w7panel-cloudnoauth` 是运行在业务 Pod 内的 Kubernetes Sidecar。它通过 iptables
透明接管业务容器访问 `api.w7.cc` 的出站流量，并提供一个 Pod 内可调用的签名验证 API。
它不接管、路由或转发任何入站业务请求。

## 设计目标

业务代码继续使用原来的地址访问 API：

```text
https://api.w7.cc/...
```

业务容器不需要修改出站 URL，也不需要显式配置 HTTP 代理：

```text
出站：业务容器 -> Pod OUTPUT -> Sidecar:15080/15443 -> api.w7.cc
验签：业务容器 -> http://127.0.0.1:15080/api/signature/verify
```

## Pod 内的组件

一个业务 Pod 包含两个容器：

```text
+------------------------- Pod --------------------------+
|                                                         |
|  业务容器                                               |
|    - 监听 8080                                          |
|    - 请求 https://api.w7.cc                             |
|                                                         |
|  w7panel-cloudnoauth-iptables InitContainer            |
|    - root + NET_ADMIN 初始化 iptables                  |
|                                                         |
|  w7panel-cloudnoauth Sidecar                            |
|    - 15080: 出站 HTTP                                   |
|    - 15443: 出站 HTTPS                                  |
|    - /api/signature/verify: 本地签名验证                |
|    - 直接以 UID 1337 运行 Go 进程                       |
|                                                         |
|  共享网络命名空间和 TLS 证书卷                          |
+---------------------------------------------------------+
```

Sidecar 通过 Downward API 注入的 `POD_NAME` 和 namespace 查询当前 Pod。它优先从 Pod
的 `w7.cc/owner-group-name`、`w7.cc/parent-group-name` 或 `w7.cc/group-name` 元数据解析
AppGroup；Pod 没有这些元数据时，沿 Pod -> ReplicaSet -> Deployment 查找所属 AppGroup，
再读取 `appid` 和 `appsecret`。

## 启动流程

1. InitContainer 以 root 和 `NET_ADMIN` 执行 [scripts/iptables-setup.sh](scripts/iptables-setup.sh)。
2. 创建并刷新 `W7PANEL_OUTBOUND` NAT 链。
3. 将 ZPK 为 `api.w7.cc` 注入的固定虚拟 IP 写入出站重定向规则。
4. Sidecar 主容器直接以 `SIDECAR_RUNTIME_UID`（默认 `1337`）启动 Go 进程。

只有 InitContainer 需要 `NET_ADMIN`。Sidecar 主容器不再需要 root、`SETUID`、`SETGID`
或 `NET_ADMIN` capability。

## 出站流程

### 1. iptables 接管

Sidecar 将自定义链挂到 Pod 网络命名空间的 `nat/OUTPUT`：

```text
业务进程连接 api.w7.cc:80/443
        |
        v
nat/OUTPUT
        |
        v
W7PANEL_OUTBOUND
        |
        +-- 80  -> REDIRECT 到 15080
        +-- 443 -> REDIRECT 到 15443
```

规则只匹配 `sidecar.virtualIP`（默认 `198.18.0.1`）。Sidecar 自身使用 UID 1337，
`--uid-owner 1337 -j RETURN` 会让它发往上游的连接直接放行，避免代理请求再次被
OUTPUT 链劫持形成循环。

ZPK 通过 `w7panel-cloudnoauth.hostAliases` 把 `api.w7.cc` 映射到虚拟 IP。Sidecar
Chart 会在当前业务 Release 的命名空间创建一个独立的 ExternalName Service。Sidecar
访问真实上游时只在 TCP 拨号阶段改连这个 Service，由 CoreDNS 解析 `api.w7.cc` 的
真实地址。请求的 HTTP Host 和 TLS SNI 仍保持 `api.w7.cc`，同时不会再次连接虚拟 IP。

生成的资源类似：

```yaml
apiVersion: v1
kind: Service
metadata:
  name: <release>-w7panel-cloudnoauth-upstream
  namespace: <业务 release namespace>
spec:
  type: ExternalName
  externalName: api.w7.cc
```

### 2. HTTP 出站代理

请求进入 `Outbound.Forward` 后会执行以下步骤：

1. 校验请求 Host 是否在 `outbound.allowed_host` 中，默认只允许 `api.w7.cc`。
2. 对非 `/` 路径读取请求体。
3. 对 JSON、`application/x-www-form-urlencoded` 或 `multipart/form-data` 请求追加签名字段：
   `appid`、`timestamp`、`nonce`、`sign`。
4. 如果请求体已经包含 `sign`，认为调用方已经签名，保持原请求体不重复签名。
5. 通过反向代理转发到 `https://api.w7.cc`（协议可由 `outbound.scheme` 配置）。

签名使用当前 Pod 所属 AppGroup 的 `appsecret`。AppGroup 不存在时，出站请求会
保持原请求体继续转发；其他凭据或解析错误会返回服务端错误。

### 3. HTTPS 出站代理

HTTPS 流量会被重定向到 Sidecar 的 `15443`。Sidecar 使用挂载的 `api.w7.cc` 叶子证书
和私钥与业务容器完成 TLS，再将请求转发给上游 API。因为业务容器看到的是 Sidecar
提供的证书，所以业务容器仍必须信任该证书的根 CA。

## 签名验证 API

业务应用收到平台请求后，可以把原始 JSON、`application/x-www-form-urlencoded` 或
`multipart/form-data` 请求体提交到 Pod 内的 Sidecar：

```text
POST http://127.0.0.1:15080/api/signature/verify
Content-Type: application/json

{"appid":"...","timestamp":123,"nonce":"...","sign":"...","action":"ping"}
```

Sidecar 使用当前 Pod 所属 AppGroup 的 `appsecret` 计算签名，并以常量时间比较签名值。
验证成功返回 HTTP `200`：

```json
{"valid":true,"appid":"...","appgroup":"..."}
```

请求体格式错误返回 HTTP `400`，签名缺失、`appid` 不匹配或签名错误返回 HTTP `401`，
AppGroup 凭据暂时无法读取返回 HTTP `503`。该接口只验证签名，不修改请求体，也不会向
业务容器、Higress 或其他地址转发请求。

## Sidecar 自身接口

这些接口由独立的 `Sidecar` controller 提供，不属于出站代理：

| 接口 | 用途 |
| --- | --- |
| `GET /api/live` | Kubernetes liveness/readiness 探针 |
| `GET /api/app/info` | 根据 Pod 的 AppGroup 返回 `appid` 和 `appsecret` |
| `POST /api/signature/verify` | 验证 JSON、URL 编码表单或 multipart 表单请求体中的平台签名 |

其他请求由 `Outbound` controller 处理并转发到 `api.w7.cc`。

## Helm 接入

本项目发布为 `sidecar` 类型的 Helm library chart。业务制品启用“创建站点”后，
w7panel-zpk 自动将本 Chart 作为本地依赖一起打包，并在生成业务 Helm 模板时调用下列
模板契约：

```yaml
metadata:
spec:
  hostAliases:
    {{- include "w7panel-cloudnoauth.hostAliases" . | nindent 4 }}
  initContainers:
    {{- include "w7panel-cloudnoauth.initContainer" . | nindent 4 }}
  containers:
    - name: application
      # 业务容器，默认监听 8080
    {{- include "w7panel-cloudnoauth.container" . | nindent 4 }}
  volumes:
    {{- include "w7panel-cloudnoauth.volumes" . | nindent 4 }}
```

Chart annotations 声明模板入口。可选的 `w7.cc/sidecar-resources-template` 可输出一次性的
配套 Kubernetes 资源；本制品用它
生成读取当前 Pod、ReplicaSet、Deployment 和 AppGroup 所需的 Role/RoleBinding，其他
sidecar 可用同一出口生成 PVC、Secret、ConfigMap、Service 等资源。
`w7.cc/sidecar-host-aliases-template` 输出需要合并到目标 PodSpec 的 `hostAliases`；
ZPK 注入器应保留业务已有条目，并按 IP 和 hostname 去重。

Job 使用 `w7panel-cloudnoauth.jobContainer`，它把长期运行的代理声明为
`restartPolicy: Always` 的 Kubernetes 原生 sidecar initContainer；iptables 仍由更早执行且
会退出的 `w7panel-cloudnoauth.initContainer` 初始化。因此业务 Job 结束后不会被代理阻塞。

`w7.cc/inject-root-ca: "true"` 继续复用 w7panel-server 既有根 CA 注入逻辑。
`inject-sidecar` 注解和 server 动态拉取 sidecar 制品的逻辑已不再使用。

Sidecar 的运行时默认值随制品一起发布在 [charts/values.yaml](charts/values.yaml) 的
`sidecar` 节点中，由 w7panel-zpk 随业务 Chart 一起打包。常用运行参数已由
[config.yaml](config.yaml) 提供默认值，生成的 Sidecar 容器只传入 Pod 身份以及用户显式
覆盖的环境变量。

| 配置 | 默认值 | 说明 |
| --- | --- | --- |
| `sidecar.image.repository` | `zpk.w7.cc/public/w7panel-cloudnoauth` | Sidecar 镜像仓库 |
| `sidecar.image.tag` | `v1.0.19` | Sidecar 镜像版本 |
| `sidecar.virtualIP` | `198.18.0.1` | 仅在当前 Pod 内用于接管 `api.w7.cc` 的虚拟 IPv4 |
| `sidecar.upstream.serviceName` | 自动使用 `<release>-w7panel-cloudnoauth-upstream` | 当前业务 Release 内的 ExternalName Service 名称 |
| `sidecar.upstream.caFile` | `/etc/ssl/certs/ca-certificates.crt` | 验证真实上游 TLS 的 CA bundle |
| `sidecar.env` / `sidecar.extraEnv` | 空 | 必要时覆盖默认运行配置 |

## 证书要求

### Sidecar 证书

Sidecar 的 TLS 卷由 cert-manager CSI Driver 挂载。证书的 SAN 必须包含 profile 中的
目标主机（默认是 `api.w7.cc`），否则业务进程访问 `https://api.w7.cc` 时会因
主机名校验失败而拒绝连接。

叶子证书和私钥只挂载到 Sidecar：

```text
/var/run/w7panel-cloudnoauth/tls.crt
/var/run/w7panel-cloudnoauth/tls.key
```

业务容器不需要也不应该读取 `tls.key`。私钥只用于 Sidecar 在 `15443` 接收被重定向的
HTTPS 连接。

### 业务 Pod 证书要求

业务进程原本认为自己直接连接 `api.w7.cc`，但透明接管后，第一次 TLS 握手实际发生在
业务进程与 Sidecar 之间。因此业务容器必须信任“签发 Sidecar 叶子证书的根 CA”。

业务 Pod 的要求如下：

1. Pod metadata 必须包含 `w7.cc/inject-root-ca: "true"`。library chart 提供的
   `w7panel-cloudnoauth.podAnnotations` 模板会生成该 annotation；w7panel-zpk 在业务制品
   启用“创建站点”时也会自动补上它。
2. 集群中必须启用 w7panel 的既有根 CA admission 注入，把根 CA 放入业务容器可访问的
   信任库或证书目录。Sidecar 容器本身由 Helm 静态生成，不依赖 admission 动态注入。
3. 业务镜像必须包含系统 CA 支持，例如 Alpine 的 `ca-certificates` 包。缺少系统信任库时，
   即使证书文件已经注入，HTTPS 客户端也可能无法加载它。
4. 业务运行时必须实际使用注入后的信任库。Go、curl 和大多数 PHP/OpenSSL 客户端通常
   使用系统 CA；Java、Node.js 或自定义 HTTP Client 可能使用独立 truststore，需要通过
   `javax.net.ssl.trustStore`、`NODE_EXTRA_CA_CERTS`、`SSL_CERT_FILE` 等运行时配置显式加载。
5. 业务容器只需要根 CA，不需要 Sidecar 的叶子证书、私钥或客户端证书。当前实现不是
   mTLS，业务进程不需要向 Sidecar 提供客户端证书。

证书关系如下：

```text
业务容器信任：Root CA
                    |
                    +-- 签发 api.w7.cc 叶子证书
                                      |
业务容器 -- TLS --> Sidecar:15443 -- TLS --> api.w7.cc
```

可以在业务容器内验证证书信任和主机名：

```bash
kubectl exec <pod> -c <业务容器> -- \
  curl -v https://api.w7.cc/
```

如果出现 `certificate signed by unknown authority`、`unable to get local issuer certificate`
或类似错误，说明根 CA 没有注入成功，或者业务运行时没有使用注入后的信任库。如果出现
主机名不匹配错误，则需要检查 Sidecar 证书的 SAN 是否包含 `api.w7.cc`。

## 故障排查

查看 Pod 内 NAT 规则：

```bash
kubectl exec -it <pod> -c w7panel-cloudnoauth -- iptables -t nat -S
```

预期能看到：

```text
-A OUTPUT -j W7PANEL_OUTBOUND
```

检查 Sidecar 监听端口：

```bash
kubectl exec <pod> -c w7panel-cloudnoauth -- wget -qO- http://127.0.0.1:15080/api/live
```

如果出站未被接管，先确认 `getent hosts api.w7.cc` 返回 `sidecar.virtualIP`，再检查
`W7PANEL_OUTBOUND` 是否包含该虚拟 IP 的 80/443 规则。上游连接失败时检查
`API_PROXY_UPSTREAM_HOST` 指向的 ExternalName Service 是否存在并能由 CoreDNS 解析，以及 Sidecar 日志中的
上游 CA 错误。验签 API 返回 `401` 时检查请求体中的 `appid`、`timestamp`、`nonce`、
`sign` 是否使用对应 AppGroup 的密钥生成；返回 `503` 时检查 Pod/AppGroup 元数据和 RBAC。
