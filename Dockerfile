FROM golang:1.25-alpine AS builder

WORKDIR /src
RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /watchducker .

FROM alpine:latest

WORKDIR /app

RUN apk add --no-cache tzdata ca-certificates

ENV TZ=UTC

COPY --from=builder /watchducker /app/watchducker
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY push.yaml.example /app/

RUN chmod +x /app/watchducker /usr/local/bin/entrypoint.sh && \
    ln -s /app/watchducker /usr/local/bin/watchducker && \
    mkdir -p /data

VOLUME /data
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["watchducker", "--all"]
