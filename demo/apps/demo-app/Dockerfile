FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
ARG VERSION=dev
ARG ACCENT=#1D8CF8
ARG HEALTHY=true
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X 'main.accent=${ACCENT}' -X main.healthy=${HEALTHY}" \
    -o /demo-app .

FROM scratch
COPY --from=build /demo-app /demo-app
EXPOSE 8080
USER 65534
ENTRYPOINT ["/demo-app"]
