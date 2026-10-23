#!/usr/bin/env bash
#
# 保持一条到本地服务的公网隧道：ssh 断开后自动重连，并把当前公网地址写到文件。
#
# serveo 的 SSH 连接会周期性掉线（exit 255）。手敲 ssh 的后果是每次都要
# 重新复制链接、改 README，所以这里把「连上 -> 输出地址 -> 断了重连」做成循环。
#
# 用法:
#   scripts/tunnel.sh                    # 随机子域名，地址每次都可能变
#   SUBDOMAIN=matching-engine scripts/tunnel.sh   # 固定子域名，地址稳定
#
# 可选环境变量:
#   LOCAL_PORT  本地服务端口，默认 8080
#   REMOTE_HOST 隧道服务地址，默认 serveo.net
#   SUBDOMAIN   请求的固定子域名（不带 .serveo.net）
#   URL_FILE    当前地址写入的文件，默认 /tmp/matching-engine-tunnel.url
#
# 注意: 只有第一次成功连接后连上时，地址才有效；URL_FILE 会在每次重连前清空。

set -uo pipefail

LOCAL_PORT=${LOCAL_PORT:-8080}
REMOTE_HOST=${REMOTE_HOST:-serveo.net}
SUBDOMAIN=${SUBDOMAIN:-}
URL_FILE=${URL_FILE:-/tmp/matching-engine-tunnel.url}

# 重连退避：从 2 秒开始翻倍，封顶 60 秒，避免服务端故障时疯狂重连。
INITIAL_BACKOFF=2
MAX_BACKOFF=60

# 连接存活超过这个秒数才认为「隧道曾经正常工作过」，重连时把退避重置回初始值。
HEALTHY_UPTIME=30

# 请求固定子域名时，ssh 的远程转发端口要写成 "<子域名>:80"。
if [[ -n "$SUBDOMAIN" ]]; then
	remote_forward="$SUBDOMAIN:80:localhost:$LOCAL_PORT"
else
	remote_forward="80:localhost:$LOCAL_PORT"
fi

backoff=$INITIAL_BACKOFF

while true; do
	# 上一轮的地址已经失效，先清空，免得别人读到过期链接
	: >"$URL_FILE"
	echo "[$(date '+%H:%M:%S')] connecting to $REMOTE_HOST (forward $remote_forward)"

	started=$(date +%s)

	# 把 ssh 的输出逐行读出来：带 https:// 的行就是公网地址，其余当日志透传。
	# 用管道读而不是先写进日志再 tail，是为了读循环随 ssh 退出而结束。
	ssh \
		-o StrictHostKeyChecking=no \
		-o UserKnownHostsFile=/dev/null \
		-o ServerAliveInterval=30 \
		-o ServerAliveCountMax=3 \
		-o ExitOnForwardFailure=yes \
		-R "$remote_forward" "$REMOTE_HOST" 2>&1 |
		while IFS= read -r line; do
			# 只认 "Forwarding ... https://..." 这一行：serveo 在申请固定子域名时
			# 还会打印一个 console.serveo.net 的注册链接，那不是隧道地址。
			# 正则不加引号，因此字符类里不能出现空格，否则会被当成词的分隔符。
			if [[ $line == *Forwarding* && $line =~ https://[^[:space:]]+ ]]; then
				url=${BASH_REMATCH[0]}
				printf '%s\n' "$url" >"$URL_FILE"
				echo "[$(date '+%H:%M:%S')] public url: $url"
			else
				echo "  | $line"
			fi
		done

	# PIPESTATUS[0] 是 ssh 的退出码；管道右边的 while 总是正常结束
	status=${PIPESTATUS[0]}
	lived=$(($(date +%s) - started))

	if ((lived >= HEALTHY_UPTIME)); then
		backoff=$INITIAL_BACKOFF
	fi

	echo "[$(date '+%H:%M:%S')] tunnel exited (status $status, lasted ${lived}s), reconnecting in ${backoff}s"
	sleep "$backoff"
	backoff=$((backoff * 2 > MAX_BACKOFF ? MAX_BACKOFF : backoff * 2))
done
