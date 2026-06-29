FROM golang:1.25-alpine AS builder

WORKDIR /src

RUN apk --no-cache add build-base

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags "-w -s" -o /out/zpk-market .

FROM alpine:3.22

RUN apk --no-cache add ca-certificates tzdata && \
    ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

ENV TZ=Asia/Shanghai

WORKDIR /home

COPY --from=builder /out/zpk-market /home/zpk-market
COPY config.yaml /home/config.yaml

EXPOSE 8000

CMD ["/home/zpk-market", "server:start", "-f", "/home/config.yaml"]
