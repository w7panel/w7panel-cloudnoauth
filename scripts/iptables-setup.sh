#!/bin/sh
set -eu

virtual_api_ip="${API_PROXY_VIRTUAL_IP:-198.18.0.1}"
outbound_http_port="${PROXY_HTTP_PORT:-15080}"
outbound_https_port="${PROXY_HTTPS_PORT:-15443}"
sidecar_runtime_uid="${SIDECAR_RUNTIME_UID:-1337}"
outbound_chain_name="W7PANEL_OUTBOUND"

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

# 业务容器通过 Pod hostAliases 将目标域名解析到固定虚拟 IP。这里只匹配该地址，
# 不再依赖启动时的真实 DNS 结果。
iptables -t nat -A "$outbound_chain_name" -p tcp -d "$virtual_api_ip/32" --dport 80 \
	-j REDIRECT --to-ports "$outbound_http_port"
iptables -t nat -A "$outbound_chain_name" -p tcp -d "$virtual_api_ip/32" --dport 443 \
	-j REDIRECT --to-ports "$outbound_https_port"
