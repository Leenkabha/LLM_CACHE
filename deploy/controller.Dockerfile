# Internal plugin controller. It builds/pulls/scans/starts plugin containers, so
# it is the ONE service that needs container-runtime access -- and it must reach
# the runtime only through the restricted docker-socket-proxy (DOCKER_HOST), never
# through a mounted /var/run/docker.sock. Do not expose it publicly.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/plugin-controller ./cmd/plugin-controller

FROM alpine:3.20
# docker-cli: talks to the socket proxy. git: exact-revision checkout of plugin repositories.
RUN apk add --no-cache docker-cli git ca-certificates \
 && adduser -D -u 10002 controller
COPY --from=build /out/plugin-controller /usr/local/bin/plugin-controller
# Sources of the platform's own services: Python plugins are built INTO these.
COPY embedding_service/app /opt/runner/embedding_service/app
COPY embedding_service/requirements-core.txt /opt/runner/embedding_service/requirements-core.txt
COPY vector_store_service/app /opt/runner/vector_store_service/app
COPY vector_store_service/requirements.txt /opt/runner/vector_store_service/requirements.txt
ENV PLUGIN_RUNNER_DIR=/opt/runner PLUGIN_CONTROLLER_ADDR=:8090
USER controller
EXPOSE 8090
ENTRYPOINT ["plugin-controller"]
