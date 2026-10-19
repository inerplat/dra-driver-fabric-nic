FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /fabric-nic-plugin ./cmd/fabric-nic-plugin

FROM gcr.io/distroless/static-debian12
COPY --from=build /fabric-nic-plugin /fabric-nic-plugin
ENTRYPOINT ["/fabric-nic-plugin"]
