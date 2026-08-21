# build
FROM golang:1.22-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN go build -trimpath -ldflags="-s -w" -o /modem-exporter .

# runtime
FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=build /modem-exporter /modem-exporter
EXPOSE 9210
ENTRYPOINT ["/modem-exporter"]
