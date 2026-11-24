FROM alpine:3.21
ARG BINARY
COPY bin/${BINARY} /usr/local/bin/app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/app"]
