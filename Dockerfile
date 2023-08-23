FROM cgr.dev/chainguard/go:latest as build-env

ENV CGO_ENABLED=0
WORKDIR /app

COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .

#RUN ./do.sh tests
RUN ./do.sh build

FROM cgr.dev/chainguard/wolfi-base
ENV PORT=9000
ENV USE_TLS=false
COPY --from=build-env /app/.bin/proxy /proxy
USER 65534
EXPOSE 9000
CMD ["/bin/sh", "-c", "./proxy registry serve"]
