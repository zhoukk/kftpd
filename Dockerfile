FROM golang AS gobuilder

WORKDIR /app

COPY kftpd.go .
COPY go.mod .
COPY go.sum .

RUN go build -tags netgo -ldflags "-linkmode 'external' -extldflags '-static' -w -s" -o kftpd kftpd.go

FROM scratch
COPY --from=gobuilder /app/kftpd .
COPY kftpd.yaml .
EXPOSE 21
EXPOSE 21000-21100
ENTRYPOINT ["/kftpd"]