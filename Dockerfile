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
ENV PORT=9001
COPY --from=build-env /root/proxy .
EXPOSE 9001
CMD ["/bin/sh", "-c", "./proxy registry serve"]
