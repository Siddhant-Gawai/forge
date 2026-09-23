FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
COPY migrations ./migrations
COPY certificates ./certificates
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forge .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 forge && mkdir /data && chown forge:forge /data
USER forge
WORKDIR /app
COPY --from=build /forge /app/forge
ENV FORGE_ADDR=0.0.0.0:8080 FORGE_DATA=/data/state.json
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/app/forge"]
