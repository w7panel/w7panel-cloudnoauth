#!/bin/sh
set -eu

api_host="${API_PROXY_ALLOWED_HOST:-api.w7.cc}"
outbound_http_port="${PROXY_HTTP_PORT:-15080}"
outbound_https_port="${PROXY_HTTPS_PORT:-15443}"
sidecar_runtime_uid="${SIDECAR_RUNTIME_UID:-1337}"
inbound_target_port="${INBOUND_TARGET_PORT:-8080}"
inbound_redirect_port="${INBOUND_LISTEN_PORT:-15081}"
outbound_chain_name="W7PANEL_OUTBOUND"
inbound_chain_name="W7PANEL_INBOUND"

# 首次启动 Sidecar 时创建出站 NAT 链；链已存在时忽略错误。
iptables -t nat -N "$outbound_chain_name" 2>/dev/null || true
# 清空旧 Sidecar 进程遗留的出站规则，避免重复添加。
iptables -t nat -F "$outbound_chain_name"

# 检查 Pod 内本机产生的流量是否已经进入自定义出站链。
if ! iptables -t nat -C OUTPUT -j "$outbound_chain_name" 2>/dev/null; then
	# 将自定义出站链挂到 OUTPUT，使 Pod 内产生的流量进入该链。
	iptables -t nat -A OUTPUT -j "$outbound_chain_name"
fi

# 放行 Sidecar 自身产生的流量，避免代理请求被再次劫持形成循环。
iptables -t nat -A "$outbound_chain_name" -m owner --uid-owner "$sidecar_runtime_uid" -j RETURN

api_ipv4_addresses="$(getent hosts "$api_host" | awk '$1 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ {print $1}' | sort -u)"
if [ -z "$api_ipv4_addresses" ]; then
	echo "failed to resolve IPv4 addresses for $api_host" >&2
	exit 1
fi

for api_ipv4_address in $api_ipv4_addresses; do
	# 将访问 API IPv4 地址 80 端口的请求重定向到 Sidecar 出站 HTTP 监听端口。
	iptables -t nat -A "$outbound_chain_name" -p tcp -d "$api_ipv4_address/32" --dport 80 \
		-j REDIRECT --to-ports "$outbound_http_port"
	# 将访问 API IPv4 地址 443 端口的请求重定向到 Sidecar 出站 TLS 监听端口。
	iptables -t nat -A "$outbound_chain_name" -p tcp -d "$api_ipv4_address/32" --dport 443 \
		-j REDIRECT --to-ports "$outbound_https_port"
done

# 首次启动 Sidecar 时创建入站 NAT 链；链已存在时忽略错误。
iptables -t nat -N "$inbound_chain_name" 2>/dev/null || true
# 清空旧 Sidecar 进程遗留的入站规则，避免重复添加。
iptables -t nat -F "$inbound_chain_name"

# 检查进入 Pod 的数据包是否已经经过自定义入站链。
if ! iptables -t nat -C PREROUTING -j "$inbound_chain_name" 2>/dev/null; then
	# 将自定义入站链挂到 PREROUTING，在流量交给业务容器前进行处理。
	iptables -t nat -A PREROUTING -j "$inbound_chain_name"
fi
# 将业务容器端口的入站流量重定向到 Sidecar 验签监听端口。
iptables -t nat -A "$inbound_chain_name" -p tcp --dport "$inbound_target_port" \
	-j REDIRECT --to-ports "$inbound_redirect_port"
