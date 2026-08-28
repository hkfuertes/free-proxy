FROM golang:1.24-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod main.go config.go ./
COPY provider ./provider
RUN CGO_ENABLED=0 GOOS=linux go build -tags netgo -trimpath -ldflags='-s -w' -o /out/free-proxy .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/free-proxy /free-proxy
EXPOSE 8080
ENTRYPOINT ["/free-proxy"]
