FROM golang AS gobuilder

WORKDIR /app

COPY . .

RUN go env -w GOPROXY=https://goproxy.cn,https://goproxy.io,direct && \
    go build -tags netgo -ldflags "-linkmode 'external' -extldflags '-static' -w -s" -o kftpd main/main.go

FROM scratch
COPY --from=gobuilder /app/kftpd .
COPY kftpd.yaml .
EXPOSE 21
EXPOSE 21000-21100
ENTRYPOINT ["/kftpd"]