# Build:  docker build -t sdk-lru-policy:1.0.0 .
# The platform runs this image as a non-root user with a read-only root
# filesystem, all capabilities dropped and no privileges, so it must not write
# outside /tmp. It listens on $PORT (8080 unless plugin.yaml says otherwise).
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/plugin .

FROM alpine:3.20
COPY --from=build /out/plugin /usr/local/bin/plugin
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["plugin"]
