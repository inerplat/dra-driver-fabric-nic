FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /vfio-nic-plugin ./cmd/vfio-nic-plugin

FROM gcr.io/distroless/static-debian12
COPY --from=build /vfio-nic-plugin /vfio-nic-plugin
ENTRYPOINT ["/vfio-nic-plugin"]
