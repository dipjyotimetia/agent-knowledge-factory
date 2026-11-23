FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/factoryd ./cmd/factoryd \
    && CGO_ENABLED=0 go build -trimpath -o /out/kfactory ./cmd/kfactory \
    && CGO_ENABLED=0 go build -trimpath -o /out/factory-worker ./cmd/factory-worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/factoryd /factoryd
COPY --from=build /out/kfactory /kfactory
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/factoryd"]
