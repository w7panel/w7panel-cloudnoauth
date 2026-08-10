FROM golang:1.25-alpine AS builder

WORKDIR /src

RUN apk --no-cache add build-base

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags "-w -s" -o /out/w7panel-app-proxy .

FROM alpine:3.22

RUN apk --no-cache add ca-certificates iptables tzdata && \
    addgroup -S -g 1337 w7proxy && \
    adduser -S -D -H -u 1337 -G w7proxy w7proxy && \
    ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

ENV TZ=Asia/Shanghai

WORKDIR /home

COPY --from=builder /out/w7panel-app-proxy /home/w7panel-app-proxy
COPY config.yaml /home/config.yaml
COPY scripts/iptables-setup.sh /usr/local/bin/iptables-setup
RUN chmod 0755 /usr/local/bin/iptables-setup

EXPOSE 15080 15081 15443

CMD ["/home/w7panel-app-proxy", "server:start", "-f", "/home/config.yaml"]
