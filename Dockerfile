FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go web.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /cfscan .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai
COPY --from=build /cfscan /usr/local/bin/cfscan
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["cfscan", "serve", "-addr", ":8080", "-data", "/data"]
