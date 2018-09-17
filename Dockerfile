FROM golang:1.11-alpine as builder
ARG SRC_DIR=/go/src/github.com/charithe/instrumentation-example
ADD . $SRC_DIR
WORKDIR $SRC_DIR
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-w -s' .

FROM gcr.io/distroless/base
COPY --from=builder /go/src/github.com/charithe/instrumentation-example/instrumentation-example /instrumentation-example
ENTRYPOINT ["/instrumentation-example"]
