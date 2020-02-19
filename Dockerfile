FROM golang:1.13-alpine as builder
COPY rclone /go/src/github.com/storj.io/rclone
WORKDIR /go/src/github.com/storj.io/rclone
ENV CGO_ENABLED=0
RUN go install --ldflags '-s -w -extldflags "-static"'
COPY . /go/src/github.com/brimstone/docker-volume-rclone
WORKDIR /go/src/github.com/brimstone/docker-volume-rclone
RUN go install --ldflags '-s -w -extldflags "-static"'

FROM alpine
RUN apk -U add fuse
RUN mkdir -p /run/docker/plugins /mnt/state /mnt/volumes
COPY --from=builder /go/bin/rclone .
COPY --from=builder /go/bin/docker-volume-rclone .

CMD ["docker-volume-rclone"]
