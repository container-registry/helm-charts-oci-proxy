FROM golang:1.20-alpine as build-env
ENV CGO_ENABLED=0
WORKDIR /root

COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .

RUN ./do.sh tests
RUN ./do.sh build

FROM alpine
RUN apk add ca-certificates
WORKDIR /root
ENV PORT=9000
ENV USE_TLS=false
COPY --from=build-env /root/.bin/proxy .
COPY --from=build-env /root/certs ./certs
EXPOSE 9000
CMD ["/bin/sh", "-c", "./proxy registry serve"]
