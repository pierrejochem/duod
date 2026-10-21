# duod as a container: talks to the host Docker daemon via the socket.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /duod ./cmd/duod

FROM alpine:3.22
RUN apk add --no-cache git openssh-client docker-cli docker-cli-compose ca-certificates \
 && git config --system --add safe.directory '*'   # repos mounted from the host have another owner
COPY --from=build /duod /usr/local/bin/duod
ENTRYPOINT ["duod"]
CMD ["run", "-config", "/etc/duod/duod.yaml", "-log-format", "json"]
