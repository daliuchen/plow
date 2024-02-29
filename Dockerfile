FROM golang:1.18 as builder
WORKDIR /app
COPY . .
RUN go mod tidy \
    && CGO_ENABLED=0 GOOS=linux  go build -o plow github.com/six-ddc/plow

FROM alpine
EXPOSE 18888
COPY --from=builder /app/plow /usr/bin/plow

ENTRYPOINT ["/usr/bin/plow"]
