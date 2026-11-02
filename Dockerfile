FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-X ros-ddns/internal/version.Version=${VERSION} -X ros-ddns/internal/version.Commit=${COMMIT} -X ros-ddns/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/ros-ddns ./cmd/ros-ddns

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app \
    && mkdir /data \
    && chown 10001:10001 /data \
    && chmod 0700 /data
COPY --from=build /out/ros-ddns /usr/local/bin/ros-ddns
WORKDIR /data
USER 10001
EXPOSE 8080
ENTRYPOINT ["ros-ddns"]
# Pin the lock so a WebUI state.path hot reload cannot move it.
CMD ["-config", "/data/config.yaml", "-lock-file", "/data/state.json.lock"]
