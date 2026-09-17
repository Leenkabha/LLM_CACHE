FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY examples/llm-server ./examples/llm-server
RUN CGO_ENABLED=0 go build -o /out/example-llm ./examples/llm-server

FROM alpine:3.20
COPY --from=build /out/example-llm /usr/local/bin/example-llm
EXPOSE 8090
ENTRYPOINT ["example-llm"]
